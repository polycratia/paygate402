package paygate402

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// settleWatcher reports the context it was settled under: whether it was still
// live, and what was bounding it.
type settleWatcher struct {
	StaticFacilitator
	live        bool
	hasDeadline bool
	left        time.Duration
}

func (f *settleWatcher) Settle(ctx context.Context, payment Payment, terms Requirements) (Settlement, error) {
	f.live = ctx.Err() == nil
	if deadline, ok := ctx.Deadline(); ok {
		f.hasDeadline, f.left = true, time.Until(deadline)
	}
	return f.StaticFacilitator.Settle(ctx, payment, terms)
}

func settleWatcherThatSucceeds() *settleWatcher {
	return &settleWatcher{StaticFacilitator: StaticFacilitator{
		Verification: Verification{Valid: true, Payer: "0xabc"},
		Settlement:   Settlement{Success: true, Transaction: "0xfeed"},
	}}
}

func settleGate(f Facilitator) *Gate {
	return &Gate{
		Accepts:     []Requirements{terms()},
		Facilitator: f,
		Seen:        NewMemoryStore(),
	}
}

// handlerThatLosesTheClient answers and the connection goes, which is the
// ordinary case rather than a failure: a client has what it asked for and
// stops waiting.
func handlerThatLosesTheClient(gone context.CancelFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"report":"quarterly numbers"}`))
		gone()
	})
}

func settleRequest(t *testing.T, g *Gate, ctx context.Context, next http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil).WithContext(ctx)
	r.Header.Set(PaymentHeader, header(t, payment("1")))
	w := httptest.NewRecorder()
	g.Handler(next).ServeHTTP(w, r)
	return w
}

// The work is done by the time settlement starts, so a client that hung up
// must not take the transfer with it: settlement runs on a context detached
// from the request's, bounded by a deadline instead of by the connection.
func TestSettlementOutlivesTheClientsConnection(t *testing.T) {
	var logged bytes.Buffer
	facilitator := settleWatcherThatSucceeds()
	g := settleGate(facilitator)
	g.SettleTimeout = time.Minute
	g.Logger = slog.New(slog.NewTextHandler(&logged, nil))

	ctx, gone := context.WithCancel(context.Background())
	defer gone()
	response := settleRequest(t, g, ctx, handlerThatLosesTheClient(gone))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Fatalf("calls = %q, want verify,settle", facilitator.Trail())
	}
	if !facilitator.live {
		t.Error("the facilitator was handed a cancelled context: the client hanging up aborted the transfer")
	}
	if !facilitator.hasDeadline {
		t.Fatal("the detached context carried no deadline, so a silent facilitator would hold the call forever")
	}
	if facilitator.left <= 0 || facilitator.left > time.Minute {
		t.Errorf("deadline in %s, want the configured minute", facilitator.left)
	}
	if response.Header().Get(ResponseHeader) == "" {
		t.Error("the settlement was not reported back to the client")
	}
	if !strings.Contains(logged.String(), "0xfeed") {
		t.Errorf("the transaction was not logged: %s", logged.String())
	}
}

func TestSettlementIsBoundedWithoutBeingConfigured(t *testing.T) {
	facilitator := settleWatcherThatSucceeds()
	g := settleGate(facilitator)

	ctx, gone := context.WithCancel(context.Background())
	defer gone()
	if response := settleRequest(t, g, ctx, handlerThatLosesTheClient(gone)); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}

	if !facilitator.hasDeadline {
		t.Fatal("an unconfigured gate settled with no deadline at all")
	}
	if facilitator.left <= DefaultSettleTimeout-time.Second || facilitator.left > DefaultSettleTimeout {
		t.Errorf("deadline in %s, want about the default %s", facilitator.left, DefaultSettleTimeout)
	}
}

// Verification is the other way round. Nothing has been delivered yet, so a
// client that is gone before the facilitator answers costs nobody anything,
// and the request's own context is the right one to carry that.
func TestVerificationStillRunsOnTheRequestsContext(t *testing.T) {
	var live bool
	g := settleGate(&contextFacilitator{live: &live})

	ctx, gone := context.WithCancel(context.Background())
	gone()
	settleRequest(t, g, ctx, paidHandler())

	if live {
		t.Error("a cancelled request was still put to the facilitator")
	}
}

// contextFacilitator reports whether Verify was asked under a live context.
type contextFacilitator struct {
	StaticFacilitator
	live *bool
}

func (f *contextFacilitator) Verify(ctx context.Context, payment Payment, terms Requirements) (Verification, error) {
	*f.live = ctx.Err() == nil
	return f.StaticFacilitator.Verify(ctx, payment, terms)
}
