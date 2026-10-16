package paygate402

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handlerThatFails answers outside 2xx, the way a handler whose work did not
// come off does.
func handlerThatFails() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "the report could not be generated", http.StatusInternalServerError)
	})
}

func releaseGate(store SeenStore) *Gate {
	return &Gate{
		Accepts: []Requirements{terms()},
		Facilitator: &StaticFacilitator{
			Verification: Verification{Valid: true},
			Settlement:   Settlement{Success: true},
		},
		Seen: store,
	}
}

func releaseRequest(t *testing.T, g *Gate, next http.Handler, paymentHeader string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil)
	if paymentHeader != "" {
		r.Header.Set(PaymentHeader, paymentHeader)
	}
	w := httptest.NewRecorder()
	g.Handler(next).ServeHTTP(w, r)
	return w
}

// keepingStore is a SeenStore and nothing more: it cannot take a payment back.
type keepingStore struct{ inner *MemoryStore }

func (s *keepingStore) SeenBefore(key string, ttl time.Duration) bool {
	return s.inner.SeenBefore(key, ttl)
}

// A payment buys a response. A handler that answered 500 delivered nothing and
// settled nothing, so the payment is given back and the same signed payload
// goes through on the retry.
func TestAPaymentTheHandlerFailedOnIsGivenBack(t *testing.T) {
	g := releaseGate(NewMemoryStore())
	facilitator := g.Facilitator.(*StaticFacilitator)
	signed := header(t, payment("1"))

	failed := releaseRequest(t, g, handlerThatFails(), signed)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the handler's 500; body %s", failed.Code, failed.Body)
	}
	if facilitator.Trail() != "verify" {
		t.Errorf("calls = %q: nothing should settle for work that was not delivered", facilitator.Trail())
	}

	retry := releaseRequest(t, g, paidHandler(), signed)
	if retry.Code != http.StatusOK {
		t.Fatalf("retried request: status = %d, want 200; body %s", retry.Code, retry.Body)
	}
	if facilitator.Trail() != "verify,verify,settle" {
		t.Errorf("calls = %q, want the retry verified and settled", facilitator.Trail())
	}
}

// Release is optional, and a store without it keeps the payment spent. That is
// the safe direction: the gate must never be the thing that charges twice.
func TestAStoreWithoutReleaseLeavesThePaymentSpent(t *testing.T) {
	g := releaseGate(&keepingStore{inner: NewMemoryStore()})
	signed := header(t, payment("1"))

	if failed := releaseRequest(t, g, handlerThatFails(), signed); failed.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the handler's 500", failed.Code)
	}
	retry := releaseRequest(t, g, paidHandler(), signed)
	if retry.Code != http.StatusPaymentRequired {
		t.Fatalf("retried request: status = %d, want 402; body %s", retry.Code, retry.Body)
	}
	if !strings.Contains(retry.Body.String(), "already been used") {
		t.Errorf("the client was not told why: %s", retry.Body)
	}
}

// A payment settlement failed on stays spent: the attempt may have reached the
// chain, and the ambiguous case must not risk a second charge.
func TestAPaymentSettlementFailedOnStaysSpent(t *testing.T) {
	g := releaseGate(NewMemoryStore())
	g.Facilitator = &StaticFacilitator{
		Verification: Verification{Valid: true},
		Settlement:   Settlement{Success: false, ErrorReason: "the transfer was rejected"},
	}
	signed := header(t, payment("1"))

	if declined := releaseRequest(t, g, paidHandler(), signed); declined.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body %s", declined.Code, declined.Body)
	}
	retry := releaseRequest(t, g, paidHandler(), signed)
	if !strings.Contains(retry.Body.String(), "already been used") {
		t.Errorf("a payment settlement was attempted for was offered again: %s", retry.Body)
	}
}

func TestAFileStoreWritesALineWhenItTakesAPaymentBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")
	store := fileStore(t, path)

	if store.SeenBefore("key", time.Hour) {
		t.Fatal("a fresh key was reported as seen")
	}
	store.Release("key")
	if got := len(lines(t, path)); got != 2 {
		t.Errorf("file holds %d records, want the payment and the line taking it back", got)
	}
	if store.SeenBefore("key", time.Hour) {
		t.Error("a released payment was still refused")
	}
	if err := store.Err(); err != nil {
		t.Errorf("the store reported a problem with its file: %v", err)
	}
}

// The release has to survive a restart too, or a payment given back comes back
// spent the moment the process does.
func TestAFileStoreDoesNotRememberAPaymentItGaveBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seen")

	first := fileStore(t, path)
	first.SeenBefore("key", time.Hour)
	first.Release("key")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	if fileStore(t, path).SeenBefore("key", time.Hour) {
		t.Error("a payment given back before the restart came back spent")
	}
}
