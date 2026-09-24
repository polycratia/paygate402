package paygate402

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// quoteDomain tags the canonical form and carries its own version: a change to
// the layout below takes a new tag rather than silently reinterpreting bytes
// that were already signed.
const quoteDomain = "paygate402/quote/v1"

// QuoteHeader carries a signed quote. It is this package's own header, not one
// x402 defines: the 402 sends the offer behind each set of terms, and a client
// sends back the offer it is answering.
const QuoteHeader = "X-PAYMENT-QUOTE"

// Errors a caller may want to distinguish when handling quotes.
var (
	ErrQuoteIncomplete = errors.New("quote is incomplete")
	ErrQuoteSignature  = errors.New("quote signature does not verify")
	ErrQuoteExpired    = errors.New("quote has expired")
	ErrQuoteMismatch   = errors.New("quote is not an offer for these terms")
	ErrQuoteMalformed  = errors.New("quote header could not be decoded")
	ErrNoQuote         = errors.New("no quote presented")
	ErrNoQuoteKey      = errors.New("paygate402: no quote signing key configured")
)

// Quote is the priced offer behind a 402: how much, in which asset, until when,
// and a nonce that makes this offer one of a kind.
//
// The signature is the issuing server's own, over the canonical form. It says
// "these were my terms", and nothing more: it is not a chain signature and
// reports nothing about whether anyone paid.
type Quote struct {
	Amount    string    `json:"amount"`
	Asset     string    `json:"asset"`
	Network   string    `json:"network,omitempty"`
	Resource  string    `json:"resource,omitempty"`
	PayTo     string    `json:"payTo,omitempty"`
	Expiry    time.Time `json:"expiry"`
	Nonce     string    `json:"nonce"`
	Signature string    `json:"signature,omitempty"`
}

type quoteField struct{ name, value string }

// Canonical returns the bytes a quote is signed over.
//
// The form is fixed so that a signature made by one version of this package
// still verifies under the next: a domain tag, then every set field in a fixed
// order, each part written as its length followed by its bytes. Lengths rather
// than separators mean no value can be read as two fields, and a field left
// empty is not written at all — so a field added in a later version leaves the
// signed bytes of a quote that does not use it exactly as they were.
//
// The signature itself is not part of it, and the expiry is signed as whole
// Unix seconds: sub-second precision is not part of the offer.
func (q Quote) Canonical() []byte {
	out := appendPart(nil, quoteDomain)
	for _, field := range q.canonicalFields() {
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
func (q Quote) canonicalFields() []quoteField {
	expiry := ""
	if !q.Expiry.IsZero() {
		expiry = strconv.FormatInt(q.Expiry.UTC().Unix(), 10)
	}
	return []quoteField{
		{"amount", q.Amount},
		{"asset", q.Asset},
		{"expiry", expiry},
		{"network", q.Network},
		{"nonce", q.Nonce},
		{"payTo", q.PayTo},
		{"resource", q.Resource},
	}
}

func appendPart(out []byte, part string) []byte {
	out = binary.AppendUvarint(out, uint64(len(part)))
	return append(out, part...)
}

// Validate refuses a quote a client could not act on, or one that could not be
// signed meaningfully.
func (q Quote) Validate() error {
	var missing []string
	for _, field := range []quoteField{
		{"amount", q.Amount},
		{"asset", q.Asset},
		{"nonce", q.Nonce},
	} {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if q.Expiry.IsZero() {
		missing = append(missing, "expiry")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing %s", ErrQuoteIncomplete, strings.Join(sorted(missing), ", "))
	}
	if !isUnsignedInteger(q.Amount) {
		return fmt.Errorf("%w: amount %q is not an integer in the asset's own units",
			ErrQuoteIncomplete, q.Amount)
	}
	return nil
}

// Expired reports whether the offer has run out. The comparison is in whole
// seconds, which is the precision the expiry was signed at.
func (q Quote) Expired(now time.Time) bool {
	return now.Unix() >= q.Expiry.Unix()
}

// Covers reports whether this quote is the offer behind these terms.
//
// A signature says the server issued the quote, not that it issued it for what
// is being bought now. Without this check a quote for the cheapest resource on
// a server would stand as the offer behind the most expensive one.
func (q Quote) Covers(terms Requirements) error {
	for _, field := range []struct{ name, quoted, required string }{
		{"amount", q.Amount, terms.MaxAmountRequired},
		{"asset", q.Asset, terms.Asset},
		{"network", q.Network, terms.Network},
		{"payTo", q.PayTo, terms.PayTo},
		{"resource", q.Resource, terms.Resource},
	} {
		if field.quoted != field.required {
			return fmt.Errorf("%w: it offers %s %q, the terms ask for %q",
				ErrQuoteMismatch, field.name, field.quoted, field.required)
		}
	}
	return nil
}

func (q Quote) mac(key []byte) []byte {
	q.Signature = ""
	sum := hmac.New(sha256.New, key)
	sum.Write(q.Canonical())
	return sum.Sum(nil)
}

// EncodeQuote produces an X-PAYMENT-QUOTE header value: base64 of the quote's
// JSON, so an offer travels in a header with no question of escaping.
func EncodeQuote(quote Quote) (string, error) {
	body, err := json.Marshal(quote)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// DecodeQuote reads an X-PAYMENT-QUOTE header. It says what the client sent,
// not that the server ever offered it: QuoteSigner.Verify decides that.
func DecodeQuote(header string) (Quote, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return Quote{}, ErrNoQuote
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		// Some clients strip padding; try the tolerant alphabet before giving up.
		raw, err = base64.RawStdEncoding.DecodeString(header)
		if err != nil {
			return Quote{}, fmt.Errorf("%w: %v", ErrQuoteMalformed, err)
		}
	}
	var quote Quote
	if err := json.Unmarshal(raw, &quote); err != nil {
		return Quote{}, fmt.Errorf("%w: %v", ErrQuoteMalformed, err)
	}
	return quote, nil
}

// QuoteSigner issues quotes and checks the ones that come back. The key is this
// server's own secret; a quote it did not sign does not verify.
type QuoteSigner struct {
	// Key is the HMAC key. Required.
	Key []byte
	// TTL is how long an issued quote stays good. Defaults to a minute.
	TTL time.Duration
	// Now is the clock; time.Now is used when it is nil.
	Now func() time.Time
}

// Issue returns a signed quote for the given terms, with a fresh nonce and an
// expiry a TTL from now.
func (s *QuoteSigner) Issue(terms Requirements) (Quote, error) {
	nonce, err := NewNonce()
	if err != nil {
		return Quote{}, err
	}
	return s.Sign(Quote{
		Amount:   terms.MaxAmountRequired,
		Asset:    terms.Asset,
		Network:  terms.Network,
		Resource: terms.Resource,
		PayTo:    terms.PayTo,
		// Truncated to the precision it is signed at, so the instant a client
		// reads out of the quote is the instant the signature covers.
		Expiry: s.now().Add(s.ttl()).Truncate(time.Second).UTC(),
		Nonce:  nonce,
	})
}

// Sign returns the quote with its signature set.
func (s *QuoteSigner) Sign(quote Quote) (Quote, error) {
	if len(s.Key) == 0 {
		return Quote{}, ErrNoQuoteKey
	}
	quote.Signature = ""
	if err := quote.Validate(); err != nil {
		return Quote{}, err
	}
	quote.Signature = hex.EncodeToString(quote.mac(s.Key))
	return quote, nil
}

// Verify checks that this server issued the quote and that the offer still
// stands. The signature is checked first: a quote nobody here signed is refused
// as forged rather than as late.
func (s *QuoteSigner) Verify(quote Quote) error {
	if len(s.Key) == 0 {
		return ErrNoQuoteKey
	}
	if err := quote.Validate(); err != nil {
		return err
	}
	given, err := hex.DecodeString(quote.Signature)
	if err != nil || len(given) == 0 {
		return ErrQuoteSignature
	}
	if !hmac.Equal(given, quote.mac(s.Key)) {
		return ErrQuoteSignature
	}
	if quote.Expired(s.now()) {
		return fmt.Errorf("%w: it was good until %s", ErrQuoteExpired,
			quote.Expiry.UTC().Format(time.RFC3339))
	}
	return nil
}

func (s *QuoteSigner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *QuoteSigner) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return time.Minute
}

// NewNonce returns a random nonce for a quote.
func NewNonce() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("quote nonce: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func isUnsignedInteger(value string) bool {
	if value == "" {
		return false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
