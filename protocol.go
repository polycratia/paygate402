// Package paygate402 puts an x402 payment gate in front of an HTTP handler.
//
// The protocol is short: an unpaid request comes back as 402 with a list of
// terms the server accepts, the client repeats the request with an X-PAYMENT
// header, and the server has that payment verified and settled before it
// serves the response.
//
// What this package does not do is check signatures or move money. In x402
// that work belongs to a facilitator — a service that verifies a payment
// payload and settles it on chain — and the boundary is honoured here rather
// than blurred: cryptographic verification of a chain signature has no business
// living in a web middleware, and a middleware that claimed to do it while
// merely decoding base64 would be worse than useless.
package paygate402

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Version is the x402 protocol version this package speaks. The protocol is
// young and still moving; a payload announcing anything else is refused rather
// than interpreted hopefully.
const Version = 1

// Header names defined by x402.
const (
	PaymentHeader  = "X-PAYMENT"
	ResponseHeader = "X-PAYMENT-RESPONSE"
)

// Requirements is one set of terms a server will accept: what to pay, in what
// asset, on what network, to whom.
type Requirements struct {
	Scheme            string         `json:"scheme"`
	Network           string         `json:"network"`
	MaxAmountRequired string         `json:"maxAmountRequired"`
	Resource          string         `json:"resource"`
	Description       string         `json:"description,omitempty"`
	MimeType          string         `json:"mimeType,omitempty"`
	PayTo             string         `json:"payTo"`
	MaxTimeoutSeconds int            `json:"maxTimeoutSeconds,omitempty"`
	Asset             string         `json:"asset"`
	Extra             map[string]any `json:"extra,omitempty"`
}

// Validate refuses terms a client could not act on. A 402 that does not say
// where to pay, or how much, wastes a round trip and looks like a broken
// server.
func (r Requirements) Validate() error {
	var missing []string
	for name, value := range map[string]string{
		"scheme":            r.Scheme,
		"network":           r.Network,
		"maxAmountRequired": r.MaxAmountRequired,
		"resource":          r.Resource,
		"payTo":             r.PayTo,
		"asset":             r.Asset,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		// Sorted so the message is the same every time it is read.
		return fmt.Errorf("payment requirements are missing: %s", strings.Join(sorted(missing), ", "))
	}
	return nil
}

// Challenge is the body of a 402 response: why, and what would be accepted.
type Challenge struct {
	X402Version int            `json:"x402Version"`
	Error       string         `json:"error,omitempty"`
	Accepts     []Requirements `json:"accepts"`
}

// Payment is what a client sends back in the X-PAYMENT header. The payload is
// scheme-specific and deliberately left opaque: this package routes it to the
// facilitator, it does not interpret it.
type Payment struct {
	X402Version int             `json:"x402Version"`
	Scheme      string          `json:"scheme"`
	Network     string          `json:"network"`
	Payload     json.RawMessage `json:"payload"`
}

// Settlement is what the facilitator reports after moving the money.
type Settlement struct {
	Success     bool   `json:"success"`
	Transaction string `json:"transaction,omitempty"`
	Network     string `json:"network,omitempty"`
	Payer       string `json:"payer,omitempty"`
	ErrorReason string `json:"errorReason,omitempty"`
}

// Errors a caller may want to distinguish.
var (
	ErrNoPayment      = errors.New("no payment presented")
	ErrMalformed      = errors.New("payment header could not be decoded")
	ErrWrongVersion   = errors.New("unsupported x402 version")
	ErrNoMatchingTerm = errors.New("payment does not match any accepted terms")
)

// DecodePayment reads the X-PAYMENT header: base64 of a JSON object.
func DecodePayment(header string) (Payment, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return Payment{}, ErrNoPayment
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil {
		// Some clients strip padding; try the tolerant alphabet before giving up.
		raw, err = base64.RawStdEncoding.DecodeString(header)
		if err != nil {
			return Payment{}, fmt.Errorf("%w: %v", ErrMalformed, err)
		}
	}
	var payment Payment
	if err := json.Unmarshal(raw, &payment); err != nil {
		return Payment{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if payment.X402Version != Version {
		return Payment{}, fmt.Errorf("%w: got %d, this server speaks %d",
			ErrWrongVersion, payment.X402Version, Version)
	}
	if payment.Scheme == "" || payment.Network == "" {
		return Payment{}, fmt.Errorf("%w: scheme and network are required", ErrMalformed)
	}
	return payment, nil
}

// EncodePayment produces an X-PAYMENT header value. Clients need this; so do
// tests, and a package whose own tests cannot speak its protocol is hard to
// trust.
func EncodePayment(payment Payment) (string, error) {
	body, err := json.Marshal(payment)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// EncodeSettlement produces an X-PAYMENT-RESPONSE header value.
func EncodeSettlement(s Settlement) (string, error) {
	body, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(body), nil
}

// Match finds the terms a payment is answering.
//
// Scheme and network must agree exactly. Nothing else is compared here: the
// amount, the asset and the recipient live inside a scheme-specific payload,
// and the facilitator is the component that can read it. Guessing at those
// fields here would produce a middleware that accepts a payment the chain
// never saw.
func Match(payment Payment, accepts []Requirements) (Requirements, error) {
	for _, candidate := range accepts {
		if candidate.Scheme == payment.Scheme && candidate.Network == payment.Network {
			return candidate, nil
		}
	}
	return Requirements{}, fmt.Errorf("%w: %s on %s", ErrNoMatchingTerm, payment.Scheme, payment.Network)
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
