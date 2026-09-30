package paygate402

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// receiptDomain tags the canonical form and carries its own version: a change
// to the layout below takes a new tag rather than silently reinterpreting bytes
// that were already signed. It is not the quote's tag, so one key can sign both
// without either being readable as the other.
const receiptDomain = "paygate402/receipt/v1"

// ReceiptHeader carries a signed settlement receipt. It is this package's own
// header, not one x402 defines: the paid response hands the payer the record of
// what settled, to keep and to present later.
const ReceiptHeader = "X-PAYMENT-RECEIPT"

// Errors a caller may want to distinguish when handling receipts.
var (
	ErrReceiptIncomplete = errors.New("receipt is incomplete")
	ErrReceiptSignature  = errors.New("receipt signature does not verify")
	ErrReceiptMalformed  = errors.New("receipt header could not be decoded")
	ErrNoReceipt         = errors.New("no receipt presented")
	ErrNoReceiptKey      = errors.New("paygate402: no receipt signing key configured")
)

// Receipt is the record of a payment that settled: which payment, who paid, how
// much, in what asset, to whom, what the facilitator reported, and when.
//
// The signature is the settling server's own. It says "this payment settled
// here", which is this server's statement about its own accounting rather than
// a chain's about a transfer: Transaction is what the facilitator named, and a
// chain is where that is checked.
type Receipt struct {
	// Payment is the replay key of the payment that settled, so a receipt
	// belongs to one payment and cannot be moved to another.
	Payment     string    `json:"payment"`
	Payer       string    `json:"payer,omitempty"`
	Amount      string    `json:"amount"`
	Asset       string    `json:"asset"`
	Network     string    `json:"network,omitempty"`
	Resource    string    `json:"resource,omitempty"`
	PayTo       string    `json:"payTo,omitempty"`
	Transaction string    `json:"transaction,omitempty"`
	Settled     time.Time `json:"settled"`
	Issuer      string    `json:"issuer,omitempty"`
	Signature   string    `json:"signature,omitempty"`
}

type receiptField struct{ name, value string }

// Canonical returns the bytes a receipt is signed over.
//
// The form is the quote's, under the receipt's own domain tag: the tag, then
// every set field in a fixed order, each part written as its length followed by
// its bytes. Lengths rather than separators mean no value can be read as two
// fields, and a field left empty is not written at all — so a field added in a
// later version leaves the signed bytes of a receipt that does not use it
// exactly as they were.
//
// The signature itself is not part of it, and the settlement time is signed as
// whole Unix seconds.
func (r Receipt) Canonical() []byte {
	out := appendPart(nil, receiptDomain)
	for _, field := range r.canonicalFields() {
		if field.value == "" {
			continue
		}
		out = appendPart(out, field.name)
		out = appendPart(out, field.value)
	}
	return out
}

// canonicalFields lists the signed fields, ordered by name. New fields go in
// their alphabetical place; renaming or reordering one breaks every signature
// already issued.
func (r Receipt) canonicalFields() []receiptField {
	settled := ""
	if !r.Settled.IsZero() {
		settled = strconv.FormatInt(r.Settled.UTC().Unix(), 10)
	}
	return []receiptField{
		{"amount", r.Amount},
		{"asset", r.Asset},
		{"issuer", r.Issuer},
		{"network", r.Network},
		{"payTo", r.PayTo},
		{"payer", r.Payer},
		{"payment", r.Payment},
		{"resource", r.Resource},
		{"settled", settled},
		{"transaction", r.Transaction},
	}
}

// Validate refuses a receipt that would record nothing useful.
func (r Receipt) Validate() error {
	var missing []string
	for _, field := range []receiptField{
		{"amount", r.Amount},
		{"asset", r.Asset},
		{"payment", r.Payment},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if r.Settled.IsZero() {
		missing = append(missing, "settled")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing %s", ErrReceiptIncomplete, strings.Join(sorted(missing), ", "))
	}
	if !isUnsignedInteger(r.Amount) {
		return fmt.Errorf("%w: amount %q is not an integer in the asset's own units",
			ErrReceiptIncomplete, r.Amount)
	}
	return nil
}

func (r Receipt) mac(key []byte) []byte {
	r.Signature = ""
	sum := hmac.New(sha256.New, key)
	sum.Write(r.Canonical())
	return sum.Sum(nil)
}

// EncodeReceipt produces an X-PAYMENT-RECEIPT header value: base64 of the
// receipt's JSON, so a record travels in a header with no question of escaping.
func EncodeReceipt(receipt Receipt) (string, error) {
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// DecodeReceipt reads an X-PAYMENT-RECEIPT header. It says what the holder
// presented, not that this server ever issued it: ReceiptSigner.Verify decides
// that.
func DecodeReceipt(header string) (Receipt, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return Receipt{}, ErrNoReceipt
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		// Some clients strip padding; try the tolerant alphabet before giving up.
		raw, err = base64.RawStdEncoding.DecodeString(header)
		if err != nil {
			return Receipt{}, fmt.Errorf("%w: %v", ErrReceiptMalformed, err)
		}
	}
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return Receipt{}, fmt.Errorf("%w: %v", ErrReceiptMalformed, err)
	}
	return receipt, nil
}

// ReceiptSigner issues receipts and checks the ones presented back. The key is
// this server's own secret; a receipt it did not sign does not verify.
type ReceiptSigner struct {
	// Key is the HMAC key. Required.
	Key []byte
	// Issuer, when set, names the server on every receipt it signs, for a payer
	// that keeps receipts from more than one.
	Issuer string
	// Now is the clock; time.Now is used when it is nil.
	Now func() time.Time
}

// Issue returns a signed receipt for a payment the facilitator settled.
func (s *ReceiptSigner) Issue(payment Payment, terms Requirements, settlement Settlement) (Receipt, error) {
	return s.Sign(Receipt{
		Payment:     PaymentKey(payment),
		Payer:       settlement.Payer,
		Amount:      terms.MaxAmountRequired,
		Asset:       terms.Asset,
		Network:     orDefault(settlement.Network, terms.Network),
		Resource:    terms.Resource,
		PayTo:       terms.PayTo,
		Transaction: settlement.Transaction,
		// Truncated to the precision it is signed at, so the instant the holder
		// reads out of the receipt is the instant the signature covers.
		Settled: s.now().Truncate(time.Second).UTC(),
		Issuer:  s.Issuer,
	})
}

// Sign returns the receipt with its signature set.
func (s *ReceiptSigner) Sign(receipt Receipt) (Receipt, error) {
	if len(s.Key) == 0 {
		return Receipt{}, ErrNoReceiptKey
	}
	receipt.Signature = ""
	if err := receipt.Validate(); err != nil {
		return Receipt{}, err
	}
	receipt.Signature = hex.EncodeToString(receipt.mac(s.Key))
	return receipt, nil
}

// Verify checks that this server issued the receipt.
//
// There is nothing else to check: a quote stops standing because the offer
// behind it is about to change, but a settlement stays settled, and a record
// that expired would be no record at all.
func (s *ReceiptSigner) Verify(receipt Receipt) error {
	if len(s.Key) == 0 {
		return ErrNoReceiptKey
	}
	if err := receipt.Validate(); err != nil {
		return err
	}
	given, err := hex.DecodeString(receipt.Signature)
	if err != nil || len(given) == 0 {
		return ErrReceiptSignature
	}
	if !hmac.Equal(given, receipt.mac(s.Key)) {
		return ErrReceiptSignature
	}
	return nil
}

func (s *ReceiptSigner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
