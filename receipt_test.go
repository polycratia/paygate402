package paygate402

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func receiptKey() []byte { return []byte("this server's own receipt secret") }

func receiptSigner(now int64) *ReceiptSigner {
	return &ReceiptSigner{
		Key:    receiptKey(),
		Issuer: "api.example.com",
		Now:    func() time.Time { return time.Unix(now, 0) },
	}
}

func receipt() Receipt {
	return Receipt{
		Payment:     PaymentKey(payment("1")),
		Payer:       "0xc0ffee",
		Amount:      "10000",
		Asset:       "0x0000000000000000000000000000000000000002",
		Network:     "base",
		Resource:    "https://api.example.com/report",
		PayTo:       "0x0000000000000000000000000000000000000001",
		Transaction: "0xdeadbeef",
		Settled:     time.Unix(1800000000, 0).UTC(),
		Issuer:      "api.example.com",
	}
}

// canonicalReceiptParts is the signed form of receipt(), written out as the
// sequence of parts it is: the domain tag, then each set field as a name and a
// value.
func canonicalReceiptParts() []string {
	return []string{
		"paygate402/receipt/v1",
		"amount", "10000",
		"asset", "0x0000000000000000000000000000000000000002",
		"issuer", "api.example.com",
		"network", "base",
		"payTo", "0x0000000000000000000000000000000000000001",
		"payer", "0xc0ffee",
		"payment", PaymentKey(payment("1")),
		"resource", "https://api.example.com/report",
		"settled", "1800000000",
		"transaction", "0xdeadbeef",
	}
}

// A receipt is kept by whoever paid, so the bytes it is signed over have to
// outlive the version that issued it. Changing this test invalidates every
// receipt ever handed out, and should be read as a version bump, not a fix.
func TestAReceiptsCanonicalFormIsFixed(t *testing.T) {
	want := canonicalReceiptParts()
	got := parts(t, receipt().Canonical())
	if len(got) != len(want) {
		t.Fatalf("canonical parts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d = %q, want %q", i, got[i], want[i])
		}
	}
	if encoded := canonicalBytes(want); !bytes.Equal(receipt().Canonical(), encoded) {
		t.Errorf("canonical = %x, want %x", receipt().Canonical(), encoded)
	}
	// The domain tag is not the quote's, so one key can sign both without
	// either being readable as the other.
	if got[0] == parts(t, quote().Canonical())[0] {
		t.Error("a receipt and a quote are signed under the same domain tag")
	}
}

func TestAReceiptsSignatureIsHMACOverTheCanonicalForm(t *testing.T) {
	signed, err := receiptSigner(1800000000).Sign(receipt())
	if err != nil {
		t.Fatal(err)
	}
	sum := hmac.New(sha256.New, receiptKey())
	sum.Write(canonicalBytes(canonicalReceiptParts()))
	if want := hex.EncodeToString(sum.Sum(nil)); signed.Signature != want {
		t.Errorf("signature = %s, want %s", signed.Signature, want)
	}
	if err := receiptSigner(1800000000).Verify(signed); err != nil {
		t.Errorf("a freshly signed receipt did not verify: %v", err)
	}
	// A quote runs out because the offer behind it is about to change. A
	// settlement stays settled, so its record holds whenever it is presented.
	if err := receiptSigner(1900000000).Verify(signed); err != nil {
		t.Errorf("a receipt was refused years later: %v", err)
	}
}

// The point of signing: a receipt presented back is checked against what was
// issued rather than against what the holder says was issued.
func TestATamperedReceiptDoesNotVerify(t *testing.T) {
	signer := receiptSigner(1800000000)
	signed, err := signer.Sign(receipt())
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(*Receipt){
		"a larger amount":     func(r *Receipt) { r.Amount = "1000000" },
		"another payer":       func(r *Receipt) { r.Payer = "0xbadbad" },
		"another payment":     func(r *Receipt) { r.Payment = PaymentKey(payment("2")) },
		"another transaction": func(r *Receipt) { r.Transaction = "0xfeed" },
		"another resource":    func(r *Receipt) { r.Resource = "https://api.example.com/reports/1" },
		"another moment":      func(r *Receipt) { r.Settled = r.Settled.Add(time.Hour) },
		"dropped issuer":      func(r *Receipt) { r.Issuer = "" },
		"bent signature":      func(r *Receipt) { r.Signature = "00" + r.Signature[2:] },
		"junk signature":      func(r *Receipt) { r.Signature = "not hex" },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			tampered := signed
			tamper(&tampered)
			if err := signer.Verify(tampered); !errors.Is(err, ErrReceiptSignature) {
				t.Errorf("error = %v, want a signature failure", err)
			}
		})
	}

	stranger := &ReceiptSigner{Key: []byte("someone else's secret"), Issuer: "api.example.com"}
	if err := stranger.Verify(signed); !errors.Is(err, ErrReceiptSignature) {
		t.Errorf("error = %v, want a receipt from another key to be refused", err)
	}
}

func TestAReceiptSurvivesItsHeader(t *testing.T) {
	signer := receiptSigner(1800000000)
	signed, err := signer.Sign(receipt())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeReceipt(signed)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeReceipt(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Verify(back); err != nil {
		t.Errorf("the signature did not survive the header: %v", err)
	}
	if back.Settled.Unix() != signed.Settled.Unix() || back.Transaction != signed.Transaction {
		t.Errorf("receipt = %+v, want the one that was sent", back)
	}
	if _, err := DecodeReceipt(strings.TrimRight(encoded, "=")); err != nil {
		t.Errorf("unpadded base64 was refused: %v", err)
	}
	if _, err := DecodeReceipt(""); !errors.Is(err, ErrNoReceipt) {
		t.Errorf("error = %v, want a missing receipt to be named", err)
	}
	if _, err := DecodeReceipt("!!!!"); !errors.Is(err, ErrReceiptMalformed) {
		t.Errorf("error = %v, want an unreadable receipt to be named", err)
	}
}

func TestReceiptValidateNamesEveryMissingField(t *testing.T) {
	err := Receipt{}.Validate()
	if err == nil {
		t.Fatal("an empty receipt was accepted")
	}
	for _, field := range []string{"amount", "asset", "payment", "settled"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error does not mention %q: %v", field, err)
		}
	}

	notAnAmount := receipt()
	notAnAmount.Amount = "0.01"
	if err := notAnAmount.Validate(); !errors.Is(err, ErrReceiptIncomplete) {
		t.Errorf("error = %v, want an amount in the asset's own units to be required", err)
	}
	if err := receipt().Validate(); err != nil {
		t.Errorf("a complete receipt was refused: %v", err)
	}
	if _, err := (&ReceiptSigner{}).Sign(receipt()); !errors.Is(err, ErrNoReceiptKey) {
		t.Errorf("error = %v, want a missing key to be named", err)
	}
	if _, err := receiptSigner(1800000000).Sign(Receipt{Amount: "1"}); !errors.Is(err, ErrReceiptIncomplete) {
		t.Errorf("error = %v, want an incomplete receipt to be refused before it is signed", err)
	}
}

// What the feature is for: the payer walks away from a paid request holding a
// record this server will recognise.
func TestASettledPaymentComesBackWithASignedReceipt(t *testing.T) {
	signer := receiptSigner(1800000000)
	g := gate(&StaticFacilitator{
		Verification: Verification{Valid: true, Payer: "0xabc"},
		Settlement:   Settlement{Success: true, Transaction: "0xdeadbeef", Network: "base"},
	})
	g.Receipts = signer

	paid := payment("1")
	response := request(t, g, header(t, paid))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}
	encoded := response.Header().Get(ReceiptHeader)
	if encoded == "" {
		t.Fatal("the paid response carried no receipt")
	}
	got, err := DecodeReceipt(encoded)
	if err != nil {
		t.Fatalf("the receipt on the response could not be read: %v", err)
	}
	if err := signer.Verify(got); err != nil {
		t.Errorf("the receipt this server issued does not verify here: %v", err)
	}
	if got.Payment != PaymentKey(paid) {
		t.Errorf("receipt payment = %q, want the payment that settled", got.Payment)
	}
	// The settlement named no payer, so the verification's stands.
	if got.Payer != "0xabc" {
		t.Errorf("receipt payer = %q, want the payer the facilitator named", got.Payer)
	}
	if got.Amount != terms().MaxAmountRequired || got.PayTo != terms().PayTo || got.Resource != terms().Resource {
		t.Errorf("receipt = %+v, want the terms that were paid", got)
	}
	if got.Transaction != "0xdeadbeef" || got.Settled.Unix() != 1800000000 {
		t.Errorf("receipt = %+v, want what the facilitator reported and when", got)
	}
}

func TestTheSettlementsOwnPayerIsTheOneOnTheReceipt(t *testing.T) {
	g := gate(&StaticFacilitator{
		Verification: Verification{Valid: true, Payer: "0xabc"},
		Settlement:   Settlement{Success: true, Payer: "0xc0ffee"},
	})
	g.Receipts = receiptSigner(1800000000)

	got, err := DecodeReceipt(request(t, g, header(t, payment("1"))).Header().Get(ReceiptHeader))
	if err != nil {
		t.Fatal(err)
	}
	if got.Payer != "0xc0ffee" {
		t.Errorf("receipt payer = %q, want the one the settlement named", got.Payer)
	}
}

// A receipt is the record of a settlement, so there is nothing to issue until
// one has happened.
func TestNothingCarriesAReceiptUntilAPaymentSettles(t *testing.T) {
	unpaid := gate(&StaticFacilitator{})
	unpaid.Receipts = receiptSigner(1800000000)
	if got := request(t, unpaid, "").Header().Get(ReceiptHeader); got != "" {
		t.Errorf("%s = %q on a 402, want no receipt for a payment nobody made", ReceiptHeader, got)
	}

	declined := gate(&StaticFacilitator{
		Verification: Verification{Valid: true},
		Settlement:   Settlement{Success: false, ErrorReason: "transfer reverted"},
	})
	declined.Receipts = receiptSigner(1800000000)
	if got := request(t, declined, header(t, payment("1"))).Header().Get(ReceiptHeader); got != "" {
		t.Errorf("%s = %q, want no receipt for a payment that did not settle", ReceiptHeader, got)
	}

	unsigned := gate(&StaticFacilitator{
		Verification: Verification{Valid: true},
		Settlement:   Settlement{Success: true},
	})
	if got := request(t, unsigned, header(t, payment("1"))).Header().Get(ReceiptHeader); got != "" {
		t.Errorf("%s = %q, want nothing from a gate that signs no receipts", ReceiptHeader, got)
	}
}

func TestAReceiptSignerWithoutAKeyIsTheOperatorsMistake(t *testing.T) {
	g := gate(&StaticFacilitator{})
	g.Receipts = &ReceiptSigner{}
	if response := request(t, g, ""); response.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: this is the operator's mistake, not the client's", response.Code)
	}
}
