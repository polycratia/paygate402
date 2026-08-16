package paygate402

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Gate wraps a handler so that it only runs for paid requests.
type Gate struct {
	// Accepts lists the terms this resource will take. At least one is required.
	Accepts []Requirements
	// Facilitator verifies and settles. Required.
	Facilitator Facilitator
	// Seen prevents replay. A memory store is used when this is nil, which is
	// correct for a single instance and wrong for several — see MemoryStore.
	Seen SeenStore
	// ReplayWindow is how long a used payment is remembered. Defaults to an hour.
	ReplayWindow time.Duration
	// Logger receives settlement failures, which are the events an operator
	// most needs to see: the work was done and the money did not arrive.
	Logger *slog.Logger
}

// Handler wraps next with the payment gate.
func (g *Gate) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := g.validate(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		payment, err := DecodePayment(r.Header.Get(PaymentHeader))
		if err != nil {
			g.challenge(w, reason(err))
			return
		}
		terms, err := Match(payment, g.Accepts)
		if err != nil {
			g.challenge(w, err.Error())
			return
		}

		store := g.Seen
		if store == nil {
			store = defaultStore
		}
		if store.SeenBefore(PaymentKey(payment), g.replayWindow()) {
			g.challenge(w, "this payment has already been used")
			return
		}

		verification, err := g.Facilitator.Verify(r.Context(), payment, terms)
		if err != nil {
			// The facilitator is unreachable. This is the server's problem, not
			// the client's: answering 402 would tell them to pay again.
			http.Error(w, "payment verification is unavailable", http.StatusBadGateway)
			return
		}
		if !verification.Valid {
			g.challenge(w, orDefault(verification.Reason, "payment was not accepted"))
			return
		}

		// The handler runs into a buffer. Settlement happens before anything
		// reaches the client, so a client never receives a paid response for a
		// payment that then failed to settle.
		recorder := &recorder{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(recorder, r.WithContext(withPayer(r.Context(), verification.Payer)))

		if recorder.status < 200 || recorder.status >= 300 {
			// Nothing was delivered, so nothing is charged.
			recorder.flush(w, "")
			return
		}

		settlement, err := g.Facilitator.Settle(r.Context(), payment, terms)
		switch {
		case err != nil:
			g.log("settlement call failed", "error", err)
			http.Error(w, "payment settlement is unavailable", http.StatusBadGateway)
			return
		case !settlement.Success:
			g.log("settlement declined", "reason", settlement.ErrorReason)
			g.challenge(w, orDefault(settlement.ErrorReason, "payment could not be settled"))
			return
		}

		encoded, err := EncodeSettlement(settlement)
		if err != nil {
			g.log("settlement could not be encoded", "error", err)
		}
		recorder.flush(w, encoded)
	})
}

var defaultStore = NewMemoryStore()

func (g *Gate) validate() error {
	if len(g.Accepts) == 0 {
		return errors.New("paygate402: no accepted terms configured")
	}
	for _, terms := range g.Accepts {
		if err := terms.Validate(); err != nil {
			return err
		}
	}
	if g.Facilitator == nil {
		return errors.New("paygate402: no facilitator configured")
	}
	return nil
}

func (g *Gate) replayWindow() time.Duration {
	if g.ReplayWindow > 0 {
		return g.ReplayWindow
	}
	return time.Hour
}

// challenge writes the 402 that tells a client what would be accepted.
func (g *Gate) challenge(w http.ResponseWriter, why string) {
	body, err := json.Marshal(Challenge{
		X402Version: Version,
		Error:       why,
		Accepts:     g.Accepts,
	})
	if err != nil {
		http.Error(w, "payment required", http.StatusPaymentRequired)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	w.Write(body)
}

func (g *Gate) log(message string, args ...any) {
	if g.Logger != nil {
		g.Logger.Error(message, args...)
	}
}

// recorder buffers a handler's response until settlement has been decided.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	r.wrote = true
	return r.body.Write(p)
}

func (r *recorder) flush(w http.ResponseWriter, settlement string) {
	for name, values := range r.header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	if settlement != "" {
		w.Header().Set(ResponseHeader, settlement)
	}
	w.WriteHeader(r.status)
	w.Write(r.body.Bytes())
}

func reason(err error) string {
	switch {
	case errors.Is(err, ErrNoPayment):
		return "payment required"
	default:
		return err.Error()
	}
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
