package paygate402

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func quoteKey() []byte { return []byte("this server's own secret") }

func quoteSigner(now int64) *QuoteSigner {
	return &QuoteSigner{
		Key: quoteKey(),
		TTL: time.Minute,
		Now: func() time.Time { return time.Unix(now, 0) },
	}
}

func quote() Quote {
	return Quote{
		Amount:   "10000",
		Asset:    "0x0000000000000000000000000000000000000002",
		Network:  "base",
		Resource: "https://api.example.com/report",
		PayTo:    "0x0000000000000000000000000000000000000001",
		Expiry:   time.Unix(1800000000, 0).UTC(),
		Nonce:    "0123456789abcdef0123456789abcdef",
	}
}

// canonicalParts is the signed form of quote(), written out as the sequence of
// parts it is: the domain tag, then each set field as a name and a value.
func canonicalParts() []string {
	return []string{
		"paygate402/quote/v1",
		"amount", "10000",
		"asset", "0x0000000000000000000000000000000000000002",
		"expiry", "1800000000",
		"network", "base",
		"nonce", "0123456789abcdef0123456789abcdef",
		"payTo", "0x0000000000000000000000000000000000000001",
		"resource", "https://api.example.com/report",
	}
}

// canonicalBytes writes the parts the way the format says they are written,
// independently of the code under test: a length, then the bytes. Every part
// here is shorter than 128 bytes, so every length is a single byte.
func canonicalBytes(parts []string) []byte {
	var out []byte
	for _, part := range parts {
		if len(part) > 127 {
			panic("canonicalBytes only writes single-byte lengths")
		}
		out = append(out, byte(len(part)))
		out = append(out, part...)
	}
	return out
}

// parts reads the canonical form back out, so the layout can be asserted
// without a hand-computed digest in the test.
func parts(t *testing.T, canonical []byte) []string {
	t.Helper()
	var out []string
	for len(canonical) > 0 {
		size, read := binary.Uvarint(canonical)
		if read <= 0 {
			t.Fatal("the canonical form is not a sequence of length-prefixed parts")
		}
		canonical = canonical[read:]
		if uint64(len(canonical)) < size {
			t.Fatal("a length ran past the end of the canonical form")
		}
		out = append(out, string(canonical[:size]))
		canonical = canonical[size:]
	}
	return out
}

// This is the promise the whole file exists for: the bytes a quote is signed
// over do not move. Changing this test means invalidating every signature that
// was ever issued, so it should be read as a version bump, not a fix.
func TestCanonicalFormIsFixed(t *testing.T) {
	want := canonicalParts()
	got := parts(t, quote().Canonical())
	if len(got) != len(want) {
		t.Fatalf("canonical parts = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("part %d = %q, want %q", i, got[i], want[i])
		}
	}
	if string(quote().Canonical()) != string(quote().Canonical()) {
		t.Error("the same quote serialised differently twice")
	}
}

// And the same promise at the level of bytes rather than parts: a reader who
// only has the format description must be able to rebuild them exactly, or a
// signature issued today will not verify against another implementation.
func TestCanonicalBytesAreExactlyTheDocumentedEncoding(t *testing.T) {
	want := canonicalBytes(canonicalParts())
	if got := quote().Canonical(); !bytes.Equal(got, want) {
		t.Errorf("canonical = %x, want %x", got, want)
	}
}

// A field left empty is written not at all, which is what lets a later version
// add a field without disturbing the bytes of quotes that do not set it.
func TestAnUnsetFieldIsNotWritten(t *testing.T) {
	bare := quote()
	bare.Network = ""
	bare.Resource = ""
	bare.PayTo = ""
	for _, part := range parts(t, bare.Canonical()) {
		switch part {
		case "network", "resource", "payTo":
			t.Errorf("an unset field was still written: %q", part)
		}
	}
}

// Lengths rather than separators: no value can be read as part of the next one.
func TestValuesCannotBleedIntoTheNextField(t *testing.T) {
	one, other := quote(), quote()
	one.Amount, one.Asset = "1", "23"
	other.Amount, other.Asset = "12", "3"
	if string(one.Canonical()) == string(other.Canonical()) {
		t.Error("two different quotes serialised to the same bytes")
	}
}

func TestTheSignatureIsNotPartOfWhatIsSigned(t *testing.T) {
	signed := quote()
	signed.Signature = "deadbeef"
	if string(signed.Canonical()) != string(quote().Canonical()) {
		t.Error("the signature changed the bytes it is a signature of")
	}
}

// The signature is HMAC-SHA256 over the canonical bytes, hex encoded, and it is
// the same signature every time. Computed here from the format alone, so the
// algorithm and the encoding are pinned along with the layout.
func TestTheSignatureIsHMACOverTheCanonicalForm(t *testing.T) {
	signed, err := quoteSigner(1799999900).Sign(quote())
	if err != nil {
		t.Fatal(err)
	}
	sum := hmac.New(sha256.New, quoteKey())
	sum.Write(canonicalBytes(canonicalParts()))
	if want := hex.EncodeToString(sum.Sum(nil)); signed.Signature != want {
		t.Errorf("signature = %s, want %s", signed.Signature, want)
	}

	again, err := quoteSigner(1799999900).Sign(quote())
	if err != nil {
		t.Fatal(err)
	}
	if again.Signature != signed.Signature {
		t.Error("the same quote signed differently twice")
	}
}

func TestAnIssuedQuoteVerifies(t *testing.T) {
	signer := quoteSigner(1800000000)
	issued, err := signer.Issue(terms())
	if err != nil {
		t.Fatal(err)
	}
	if issued.Amount != terms().MaxAmountRequired || issued.Asset != terms().Asset {
		t.Errorf("quote = %+v, want the terms it was issued for", issued)
	}
	if issued.Expiry.Unix() != 1800000060 {
		t.Errorf("expiry = %d, want a minute from now", issued.Expiry.Unix())
	}
	if issued.Signature == "" {
		t.Fatal("the quote came back unsigned")
	}
	if err := signer.Verify(issued); err != nil {
		t.Errorf("a freshly issued quote did not verify: %v", err)
	}

	again, err := signer.Issue(terms())
	if err != nil {
		t.Fatal(err)
	}
	if again.Nonce == issued.Nonce {
		t.Error("two quotes were issued with the same nonce")
	}
}

// The expiry is signed as whole seconds, so an issued quote must carry whole
// seconds too: otherwise the moment a client reads is not the moment the
// signature covers, and the difference is unsigned.
func TestAnIssuedExpiryTravelsAsItIsSigned(t *testing.T) {
	signer := &QuoteSigner{
		Key: quoteKey(),
		TTL: time.Minute,
		Now: func() time.Time { return time.Unix(1800000000, 500_000_000) },
	}
	issued, err := signer.Issue(terms())
	if err != nil {
		t.Fatal(err)
	}
	if issued.Expiry.Nanosecond() != 0 || issued.Expiry.Unix() != 1800000060 {
		t.Errorf("expiry = %s, want the whole second the signature covers",
			issued.Expiry.UTC().Format(time.RFC3339Nano))
	}
}

// The point of signing: a quote that comes back can be checked against what was
// offered rather than against what the client says was offered.
func TestATamperedQuoteDoesNotVerify(t *testing.T) {
	signer := quoteSigner(1799999900)
	signed, err := signer.Sign(quote())
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(*Quote){
		"cheaper amount":  func(q *Quote) { q.Amount = "1" },
		"another asset":   func(q *Quote) { q.Asset = "0x00000000000000000000000000000000000000ff" },
		"later expiry":    func(q *Quote) { q.Expiry = q.Expiry.Add(time.Hour) },
		"another nonce":   func(q *Quote) { q.Nonce = "ffffffffffffffffffffffffffffffff" },
		"another payee":   func(q *Quote) { q.PayTo = "0x00000000000000000000000000000000000000ee" },
		"dropped network": func(q *Quote) { q.Network = "" },
		"bent signature":  func(q *Quote) { q.Signature = "00" + q.Signature[2:] },
		"junk signature":  func(q *Quote) { q.Signature = "not hex" },
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			tampered := signed
			tamper(&tampered)
			if err := signer.Verify(tampered); !errors.Is(err, ErrQuoteSignature) {
				t.Errorf("error = %v, want a signature failure", err)
			}
		})
	}
}

func TestAQuoteFromAnotherKeyDoesNotVerify(t *testing.T) {
	signed, err := quoteSigner(1799999900).Sign(quote())
	if err != nil {
		t.Fatal(err)
	}
	stranger := &QuoteSigner{Key: []byte("someone else's secret")}
	if err := stranger.Verify(signed); !errors.Is(err, ErrQuoteSignature) {
		t.Errorf("error = %v, want a signature failure", err)
	}
}

func TestAnExpiredQuoteIsRefused(t *testing.T) {
	signed, err := quoteSigner(1799999900).Sign(quote())
	if err != nil {
		t.Fatal(err)
	}
	if err := quoteSigner(1799999999).Verify(signed); err != nil {
		t.Fatalf("a quote still inside its window was refused: %v", err)
	}
	if err := quoteSigner(1800000000).Verify(signed); !errors.Is(err, ErrQuoteExpired) {
		t.Errorf("error = %v, want an expiry failure at the expiry second", err)
	}
}

// A quote travels as JSON and comes back as JSON; the signature must survive
// the trip, since the bytes that are signed are not the JSON.
func TestASignedQuoteSurvivesJSON(t *testing.T) {
	signer := quoteSigner(1799999900)
	signed, err := signer.Sign(quote())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	var back Quote
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if err := signer.Verify(back); err != nil {
		t.Errorf("the signature did not survive JSON: %v", err)
	}
}

func TestQuoteValidateNamesEveryMissingField(t *testing.T) {
	err := Quote{}.Validate()
	if err == nil {
		t.Fatal("an empty quote was accepted")
	}
	for _, field := range []string{"amount", "asset", "expiry", "nonce"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error does not mention %q: %v", field, err)
		}
	}

	notAnAmount := quote()
	notAnAmount.Amount = "0.01"
	if err := notAnAmount.Validate(); !errors.Is(err, ErrQuoteIncomplete) {
		t.Errorf("error = %v, want an amount in the asset's own units to be required", err)
	}
	if err := quote().Validate(); err != nil {
		t.Errorf("a complete quote was refused: %v", err)
	}
}

func TestSigningNeedsAKeyAndACompleteQuote(t *testing.T) {
	if _, err := (&QuoteSigner{}).Sign(quote()); !errors.Is(err, ErrNoQuoteKey) {
		t.Errorf("error = %v, want a missing key to be named", err)
	}
	if _, err := quoteSigner(1800000000).Sign(Quote{Amount: "1"}); !errors.Is(err, ErrQuoteIncomplete) {
		t.Errorf("error = %v, want an incomplete quote to be refused before it is signed", err)
	}
}
