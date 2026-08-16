package paygate402

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func terms() Requirements {
	return Requirements{
		Scheme:            "exact",
		Network:           "base",
		MaxAmountRequired: "10000",
		Resource:          "https://api.example.com/report",
		PayTo:             "0x0000000000000000000000000000000000000001",
		Asset:             "0x0000000000000000000000000000000000000002",
		MaxTimeoutSeconds: 60,
	}
}

func payment(nonce string) Payment {
	return Payment{
		X402Version: Version,
		Scheme:      "exact",
		Network:     "base",
		Payload:     json.RawMessage(`{"nonce":"` + nonce + `"}`),
	}
}

func header(t *testing.T, p Payment) string {
	t.Helper()
	encoded, err := EncodePayment(p)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func paidHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		payer, _ := Payer(r.Context())
		w.Write([]byte(`{"report":"ok","payer":"` + payer + `"}`))
	})
}

func gate(f Facilitator) *Gate {
	return &Gate{Accepts: []Requirements{terms()}, Facilitator: f, Seen: NewMemoryStore()}
}

func request(t *testing.T, g *Gate, paymentHeader string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil)
	if paymentHeader != "" {
		r.Header.Set(PaymentHeader, paymentHeader)
	}
	w := httptest.NewRecorder()
	g.Handler(paidHandler()).ServeHTTP(w, r)
	return w
}

func TestUnpaidRequestGetsTheTerms(t *testing.T) {
	facilitator := &StaticFacilitator{}
	response := request(t, gate(facilitator), "")

	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", response.Code)
	}
	var challenge Challenge
	if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("challenge is not JSON: %v", err)
	}
	if challenge.X402Version != Version || len(challenge.Accepts) != 1 {
		t.Errorf("challenge = %+v, want the accepted terms", challenge)
	}
	if challenge.Accepts[0].PayTo != terms().PayTo {
		t.Error("the challenge does not say where to pay")
	}
	if facilitator.Trail() != "" {
		t.Errorf("facilitator was called for an unpaid request: %s", facilitator.Trail())
	}
}

func TestPaidRequestIsVerifiedThenSettled(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: true, Payer: "0xabc"},
		Settlement:   Settlement{Success: true, Transaction: "0xdeadbeef", Network: "base"},
	}
	response := request(t, gate(facilitator), header(t, payment("1")))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}
	// Order matters: money moves only after the handler produced a good answer.
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want verify,settle", facilitator.Trail())
	}
	if !strings.Contains(response.Body.String(), `"payer":"0xabc"`) {
		t.Errorf("the handler did not see the payer: %s", response.Body)
	}
	if response.Header().Get(ResponseHeader) == "" {
		t.Error("the settlement was not reported back to the client")
	}
}

func TestPaymentIsRefusedWhenTheFacilitatorSaysNo(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: false, Reason: "insufficient allowance"},
	}
	response := request(t, gate(facilitator), header(t, payment("1")))

	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", response.Code)
	}
	if !strings.Contains(response.Body.String(), "insufficient allowance") {
		t.Errorf("the client was not told why: %s", response.Body)
	}
	if facilitator.Trail() != "verify" {
		t.Errorf("calls = %q: nothing should settle after a failed verification", facilitator.Trail())
	}
}

// The reason the handler runs into a buffer: a client must never receive a paid
// response for a payment that then failed to settle.
func TestNothingIsServedWhenSettlementFails(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: true},
		Settlement:   Settlement{Success: false, ErrorReason: "transfer reverted"},
	}
	response := request(t, gate(facilitator), header(t, payment("1")))

	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", response.Code)
	}
	if strings.Contains(response.Body.String(), `"report":"ok"`) {
		t.Errorf("the paid body leaked despite a failed settlement: %s", response.Body)
	}
	if !strings.Contains(response.Body.String(), "transfer reverted") {
		t.Errorf("the client was not told why: %s", response.Body)
	}
}

// The opposite guard: work that failed is not charged for.
func TestNothingIsChargedWhenTheHandlerFails(t *testing.T) {
	facilitator := &StaticFacilitator{Verification: Verification{Valid: true}}
	g := gate(facilitator)

	r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil)
	r.Header.Set(PaymentHeader, header(t, payment("1")))
	w := httptest.NewRecorder()
	g.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is down", http.StatusServiceUnavailable)
	})).ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the handler's own failure to pass through", w.Code)
	}
	if facilitator.Trail() != "verify" {
		t.Errorf("calls = %q: a failed request must not settle", facilitator.Trail())
	}
}

func TestAPaymentCannotBeUsedTwice(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: true},
		Settlement:   Settlement{Success: true},
	}
	g := gate(facilitator)
	captured := header(t, payment("1"))

	if first := request(t, g, captured); first.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", first.Code)
	}
	second := request(t, g, captured)
	if second.Code != http.StatusPaymentRequired {
		t.Fatalf("replayed request: status = %d, want 402", second.Code)
	}
	if !strings.Contains(second.Body.String(), "already been used") {
		t.Errorf("the replay was refused for the wrong reason: %s", second.Body)
	}

	// A different payment still goes through.
	if fresh := request(t, g, header(t, payment("2"))); fresh.Code != http.StatusOK {
		t.Errorf("a fresh payment was refused: %d %s", fresh.Code, fresh.Body)
	}
}

// A payment rejected at verification never touched the chain, so the same
// signed payload must be retryable: the client fixes the allowance and resends.
// Burning the key at first sight would make every verification failure final.
func TestARejectedPaymentCanBeRetriedAfterTheProblemIsFixed(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: false, Reason: "insufficient allowance"},
	}
	g := gate(facilitator)
	captured := header(t, payment("1"))

	if first := request(t, g, captured); first.Code != http.StatusPaymentRequired {
		t.Fatalf("first attempt: status = %d, want 402", first.Code)
	}

	// The client fixes the allowance; the facilitator now accepts.
	facilitator.Verification = Verification{Valid: true}
	facilitator.Settlement = Settlement{Success: true}

	second := request(t, g, captured)
	if second.Code != http.StatusOK {
		t.Fatalf("retry after fixing: status = %d, want 200; body %s", second.Code, second.Body)
	}
	// And only now is the payment spent.
	if third := request(t, g, captured); third.Code != http.StatusPaymentRequired {
		t.Errorf("replay after settlement: status = %d, want 402", third.Code)
	}
}

// An unreachable facilitator is this server's problem. Answering 402 would tell
// the client to pay again for something they may have already paid.
func TestAnUnreachableFacilitatorIsNotTheClientsFault(t *testing.T) {
	facilitator := &StaticFacilitator{VerifyErr: errors.New("connection refused")}
	response := request(t, gate(facilitator), header(t, payment("1")))

	if response.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 rather than a 402 that asks for another payment", response.Code)
	}
}

func TestMalformedPaymentsAreRefusedWithAReason(t *testing.T) {
	wrongVersion := payment("1")
	wrongVersion.X402Version = 99
	otherNetwork := payment("1")
	otherNetwork.Network = "solana"

	cases := map[string]string{
		"not base64":     "!!!!",
		"not json":       "aGVsbG8=", // "hello"
		"wrong version":  header(t, wrongVersion),
		"unknown scheme": header(t, otherNetwork),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			response := request(t, gate(&StaticFacilitator{}), value)
			if response.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402", response.Code)
			}
			var challenge Challenge
			if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil {
				t.Fatalf("challenge is not JSON: %v", err)
			}
			if challenge.Error == "" {
				t.Error("the client was refused without being told why")
			}
		})
	}
}

func TestAMisconfiguredGateFailsLoudly(t *testing.T) {
	cases := map[string]*Gate{
		"no terms":       {Facilitator: &StaticFacilitator{}},
		"no facilitator": {Accepts: []Requirements{terms()}},
		"incomplete terms": {
			Accepts:     []Requirements{{Scheme: "exact", Network: "base"}},
			Facilitator: &StaticFacilitator{},
		},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil)
			w := httptest.NewRecorder()
			g.Handler(paidHandler()).ServeHTTP(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500: this is the operator's mistake, not the client's", w.Code)
			}
		})
	}
}

func TestReplayWindowExpires(t *testing.T) {
	store := NewMemoryStore()
	clock := time.Now()
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
}

func TestPaymentKeyDistinguishesPayloads(t *testing.T) {
	if PaymentKey(payment("1")) == PaymentKey(payment("2")) {
		t.Error("two different payloads share a replay key")
	}
	if PaymentKey(payment("1")) != PaymentKey(payment("1")) {
		t.Error("the same payload hashed differently twice")
	}
}
