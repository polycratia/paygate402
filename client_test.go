package paygate402

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// transportWallet pays the way a client funded at a MockFacilitator would, and
// records what it was asked.
type transportWallet struct {
	from   string
	err    error
	paid   int
	offers []Quote
}

func (w *transportWallet) Pay(_ context.Context, terms Requirements, offer Quote) (Payment, error) {
	w.paid++
	w.offers = append(w.offers, offer)
	if w.err != nil {
		return Payment{}, w.err
	}
	return MockPayment(terms, w.from)
}

func transportServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func fundedMock() *MockFacilitator {
	return &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}
}

func transportBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The round trip a paying client exists for, in process and without a chain:
// 402 with the terms, one payment, one retry, and the settlement the server
// reported read back off the paid response.
func TestTheTransportAnswersOne402WithOnePayment(t *testing.T) {
	facilitator := fundedMock()
	server := transportServer(t, gate(facilitator).Handler(paidHandler()))
	wallet := &transportWallet{from: mockPayer}

	var settled []Settlement
	client := &http.Client{Transport: &Transport{
		Wallet:  wallet,
		Settled: func(s Settlement) { settled = append(settled, s) },
	}}

	response, err := client.Get(server.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.StatusCode, transportBody(t, response))
	}
	if body := transportBody(t, response); !strings.Contains(body, `"payer":"`+mockPayer+`"`) {
		t.Errorf("the paid response did not come from behind the gate: %s", body)
	}
	if wallet.paid != 1 {
		t.Errorf("the wallet was asked %d times, want exactly one payment", wallet.paid)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want verify,settle", facilitator.Trail())
	}
	if got := facilitator.Balance(mockPayer); got != 40_000 {
		t.Errorf("payer balance = %d, want exactly one charge taken", got)
	}
	if len(settled) != 1 {
		t.Fatalf("settlements reported = %d, want the one the paid response carried", len(settled))
	}
	if !settled[0].Success || settled[0].Transaction == "" {
		t.Errorf("settlement = %+v, want the transaction the facilitator named", settled[0])
	}
}

// A gate that signs quotes refuses a payment answering no offer, so a 200 here
// is the proof that the offer travelled back with the payment.
func TestTheTransportEchoesTheOfferItIsAnswering(t *testing.T) {
	signer := quoteSigner(1800000000)
	facilitator := fundedMock()
	server := transportServer(t, quotedGate(facilitator, signer).Handler(paidHandler()))
	wallet := &transportWallet{from: mockPayer}

	client := &http.Client{Transport: &Transport{Wallet: wallet}}
	response, err := client.Get(server.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.StatusCode, transportBody(t, response))
	}
	response.Body.Close()

	if len(wallet.offers) != 1 {
		t.Fatalf("the wallet was shown %d offers, want the one on the 402", len(wallet.offers))
	}
	if err := signer.Verify(wallet.offers[0]); err != nil {
		t.Errorf("the wallet was shown an offer the server did not sign: %v", err)
	}
	if err := wallet.offers[0].Covers(terms()); err != nil {
		t.Errorf("the wallet was shown an offer for other terms: %v", err)
	}
}

// The retry is the same request, body and all, or the handler would be paid to
// answer a different question.
func TestAResendableBodyIsSentAgainWithThePayment(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(raw)
	})
	server := transportServer(t, gate(fundedMock()).Handler(echo))

	client := &http.Client{Transport: &Transport{Wallet: &transportWallet{from: mockPayer}}}
	response, err := client.Post(server.URL+"/report", "text/plain", strings.NewReader("the question"))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.StatusCode, transportBody(t, response))
	}
	if body := transportBody(t, response); body != "the question" {
		t.Errorf("the handler answered %q, want the body sent again with the payment", body)
	}
}

// One payment, one retry. A 402 answering the payment is an answer, not a
// second price, and paying again would charge twice for one refusal.
func TestTheTransportPaysOnlyOnce(t *testing.T) {
	facilitator := &MockFacilitator{}
	server := transportServer(t, gate(facilitator).Handler(paidHandler()))
	wallet := &transportWallet{from: "0xbroke"}

	client := &http.Client{Transport: &Transport{Wallet: wallet}}
	response, err := client.Get(server.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want the refusal passed through", response.StatusCode)
	}
	if body := transportBody(t, response); !strings.Contains(body, "insufficient funds") {
		t.Errorf("the caller was not told why: %s", body)
	}
	if wallet.paid != 1 {
		t.Errorf("the wallet was asked %d times, want one payment and no second attempt", wallet.paid)
	}
	if facilitator.Trail() != "verify" {
		t.Errorf("calls = %q, want the one payment put to the facilitator once", facilitator.Trail())
	}
}

func TestARequestThatAlreadyCarriesAPaymentIsNotPaidForAgain(t *testing.T) {
	server := transportServer(t, gate(&MockFacilitator{}).Handler(paidHandler()))
	wallet := &transportWallet{from: mockPayer}

	request, err := http.NewRequest(http.MethodGet, server.URL+"/report", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(PaymentHeader, header(t, mockPayment(t, mockPayer)))

	client := &http.Client{Transport: &Transport{Wallet: wallet}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want the refusal passed through", response.StatusCode)
	}
	if wallet.paid != 0 {
		t.Errorf("the wallet was asked %d times for a request that had already paid once", wallet.paid)
	}
}

// A stream has no second copy, so there is no request left to retry and nothing
// worth paying for. The 402 goes back untouched, terms and all, so the caller
// can buffer the body and pay deliberately.
func TestTheTransportWillNotPayForABodyItCannotResend(t *testing.T) {
	facilitator := fundedMock()
	server := transportServer(t, gate(facilitator).Handler(paidHandler()))
	wallet := &transportWallet{from: mockPayer}

	// The wrapper hides *strings.Reader, so net/http cannot supply a GetBody.
	stream := struct{ io.Reader }{strings.NewReader("a stream nobody kept a copy of")}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/report", stream)
	if err != nil {
		t.Fatal(err)
	}
	if request.GetBody != nil {
		t.Fatal("the test built a request that can be resent after all")
	}

	client := &http.Client{Transport: &Transport{Wallet: wallet}}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want the 402 handed back", response.StatusCode)
	}
	if wallet.paid != 0 {
		t.Errorf("the wallet paid %d times for a request that cannot be made again", wallet.paid)
	}
	if facilitator.Trail() != "" {
		t.Errorf("calls = %q, want nothing put to the facilitator", facilitator.Trail())
	}

	var challenge Challenge
	if err := json.Unmarshal([]byte(transportBody(t, response)), &challenge); err != nil {
		t.Fatalf("the 402 came back without its body: %v", err)
	}
	if len(challenge.Accepts) != 1 {
		t.Errorf("challenge = %+v, want the terms left for the caller to read", challenge)
	}
}

func TestAResponseThatIsNot402IsNotPaidFor(t *testing.T) {
	server := transportServer(t, paidHandler())
	wallet := &transportWallet{from: mockPayer}

	client := &http.Client{Transport: &Transport{Wallet: wallet}}
	response, err := client.Get(server.URL + "/report")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", response.StatusCode)
	}
	if wallet.paid != 0 {
		t.Errorf("the wallet was asked %d times for a response nobody charged for", wallet.paid)
	}
}

func TestATransportWithoutAWalletIsTheCallersMistake(t *testing.T) {
	client := &http.Client{Transport: &Transport{}}
	if _, err := client.Get("http://api.invalid/report"); !errors.Is(err, ErrNoWallet) {
		t.Errorf("error = %v, want a missing wallet to be named", err)
	}
}

// A wallet that cannot pay is not the same as a server that refused: the reason
// comes back rather than looking like an ordinary 402.
func TestAWalletThatCannotPaySaysSo(t *testing.T) {
	server := transportServer(t, gate(fundedMock()).Handler(paidHandler()))
	locked := errors.New("the signing key is locked")

	client := &http.Client{Transport: &Transport{Wallet: &transportWallet{err: locked}}}
	if _, err := client.Get(server.URL + "/report"); !errors.Is(err, locked) {
		t.Errorf("error = %v, want the wallet's own reason", err)
	}
}

func TestSettlementRoundTripsThroughItsHeader(t *testing.T) {
	encoded, err := EncodeSettlement(Settlement{Success: true, Transaction: "0xfeed", Network: "base"})
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeSettlement(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Success || back.Transaction != "0xfeed" || back.Network != "base" {
		t.Errorf("settlement = %+v, want what was encoded", back)
	}

	// Servers that strip base64 padding are still understood.
	if _, err := DecodeSettlement(strings.TrimRight(encoded, "=")); err != nil {
		t.Errorf("unpadded base64 was refused: %v", err)
	}
	if _, err := DecodeSettlement("   "); !errors.Is(err, ErrNoSettlement) {
		t.Errorf("error = %v, want a missing header to be distinguishable", err)
	}
	if _, err := DecodeSettlement("!!!!"); !errors.Is(err, ErrMalformed) {
		t.Errorf("error = %v, want a malformed header to be named as one", err)
	}
}
