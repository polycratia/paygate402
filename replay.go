package paygate402

import (
	"crypto/sha256"
	"encoding/hex"
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
		ttl = time.Hour
	}
	m.seen[key] = now.Add(ttl)
	return false
}
