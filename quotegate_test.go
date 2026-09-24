package paygate402

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func movingSigner(clock *time.Time) *QuoteSigner {
	return &QuoteSigner{
		Key: quoteKey(),
		TTL: time.Minute,
		Now: func() time.Time { return *clock },
	}
}

func quotedGate(f Facilitator, signer *QuoteSigner) *Gate {
	return &Gate{
		Accepts:     []Requirements{terms()},
		Facilitator: f,
		Seen:        NewMemoryStore(),
		Quotes:      signer,
	}
}

func quotedRequest(t *testing.T, g *Gate, paymentHeader, quoteHeader string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil)
	if paymentHeader != "" {
		r.Header.Set(PaymentHeader, paymentHeader)
	}
	if quoteHeader != "" {
		r.Header.Set(QuoteHeader, quoteHeader)
	}
	w := httptest.NewRecorder()
	g.Handler(paidHandler()).ServeHTTP(w, r)
	return w
}

// offered takes the 402 the way a client does and returns the offer it carried.
func offered(t *testing.T, g *Gate) string {
	t.Helper()
	challenge := quotedRequest(t, g, "", "")
	if challenge.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body %s", challenge.Code, challenge.Body)
	}
	offers := challenge.Header().Values(QuoteHeader)
	if len(offers) != len(g.Accepts) {
		t.Fatalf("the challenge carried %d offers, want one per accepted term", len(offers))
	}
	return offers[0]
}

func TestTheChallengeCarriesAnOfferThisServerSigned(t *testing.T) {
	signer := quoteSigner(1800000000)
	g := quotedGate(&StaticFacilitator{}, signer)

	offer, err := DecodeQuote(offered(t, g))
	if err != nil {
		t.Fatalf("the offer on the challenge could not be read: %v", err)
	}
	if err := signer.Verify(offer); err != nil {
		t.Errorf("the offer this server sent does not verify here: %v", err)
	}
	if err := offer.Covers(terms()); err != nil {
		t.Errorf("the offer is not for the terms it was sent with: %v", err)
	}
}

// The round trip the middleware exists for: 402 with an offer, the same request
// again carrying the offer and the payment, and the handler runs.
func TestAPaymentAnsweringTheOfferIsServed(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: true, Payer: "0xabc"},
		Settlement:   Settlement{Success: true, Transaction: "0xdeadbeef"},
	}
	g := quotedGate(facilitator, quoteSigner(1800000000))

	response := quotedRequest(t, g, header(t, payment("1")), offered(t, g))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want verify,settle", facilitator.Trail())
	}
	if !strings.Contains(response.Body.String(), `"payer":"0xabc"`) {
		t.Errorf("the handler did not run behind the gate: %s", response.Body)
	}
}

// Every refusal below happens before the facilitator is asked anything: a
// payment against an offer this server is not standing behind is not a question
// worth putting to a facilitator.
func TestAnOfferThisServerIsNotStandingBehindIsRefused(t *testing.T) {
	clock := time.Unix(1800000000, 0)
	signer := movingSigner(&clock)
	stranger := &QuoteSigner{Key: []byte("someone else's secret"), Now: signer.Now}

	cheaper, err := signer.Sign(Quote{
		Amount:   "1",
		Asset:    terms().Asset,
		Network:  terms().Network,
		Resource: terms().Resource,
		PayTo:    terms().PayTo,
		Expiry:   clock.Add(time.Minute).UTC(),
		Nonce:    "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	cheaperHeader, err := EncodeQuote(cheaper)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		offer func(t *testing.T, g *Gate) string
		says  string
		later time.Duration
	}{
		"no offer at all": {
			offer: func(*testing.T, *Gate) string { return "" },
			says:  "did not carry the quote",
		},
		"an offer that is not readable": {
			offer: func(*testing.T, *Gate) string { return "!!!!" },
			says:  "could not be decoded",
		},
		"an offer signed by someone else": {
			offer: func(t *testing.T, _ *Gate) string {
				forged, err := stranger.Issue(terms())
				if err != nil {
					t.Fatal(err)
				}
				encoded, err := EncodeQuote(forged)
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			},
			says: "signature does not verify",
		},
		"an offer that has run out": {
			offer: offered,
			says:  "has expired",
			later: 2 * time.Minute,
		},
		"an offer for other terms": {
			offer: func(*testing.T, *Gate) string { return cheaperHeader },
			says:  "not an offer for these terms",
		},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			clock = time.Unix(1800000000, 0)
			facilitator := &StaticFacilitator{
				Verification: Verification{Valid: true},
				Settlement:   Settlement{Success: true},
			}
			g := quotedGate(facilitator, signer)
			offer := want.offer(t, g)
			clock = clock.Add(want.later)

			response := quotedRequest(t, g, header(t, payment("1")), offer)
			if response.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402; body %s", response.Code, response.Body)
			}
			if !strings.Contains(response.Body.String(), want.says) {
				t.Errorf("the client was not told why: %s", response.Body)
			}
			if facilitator.Trail() != "" {
				t.Errorf("calls = %q: the facilitator was asked about an offer this server is not standing behind", facilitator.Trail())
			}
			if response.Header().Get(QuoteHeader) == "" {
				t.Error("the refusal did not carry a fresh offer to pay against")
			}
		})
	}
}

// windowStore records the window it was asked to remember a payment for.
type windowStore struct {
	inner  *MemoryStore
	window time.Duration
}

func (s *windowStore) SeenBefore(key string, ttl time.Duration) bool {
	s.window = ttl
	return s.inner.SeenBefore(key, ttl)
}

// A payment is remembered until the offer behind it stops standing, plus the
// margin between the clock that stamped the offer and the clock that reads it.
func TestAPaymentIsRememberedAsLongAsItsOfferCouldStand(t *testing.T) {
	clock := time.Unix(1800000000, 0)
	store := &windowStore{inner: NewMemoryStore()}
	g := quotedGate(&StaticFacilitator{
		Verification: Verification{Valid: true},
		Settlement:   Settlement{Success: true},
	}, movingSigner(&clock))
	g.Seen = store

	offer := offered(t, g)
	if response := quotedRequest(t, g, header(t, payment("1")), offer); response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}
	if want := time.Minute + DefaultReplaySafety; store.window != want {
		t.Errorf("replay window = %s, want the life left on the offer plus the margin (%s)", store.window, want)
	}

	second := quotedRequest(t, g, header(t, payment("1")), offer)
	if second.Code != http.StatusPaymentRequired {
		t.Errorf("replayed request: status = %d, want 402", second.Code)
	}
}

func TestAGateWithoutASignerMakesNoOffer(t *testing.T) {
	if got := request(t, gate(&StaticFacilitator{}), "").Header().Get(QuoteHeader); got != "" {
		t.Errorf("%s = %q, want nothing from a gate that signs no quotes", QuoteHeader, got)
	}
}

func TestAQuoteSignerWithoutAKeyIsTheOperatorsMistake(t *testing.T) {
	g := quotedGate(&StaticFacilitator{}, &QuoteSigner{})
	if response := quotedRequest(t, g, "", ""); response.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: this is the operator's mistake, not the client's", response.Code)
	}
}
