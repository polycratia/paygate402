package paygate402

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Facilitator verifies payments and settles them. This is the seam where the
// chain lives: signature recovery, allowance checks and the actual transfer all
// happen behind it.
type Facilitator interface {
	// Verify reports whether a payment is good for the given terms. A returned
	// error means the question could not be asked; a Verification with
	// Valid=false means it was asked and answered no.
	Verify(ctx context.Context, payment Payment, terms Requirements) (Verification, error)
	// Settle moves the money and reports the transaction.
	Settle(ctx context.Context, payment Payment, terms Requirements) (Settlement, error)
}

// Verification is the facilitator's answer about a payment.
type Verification struct {
	Valid  bool   `json:"isValid"`
	Reason string `json:"invalidReason,omitempty"`
	Payer  string `json:"payer,omitempty"`
}

// HTTPFacilitator talks to a facilitator over its REST API.
type HTTPFacilitator struct {
	// BaseURL of the facilitator, e.g. https://facilitator.example.com
	BaseURL string
	// Client is optional; a sane one with a timeout is used when it is nil,
	// because a facilitator that stops answering must not hold every request
	// on this server open.
	Client *http.Client
	// Authorization, if the facilitator requires it, is sent verbatim.
	Authorization string
}

type facilitatorRequest struct {
	X402Version         int          `json:"x402Version"`
	PaymentPayload      Payment      `json:"paymentPayload"`
	PaymentRequirements Requirements `json:"paymentRequirements"`
}

// Verify asks the facilitator whether a payment holds.
func (f *HTTPFacilitator) Verify(ctx context.Context, payment Payment, terms Requirements) (Verification, error) {
	var out Verification
	err := f.post(ctx, "verify", payment, terms, &out)
	return out, err
}

// Settle asks the facilitator to move the money.
func (f *HTTPFacilitator) Settle(ctx context.Context, payment Payment, terms Requirements) (Settlement, error) {
	var out Settlement
	err := f.post(ctx, "settle", payment, terms, &out)
	return out, err
}

func (f *HTTPFacilitator) post(ctx context.Context, path string, payment Payment, terms Requirements, out any) error {
	endpoint, err := url.JoinPath(f.BaseURL, path)
	if err != nil {
		return fmt.Errorf("facilitator url: %w", err)
	}
	body, err := json.Marshal(facilitatorRequest{
		X402Version:         Version,
		PaymentPayload:      payment,
		PaymentRequirements: terms,
	})
	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/json")
	if f.Authorization != "" {
		request.Header.Set("authorization", f.Authorization)
	}

	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("facilitator %s: %w", path, err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("facilitator %s: status %d", path, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return fmt.Errorf("facilitator %s: %w", path, err)
	}
	return nil
}

// StaticFacilitator answers from a fixed script. It exists so that a server can
// be tested end to end without a chain, and it says what it is: it never looks
// at the payment.
type StaticFacilitator struct {
	Verification Verification
	Settlement   Settlement
	VerifyErr    error
	SettleErr    error
	// Calls records what it was asked, in order, for assertions.
	Calls []string
}

func (s *StaticFacilitator) Verify(context.Context, Payment, Requirements) (Verification, error) {
	s.Calls = append(s.Calls, "verify")
	return s.Verification, s.VerifyErr
}

func (s *StaticFacilitator) Settle(context.Context, Payment, Requirements) (Settlement, error) {
	s.Calls = append(s.Calls, "settle")
	return s.Settlement, s.SettleErr
}

// Trail returns the calls made, for a readable assertion in one line.
func (s *StaticFacilitator) Trail() string { return strings.Join(s.Calls, ",") }
