package paygate402

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const mockPayer = "0xc0ffee"

func mockPayment(t *testing.T, from string) Payment {
	t.Helper()
	p, err := MockPayment(terms(), from)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func paymentWith(t *testing.T, payload MockPayload) Payment {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return Payment{
		X402Version: Version,
		Scheme:      terms().Scheme,
		Network:     terms().Network,
		Payload:     raw,
	}
}

func mockPayload() MockPayload {
	return MockPayload{
		From:   mockPayer,
		Amount: terms().MaxAmountRequired,
		Asset:  terms().Asset,
		PayTo:  terms().PayTo,
		Nonce:  "0123456789abcdef",
	}
}

// The whole flow, end to end, with no chain and no account anywhere: the gate
// verifies, the handler runs, the money moves, and the client is told about it.
func TestAGateSettlesThroughTheMockFacilitator(t *testing.T) {
	facilitator := &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}
	response := request(t, gate(facilitator), header(t, mockPayment(t, mockPayer)))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want verify,settle", facilitator.Trail())
	}
	if !strings.Contains(response.Body.String(), `"payer":"`+mockPayer+`"`) {
		t.Errorf("the handler did not see the payer the mock named: %s", response.Body)
	}
	if response.Header().Get(ResponseHeader) == "" {
		t.Error("the settlement was not reported back to the client")
	}

	if got := facilitator.Balance(mockPayer); got != 40_000 {
		t.Errorf("payer balance = %d, want one charge of 10000 taken", got)
	}
	if got := facilitator.Balance(terms().PayTo); got != 10_000 {
		t.Errorf("merchant balance = %d, want the charge arrived", got)
	}
	transfers := facilitator.Transfers()
	if len(transfers) != 1 {
		t.Fatalf("transfers = %+v, want exactly one", transfers)
	}
	if transfers[0].Transaction == "" || transfers[0].To != terms().PayTo {
		t.Errorf("transfer = %+v", transfers[0])
	}
}

func TestTheMockRefusesAPayerItCannotCharge(t *testing.T) {
	facilitator := &MockFacilitator{}
	response := request(t, gate(facilitator), header(t, mockPayment(t, "0xbroke")))

	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", response.Code)
	}
	if !strings.Contains(response.Body.String(), "insufficient funds") {
		t.Errorf("the client was not told why: %s", response.Body)
	}
	if facilitator.Trail() != "verify" {
		t.Errorf("calls = %q: nothing should settle after a failed verification", facilitator.Trail())
	}
	if transfers := facilitator.Transfers(); len(transfers) != 0 {
		t.Errorf("transfers = %+v, want none", transfers)
	}
}

// The mock checks the fields the gate deliberately does not: they live inside
// the payload, and reading the payload is the facilitator's job.
func TestTheMockChecksWhatTheGateCannot(t *testing.T) {
	facilitator := &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}

	cases := map[string]func(*MockPayload){
		"another asset":   func(p *MockPayload) { p.Asset = "0x00000000000000000000000000000000000000ff" },
		"another payee":    func(p *MockPayload) { p.PayTo = "0x00000000000000000000000000000000000000ee" },
		"a smaller amount": func(p *MockPayload) { p.Amount = "1" },
		"an unfunded payer": func(p *MockPayload) { p.From = "0xbroke" },
		"no payer":          func(p *MockPayload) { p.From = "" },
		"no nonce":          func(p *MockPayload) { p.Nonce = "" },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			payload := mockPayload()
			tamper(&payload)

			verification, err := facilitator.Verify(context.Background(), paymentWith(t, payload), terms())
			if err != nil {
				t.Fatalf("the question could not be asked: %v", err)
			}
			if verification.Valid {
				t.Errorf("the mock accepted a payment with %s", name)
			}
			if verification.Reason == "" {
				t.Error("the payment was refused without a reason")
			}
		})
	}

	verification, err := facilitator.Verify(context.Background(), paymentWith(t, mockPayload()), terms())
	if err != nil {
		t.Fatal(err)
	}
	if !verification.Valid || verification.Payer != mockPayer {
		t.Errorf("verification = %+v, want a good payment accepted and the payer named", verification)
	}
}

// Settlement runs the checks again. A mock that settled whatever it was handed
// would make a test of the gate's verification prove nothing.
func TestTheMockWillNotSettleWhatItWouldNotVerify(t *testing.T) {
	facilitator := &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}
	payload := mockPayload()
	payload.Amount = "1"

	settlement, err := facilitator.Settle(context.Background(), paymentWith(t, payload), terms())
	if err != nil {
		t.Fatal(err)
	}
	if settlement.Success {
		t.Error("the mock settled a payment it would have refused")
	}
	if facilitator.Balance(mockPayer) != 50_000 {
		t.Errorf("balance = %d, want it untouched", facilitator.Balance(mockPayer))
	}
}

func TestTheMockWillNotSettleTheSameAuthorisationTwice(t *testing.T) {
	facilitator := &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}
	p := mockPayment(t, mockPayer)

	first, err := facilitator.Settle(context.Background(), p, terms())
	if err != nil || !first.Success {
		t.Fatalf("first settlement = %+v, err = %v", first, err)
	}
	second, err := facilitator.Settle(context.Background(), p, terms())
	if err != nil {
		t.Fatal(err)
	}
	if second.Success {
		t.Error("the mock charged the same authorisation twice")
	}
	if got := facilitator.Balance(mockPayer); got != 40_000 {
		t.Errorf("balance = %d, want exactly one charge", got)
	}
}

// Two calls to MockPayment are two payments, so a test that pays twice is not
// silently testing replay protection instead.
func TestTwoMockPaymentsAreTwoPayments(t *testing.T) {
	facilitator := &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}
	g := gate(facilitator)

	for attempt := 1; attempt <= 2; attempt++ {
		response := request(t, g, header(t, mockPayment(t, mockPayer)))
		if response.Code != http.StatusOK {
			t.Fatalf("payment %d: status = %d, body %s", attempt, response.Code, response.Body)
		}
	}
	if got := facilitator.Balance(mockPayer); got != 30_000 {
		t.Errorf("balance = %d, want two charges", got)
	}
}
