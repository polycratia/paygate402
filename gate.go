package paygate402

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// Gate wraps a handler so that it only runs for paid requests.
type Gate struct {
	// Accepts lists the terms this resource will take. Either this or Prices is
	// required, and setting both is a configuration error.
	Accepts []Requirements
	// Prices declares a price per route pattern, for a gate in front of more
	// than one resource. A route the table does not price is served free.
	Prices *Prices
	// Free, when set, lets each client through a few times before any payment
	// is asked for.
	Free *Allowance
	// Facilitator verifies and settles. Required.
	Facilitator Facilitator
	// Quotes, when set, signs the offer that goes out with every 402 and checks
	// the one that comes back. Without it the gate takes any payment matching
	// the terms; with it a payment must answer an offer this server made and
	// that has not run out.
	Quotes *QuoteSigner
	// Seen prevents replay. A memory store is used when this is nil, which is
	// correct for a single instance and wrong for several — see MemoryStore.
	Seen SeenStore
	// ReplayWindow is how long a used payment is remembered. Defaults to an hour,
	// and is superseded by the offer's own lifetime when Quotes is set.
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

		accepts, err := g.accepts(r)
		switch {
		case errors.Is(err, ErrNoPrice):
			// The table is the whole statement of what is sold here, so a route
			// missing from it is free rather than closed.
			next.ServeHTTP(w, r)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// The free tier is spent before the payment is even read: a client that
		// does not have to pay yet is not asked to, and a payment it sent anyway
		// is left unspent rather than taken.
		if client, left, granted := g.Free.take(r); granted {
			g.serveFree(w, r, next, client, left)
			return
		}

		payment, err := DecodePayment(r.Header.Get(PaymentHeader))
		if err != nil {
			g.challenge(w, accepts, reason(err))
			return
		}
		terms, err := Match(payment, accepts)
		if err != nil {
			g.challenge(w, accepts, err.Error())
			return
		}

		// The offer comes before the facilitator: a payment answering no offer
		// this server made, or one that has run out, is not a question worth
		// putting to a facilitator.
		window := g.replayWindow()
		if g.Quotes != nil {
			offer, err := g.presented(r, terms)
			if err != nil {
				g.challenge(w, accepts, reason(err))
				return
			}
			// A payment only has to be remembered while the offer behind it could
			// still be presented, and the offer says when that stops.
			window = ReplayWindowFor(offer, g.Quotes.now(), 0)
		}

		verification, err := g.Facilitator.Verify(r.Context(), payment, terms)
		if err != nil {
			// The facilitator is unreachable. This is the server's problem, not
			// the client's: answering 402 would tell them to pay again.
			http.Error(w, "payment verification is unavailable", http.StatusBadGateway)
			return
		}
		if !verification.Valid {
			g.challenge(w, accepts, orDefault(verification.Reason, "payment was not accepted"))
			return
		}

		// The replay key is recorded only after verification passes. Recording
		// at first sight would burn a payment the facilitator rejected — the
		// client fixes the allowance and legitimately resends the same signed
		// payload. Once recorded the payment stays spent even if settlement
		// then fails: a settlement attempt may have reached the chain, and the
		// ambiguous case must never risk a double charge.
		store := g.Seen
		if store == nil {
			store = defaultStore
		}
		if store.SeenBefore(PaymentKey(payment), window) {
			g.challenge(w, accepts, "this payment has already been used")
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
			g.challenge(w, accepts, orDefault(settlement.ErrorReason, "payment could not be settled"))
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
	switch {
	case g.Prices != nil && len(g.Accepts) > 0:
		// Two ways to say the same thing, settled by asking rather than by a
		// precedence rule nobody would remember.
		return errors.New("paygate402: set either Accepts or Prices, not both")
	case g.Prices == nil && len(g.Accepts) == 0:
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
	if g.Quotes != nil && len(g.Quotes.Key) == 0 {
		return ErrNoQuoteKey
	}
	return nil
}

// accepts returns the terms this request would be paid against: the fixed ones,
// or the ones the pricing table gives this route.
func (g *Gate) accepts(r *http.Request) ([]Requirements, error) {
	if g.Prices == nil {
		return g.Accepts, nil
	}
	terms, err := g.Prices.Price(r)
	if err != nil {
		return nil, err
	}
	return []Requirements{terms}, nil
}

// serveFree runs the handler on the house. A free request that produced nothing
// is given back, for the same reason work that failed is never charged for.
func (g *Gate) serveFree(w http.ResponseWriter, r *http.Request, next http.Handler, client string, left int) {
	recorder := &recorder{header: http.Header{}, status: http.StatusOK}
	next.ServeHTTP(recorder, r)
	if recorder.status < 200 || recorder.status >= 300 {
		g.Free.give(client)
		recorder.flush(w, "")
		return
	}
	recorder.header.Set(AllowanceHeader, strconv.Itoa(left))
	recorder.flush(w, "")
}

// presented reads the offer the retried request is answering: this server's
// signature, still inside its window, and for the terms being paid for.
func (g *Gate) presented(r *http.Request, terms Requirements) (Quote, error) {
	offer, err := DecodeQuote(r.Header.Get(QuoteHeader))
	if err != nil {
		return Quote{}, err
	}
	if err := g.Quotes.Verify(offer); err != nil {
		return Quote{}, err
	}
	return offer, offer.Covers(terms)
}

// offers signs one quote per set of accepted terms, in the order they are
// accepted, so a client can tell which offer belongs to which terms.
func (g *Gate) offers(accepts []Requirements) ([]string, error) {
	encoded := make([]string, 0, len(accepts))
	for _, terms := range accepts {
		offer, err := g.Quotes.Issue(terms)
		if err != nil {
			return nil, err
		}
		header, err := EncodeQuote(offer)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, header)
	}
	return encoded, nil
}

func (g *Gate) replayWindow() time.Duration {
	if g.ReplayWindow > 0 {
		return g.ReplayWindow
	}
	return DefaultReplayWindow
}

// challenge writes the 402 that tells a client what would be accepted, and,
// when this gate signs quotes, the offer behind each set of terms.
func (g *Gate) challenge(w http.ResponseWriter, accepts []Requirements, why string) {
	if g.Quotes != nil {
		offers, err := g.offers(accepts)
		if err != nil {
			// A 402 without the offer it promises asks a client to pay against
			// terms this server will then refuse, so it is not sent at all.
			g.log("quote could not be issued", "error", err)
			http.Error(w, "payment quote is unavailable", http.StatusInternalServerError)
			return
		}
		for _, offer := range offers {
			w.Header().Add(QuoteHeader, offer)
		}
	}
	body, err := json.Marshal(Challenge{
		X402Version: Version,
		Error:       why,
		Accepts:     accepts,
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
	case errors.Is(err, ErrNoQuote):
		return "this request did not carry the quote it is answering"
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
