package paygate402

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fileStore(t *testing.T, path string) *FileStore {
	t.Helper()
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestAFileStoreRefusesAReplayUntilTheWindowCloses(t *testing.T) {
	store := fileStore(t, filepath.Join(t.TempDir(), "seen"))
	clock := time.Unix(1800000000, 0)
	store.now = func() time.Time { return clock }

	if store.SeenBefore("key", time.Minute) {
		t.Fatal("a fresh key was reported as seen")
	}
	if !store.SeenBefore("key", time.Minute) {
		t.Fatal("a repeated key was not caught")
	}
	clock = clock.Add(2 * time.Minute)
	if store.SeenBefore("key", time.Minute) {
		t.Error("the key was still remembered after its window closed")
	}
	if err := store.Err(); err != nil {
		t.Errorf("the store reported a problem with its file: %v", err)
	}
}

// The reason the file exists: a process that comes back must not hand a
// captured header a second chance.
func TestAFileStoreRemembersThroughARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")

	first := fileStore(t, path)
	if first.SeenBefore("key", time.Hour) {
		t.Fatal("a fresh key was reported as seen")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	if !fileStore(t, path).SeenBefore("key", time.Hour) {
		t.Error("a payment spent before the restart was offered a second chance")
	}
}

func TestAFileStoreDropsEntriesWhoseWindowClosedWhileItWasDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")

	before := fileStore(t, path)
	before.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	before.SeenBefore("stale", time.Minute)
	before.SeenBefore("fresh", 3*time.Hour)
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	back := fileStore(t, path)
	if !back.SeenBefore("fresh", time.Hour) {
		t.Error("an entry still inside its window was forgotten across a restart")
	}
	if back.SeenBefore("stale", time.Minute) {
		t.Error("an entry whose window had closed was loaded anyway")
	}
}

func TestAFileStoreSkipsALineACrashCutInHalf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")

	store := fileStore(t, path)
	store.SeenBefore("key", time.Hour)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("18000000"); err != nil {
		t.Fatal(err)
	}
	file.Close()

	if !fileStore(t, path).SeenBefore("key", time.Hour) {
		t.Error("a half-written last line cost the store everything before it")
	}
}

func TestAFileStoreRewritesItselfWhenTheDeadOutnumberTheLiving(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	store := fileStore(t, path)
	clock := time.Unix(1800000000, 0)
	store.now = func() time.Time { return clock }

	spent := 4 * compactAfter
	for i := 0; i < spent; i++ {
		store.SeenBefore(fmt.Sprintf("key-%d", i), time.Minute)
	}
	if got := len(lines(t, path)); got != spent {
		t.Fatalf("file holds %d records, want one per payment", got)
	}

	clock = clock.Add(2 * time.Minute)
	store.SeenBefore("later", time.Minute)
	if got := len(lines(t, path)); got != 1 {
		t.Errorf("file holds %d records, want only the live one after a rewrite", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// The rewritten file is still a file the store can read.
	back := fileStore(t, path)
	back.now = func() time.Time { return clock }
	if !back.SeenBefore("later", time.Minute) {
		t.Error("the rewrite lost the entry it was keeping")
	}
}

func TestAGateCanKeepItsReplayStoreInAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	store := fileStore(t, path)
	g := &Gate{
		Accepts: []Requirements{terms()},
		Facilitator: &StaticFacilitator{
			Verification: Verification{Valid: true},
			Settlement:   Settlement{Success: true},
		},
		Seen: store,
	}
	captured := header(t, payment("1"))

	if first := request(t, g, captured); first.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200; body %s", first.Code, first.Body)
	}
	if second := request(t, g, captured); second.Code != http.StatusPaymentRequired {
		t.Fatalf("replayed request: status = %d, want 402", second.Code)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	g.Seen = fileStore(t, path)
	if third := request(t, g, captured); third.Code != http.StatusPaymentRequired {
		t.Errorf("restarted server: status = %d, want the spent payment still spent", third.Code)
	}
}

func TestAReplayWindowOutlivesTheQuoteItIsFor(t *testing.T) {
	if got := ReplayWindowFor(quote(), time.Unix(1799999940, 0), time.Minute); got != 2*time.Minute {
		t.Errorf("window = %s, want the minute left on the quote plus the safety minute", got)
	}
	// A quote that has already run out is still remembered for the margin: the
	// clock that says it ran out is not the clock that stamped it.
	if got := ReplayWindowFor(quote(), time.Unix(1800000600, 0), time.Minute); got != time.Minute {
		t.Errorf("window = %s, want the safety margin", got)
	}
	if got := ReplayWindowFor(quote(), time.Unix(1800000000, 0), 0); got != DefaultReplaySafety {
		t.Errorf("window = %s, want the default margin", got)
	}
}
