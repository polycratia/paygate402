package paygate402

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// e2eAnswer is one exchange with the gate, as much of it as a client can see.
type e2eAnswer struct {
	status  int
	body    string
	accepts []Requirements
	quote   string
	receipt string
	settled string
}

// e2eServer puts the gate behind a real HTTP server, so the cycle below is
// made of actual requests rather than of calls into a handler.
func e2eServer(t *testing.T, g *Gate) string {
	t.Helper()
	server := httptest.NewServer(g.Handler(paidHandler()))
	t.Cleanup(server.Close)
	return server.URL + "/report"
}

func e2eFetch(t *testing.T, url, paymentHeader, quoteHeader string) e2eAnswer {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if paymentHeader != "" {
		request.Header.Set(PaymentHeader, paymentHeader)
	}
	if quoteHeader != "" {
		request.Header.Set(QuoteHeader, quoteHeader)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}

	answer := e2eAnswer{
		status:  response.StatusCode,
		body:    string(body),
		quote:   response.Header.Get(QuoteHeader),
		receipt: response.Header.Get(ReceiptHeader),
		settled: response.Header.Get(ResponseHeader),
	}
	if answer.status == http.StatusPaymentRequired {
		var challenge Challenge
		if err := json.Unmarshal(body, &challenge); err != nil {
			t.Fatalf("the 402 is not a challenge: %v; body %s", err, body)
		}
		answer.accepts = challenge.Accepts
	}
	return answer
}

// e2ePay funds a payment for the terms a challenge named, the way a client with
// an account at the mock facilitator would.
func e2ePay(t *testing.T, challenge e2eAnswer) string {
	t.Helper()
	if len(challenge.accepts) == 0 {
		t.Fatal("the challenge named no terms to pay against")
	}
	funded, err := MockPayment(challenge.accepts[0], mockPayer)
	if err != nil {
		t.Fatal(err)
	}
	return header(t, funded)
}

func e2eSettlement(t *testing.T, value string) Settlement {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("the %s header is not base64: %v", ResponseHeader, err)
	}
	var settlement Settlement
	if err := json.Unmarshal(raw, &settlement); err != nil {
		t.Fatalf("the %s header is not a settlement: %v", ResponseHeader, err)
	}
	return settlement
}

// The whole cycle, over real HTTP and against a facilitator that keeps
// accounts: the first request comes back as 402 with the terms and the offer
// behind them, the second carries the payment and is served, and the third —
// the same captured headers again — is refused without moving any money.
func TestTheFullCycleThroughTheMockFacilitator(t *testing.T) {
	clock := time.Unix(1800000000, 0)
	signer := movingSigner(&clock)
	receipts := &ReceiptSigner{
		Key:    []byte("a receipt signing secret"),
		Issuer: "api.example.com",
		Now:    func() time.Time { return clock },
	}
	facilitator := &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}
	url := e2eServer(t, &Gate{
		Accepts:     []Requirements{terms()},
		Facilitator: facilitator,
		Quotes:      signer,
		Receipts:    receipts,
		Seen:        NewMemoryStore(),
	})

	challenge := e2eFetch(t, url, "", "")
	if challenge.status != http.StatusPaymentRequired {
		t.Fatalf("unpaid request: status = %d, want 402; body %s", challenge.status, challenge.body)
	}
	if len(challenge.accepts) != 1 || challenge.accepts[0].PayTo != terms().PayTo {
		t.Fatalf("the challenge does not say what to pay: %+v", challenge.accepts)
	}
	offer, err := DecodeQuote(challenge.quote)
	if err != nil {
		t.Fatalf("the offer on the challenge could not be read: %v", err)
	}
	if err := signer.Verify(offer); err != nil {
		t.Fatalf("the offer this server sent does not verify here: %v", err)
	}
	if err := offer.Covers(challenge.accepts[0]); err != nil {
		t.Fatalf("the offer is not for the terms it arrived with: %v", err)
	}
	if facilitator.Trail() != "" {
		t.Errorf("calls = %q: nothing should be asked about an unpaid request", facilitator.Trail())
	}

	paymentHeader := e2ePay(t, challenge)
	served := e2eFetch(t, url, paymentHeader, challenge.quote)
	if served.status != http.StatusOK {
		t.Fatalf("paid request: status = %d, want 200; body %s", served.status, served.body)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want verify,settle", facilitator.Trail())
	}
	if !strings.Contains(served.body, `"payer":"`+mockPayer+`"`) {
		t.Errorf("the handler did not see the payer the mock named: %s", served.body)
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

	settlement := e2eSettlement(t, served.settled)
	if !settlement.Success || settlement.Transaction != transfers[0].Transaction {
		t.Errorf("settlement header = %+v, want the transfer the mock made", settlement)
	}

	sent, err := DecodePayment(paymentHeader)
	if err != nil {
		t.Fatal(err)
	}
	record, err := DecodeReceipt(served.receipt)
	if err != nil {
		t.Fatalf("the paid response carried no readable receipt: %v", err)
	}
	if err := receipts.Verify(record); err != nil {
		t.Errorf("the receipt this server issued does not verify here: %v", err)
	}
	if record.Payment != PaymentKey(sent) || record.Payer != mockPayer {
		t.Errorf("receipt = %+v, want the payment that settled and who paid it", record)
	}
	if record.Amount != terms().MaxAmountRequired || record.Transaction != settlement.Transaction {
		t.Errorf("receipt = %+v, want what the facilitator reported", record)
	}

	// The captured headers a second time. Verification is asked again — a
	// payment is only known to be spent once it is known to be good — but
	// nothing settles and no money moves.
	replay := e2eFetch(t, url, paymentHeader, challenge.quote)
	if replay.status != http.StatusPaymentRequired {
		t.Fatalf("replayed request: status = %d, want 402; body %s", replay.status, replay.body)
	}
	if !strings.Contains(replay.body, "already been used") {
		t.Errorf("the replay was refused for the wrong reason: %s", replay.body)
	}
	if facilitator.Trail() != "verify,settle,verify" {
		t.Errorf("calls = %q, want the replay stopped before settlement", facilitator.Trail())
	}
	if got := facilitator.Transfers(); len(got) != 1 {
		t.Errorf("transfers = %+v, want the replay to have moved nothing", got)
	}
	if got := facilitator.Balance(mockPayer); got != 40_000 {
		t.Errorf("payer balance = %d, want exactly one charge", got)
	}
	if replay.receipt != "" {
		t.Error("a refused request carried a receipt")
	}
	if replay.quote == "" {
		t.Error("the refusal did not carry a fresh offer to pay against")
	}
}

// A payment that arrives after its offer ran out is refused before the
// facilitator is asked anything — and the payment itself is left unspent, so
// the same one goes through against a fresh offer.
func TestAnOfferThatRanOutIsRefusedBeforeTheMockIsAsked(t *testing.T) {
	clock := time.Unix(1800000000, 0)
	facilitator := &MockFacilitator{Balances: map[string]int64{mockPayer: 50_000}}
	url := e2eServer(t, &Gate{
		Accepts:     []Requirements{terms()},
		Facilitator: facilitator,
		Quotes:      movingSigner(&clock),
		Seen:        NewMemoryStore(),
	})

	challenge := e2eFetch(t, url, "", "")
	if challenge.status != http.StatusPaymentRequired {
		t.Fatalf("unpaid request: status = %d, want 402; body %s", challenge.status, challenge.body)
	}
	paymentHeader := e2ePay(t, challenge)

	// The offer was good for a minute.
	clock = clock.Add(2 * time.Minute)

	refused := e2eFetch(t, url, paymentHeader, challenge.quote)
	if refused.status != http.StatusPaymentRequired {
		t.Fatalf("late payment: status = %d, want 402; body %s", refused.status, refused.body)
	}
	if !strings.Contains(refused.body, "has expired") {
		t.Errorf("the client was not told the offer had run out: %s", refused.body)
	}
	if facilitator.Trail() != "" {
		t.Errorf("calls = %q: an offer this server no longer stands behind is not a question for a facilitator", facilitator.Trail())
	}
	if got := facilitator.Balance(mockPayer); got != 50_000 {
		t.Errorf("payer balance = %d, want it untouched", got)
	}
	if refused.quote == "" {
		t.Fatal("the refusal did not carry a fresh offer to pay against")
	}

	served := e2eFetch(t, url, paymentHeader, refused.quote)
	if served.status != http.StatusOK {
		t.Fatalf("paid against the fresh offer: status = %d, want 200; body %s", served.status, served.body)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want verify,settle", facilitator.Trail())
	}
	if got := facilitator.Balance(mockPayer); got != 40_000 {
		t.Errorf("payer balance = %d, want one charge once the offer was current", got)
	}
}
