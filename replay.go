package paygate402

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SeenStore remembers payments that have already been used, so that a captured
// X-PAYMENT header cannot be replayed against the same server.
//
// The key is a digest of the whole payment payload rather than a nonce field:
// every x402 scheme carries its own payload shape, and reaching into one to
// find "the nonce" would break the moment a new scheme appears.
type SeenStore interface {
	// SeenBefore records a key and reports whether it was already there.
	SeenBefore(key string, ttl time.Duration) bool
}

// DefaultReplayWindow is how long a used payment is remembered when nothing
// asks for a particular window.
const DefaultReplayWindow = time.Hour

// DefaultReplaySafety is the margin ReplayWindowFor adds to a quote's expiry.
const DefaultReplaySafety = 5 * time.Minute

// PaymentKey is the replay key for a payment.
func PaymentKey(payment Payment) string {
	sum := sha256.New()
	sum.Write([]byte(payment.Scheme))
	sum.Write([]byte{0})
	sum.Write([]byte(payment.Network))
	sum.Write([]byte{0})
	sum.Write(payment.Payload)
	return hex.EncodeToString(sum.Sum(nil))
}

// ReplayWindowFor returns how long a payment made against this quote has to be
// remembered: until the offer stops standing, plus a safety margin.
//
// Past its expiry the quote cannot be presented again anyway, so remembering
// the payment for longer buys nothing and only makes the store grow. The margin
// is there because the clock that decides a quote expired is not the clock that
// stamped it, and the gap between them must not be a window to replay in. A
// quote that has already expired still earns the margin, for the same reason.
func ReplayWindowFor(quote Quote, now time.Time, safety time.Duration) time.Duration {
	if safety <= 0 {
		safety = DefaultReplaySafety
	}
	window := quote.Expiry.Sub(now) + safety
	if window < safety {
		return safety
	}
	return window
}

// MemoryStore keeps seen payments in this process.
//
// It is honest about its limits: a second instance of the server does not share
// it, so a payment could be replayed once per instance. Deployments with more
// than one instance need a shared store behind this interface — which is why it
// is an interface.
type MemoryStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
	now  func() time.Time
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{seen: map[string]time.Time{}, now: time.Now}
}

// SeenBefore records the key and reports whether it had been seen.
func (m *MemoryStore) SeenBefore(key string, ttl time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	// Expiry is swept on write. A payment gate sees writes on exactly the
	// requests that matter, so no background goroutine has to exist.
	for existing, expires := range m.seen {
		if now.After(expires) {
			delete(m.seen, existing)
		}
	}
	if expires, ok := m.seen[key]; ok && now.Before(expires) {
		return true
	}
	if ttl <= 0 {
		ttl = DefaultReplayWindow
	}
	m.seen[key] = now.Add(ttl)
	return false
}

// compactAfter is the number of records a file is allowed to carry before a
// rewrite is worth its cost. Below it the file is small enough that the dead
// entries in it do not matter.
const compactAfter = 64

// FileStore keeps seen payments in a file, so that a restart does not hand a
// captured X-PAYMENT header a second chance.
//
// The file is an append-only log of "expiry key" lines, one per payment, and is
// rewritten once the expired entries outnumber the live ones. A write reaches
// the operating system before the request it belongs to is answered, so a
// process that restarts sees it; a machine that loses power may lose the last
// few entries, which is the limit of what one file can promise.
//
// The limit it shares with MemoryStore is the important one: one file is one
// machine. Several machines still need a shared store behind SeenStore.
//
// A key is written verbatim after its timestamp, so it must not contain a
// newline. PaymentKey returns hex.
type FileStore struct {
	mu      sync.Mutex
	path    string
	seen    map[string]time.Time
	file    *os.File
	records int
	err     error
	now     func() time.Time
}

// NewFileStore opens, or creates, the store at path and loads the payments it
// still remembers. Entries whose window has already closed are dropped on the
// way in, so a file left behind by a stopped server does not grow forever.
func NewFileStore(path string) (*FileStore, error) {
	store := &FileStore{path: path, seen: map[string]time.Time{}, now: time.Now}
	if err := store.load(); err != nil {
		return nil, err
	}
	if err := store.open(); err != nil {
		return nil, err
	}
	return store, nil
}

// SeenBefore records the key and reports whether it had been seen.
func (f *FileStore) SeenBefore(key string, ttl time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	now := f.now()
	f.sweep(now)
	if expires, ok := f.seen[key]; ok && now.Before(expires) {
		return true
	}
	if ttl <= 0 {
		ttl = DefaultReplayWindow
	}
	expires := now.Add(ttl)
	f.seen[key] = expires
	f.append(key, expires)
	return false
}

// Close releases the file. The store keeps answering from memory afterwards, so
// closing it during a shutdown cannot turn a replay into a served request — but
// nothing more is written, and what is not written is not there on the next
// start.
func (f *FileStore) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeFile()
}

// Err returns the first problem the store had with its file, if it had one.
//
// A store that cannot write goes on refusing replays from memory: a full disk
// must not open the gate. It must not be silent either, because the protection
// has quietly become the one MemoryStore gives, and only a restart shows it.
func (f *FileStore) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *FileStore) load() error {
	file, err := os.Open(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("replay store %s: %w", f.path, err)
	}
	defer file.Close()

	now := f.now()
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		f.records++
		key, expires, ok := parseRecord(scan.Text())
		// A line that does not parse is a line a crash cut in half. Everything
		// written before it is still good, so it is skipped rather than fatal.
		if !ok || !now.Before(expires) {
			continue
		}
		f.seen[key] = expires
	}
	if err := scan.Err(); err != nil {
		return fmt.Errorf("replay store %s: %w", f.path, err)
	}
	return nil
}

func parseRecord(line string) (string, time.Time, bool) {
	nanos, key, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found || key == "" {
		return "", time.Time{}, false
	}
	stamp, err := strconv.ParseInt(nanos, 10, 64)
	if err != nil {
		return "", time.Time{}, false
	}
	return key, time.Unix(0, stamp), true
}

func (f *FileStore) sweep(now time.Time) {
	for key, expires := range f.seen {
		if now.After(expires) {
			delete(f.seen, key)
		}
	}
	// The file is only appended to, so it holds every entry ever written,
	// expired ones included. Rewriting it once the dead outnumber the living
	// keeps the rewrite rare and the file within a constant of what is live.
	if f.records > compactAfter && f.records > 2*len(f.seen) {
		f.keep(f.rewrite())
	}
}

func (f *FileStore) append(key string, expires time.Time) {
	if f.file == nil {
		return
	}
	if _, err := f.file.WriteString(strconv.FormatInt(expires.UnixNano(), 10) + " " + key + "\n"); err != nil {
		f.keep(fmt.Errorf("replay store %s: %w", f.path, err))
		return
	}
	f.records++
}

func (f *FileStore) rewrite() error {
	temp, err := os.CreateTemp(filepath.Dir(f.path), filepath.Base(f.path)+".rewrite-*")
	if err != nil {
		return fmt.Errorf("replay store %s: %w", f.path, err)
	}
	defer os.Remove(temp.Name())

	writer := bufio.NewWriter(temp)
	for key, expires := range f.seen {
		fmt.Fprintf(writer, "%d %s\n", expires.UnixNano(), key)
	}
	err = writer.Flush()
	if err == nil {
		// The replacement is on the disk before it takes the place of what it
		// replaces: a rename onto an unwritten file forgets every payment at
		// once, which is the one failure this store exists to prevent.
		err = temp.Sync()
	}
	temp.Close()
	if err != nil {
		return fmt.Errorf("replay store %s: %w", f.path, err)
	}

	f.closeFile()
	renameErr := os.Rename(temp.Name(), f.path)
	if renameErr == nil {
		f.records = len(f.seen)
	}
	// Whether or not the rename landed, the appender is reopened: the store has
	// to go on recording against whichever file is now at the path.
	if err := f.open(); err != nil {
		return err
	}
	if renameErr != nil {
		return fmt.Errorf("replay store %s: %w", f.path, renameErr)
	}
	return nil
}

func (f *FileStore) open() error {
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("replay store %s: %w", f.path, err)
	}
	f.file = file
	return nil
}

func (f *FileStore) closeFile() error {
	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

// keep remembers the first thing that went wrong, for Err to report.
func (f *FileStore) keep(err error) {
	if err != nil && f.err == nil {
		f.err = err
	}
}
