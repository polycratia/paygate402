package paygate402

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// MockPayload is the payment payload MockFacilitator understands.
//
// A facilitator owns the shape of its scheme's payload, and this one is a
// test's: who pays, how much they authorise, in what asset, to whom, and a
// nonce that makes the authorisation one of a kind. There is no signature in it
// and none is checked — the mock stands in for a facilitator's decisions, not
// for its proofs.
type MockPayload struct {
	From   string `json:"from"`
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
	PayTo  string `json:"payTo"`
	Nonce  string `json:"nonce"`
}

// MockPayment builds the payment a client funded at a MockFacilitator would
// send for these terms. The nonce is fresh, so two calls make two distinct
// payments rather than a replay of one.
func MockPayment(terms Requirements, from string) (Payment, error) {
	nonce, err := NewNonce()
	if err != nil {
		return Payment{}, err
	}
	payload, err := json.Marshal(MockPayload{
		From:   from,
		Amount: terms.MaxAmountRequired,
		Asset:  terms.Asset,
		PayTo:  terms.PayTo,
		Nonce:  nonce,
	})
	if err != nil {
		return Payment{}, err
	}
	return Payment{
		X402Version: Version,
		Scheme:      terms.Scheme,
		Network:     terms.Network,
		Payload:     payload,
	}, nil
}

// MockTransfer is one payment the mock facilitator settled.
type MockTransfer struct {
	From        string
	To          string
	Asset       string
	Amount      int64
	Transaction string
}

// MockFacilitator is a facilitator that keeps its accounts in memory.
//
// It exists so that a server can be tested end to end without a chain: it reads
// the payload it defines, checks a payment the way a real facilitator would —
// the payer, the amount, the asset and the recipient, none of which a web layer
// can honestly check — and moves mock money between balances.
//
// Settlement runs the same checks as verification, because a facilitator that
// settled whatever it was handed would make the gate's verification decorative.
//
// The zero value is usable and has no accounts, so every payer is unfunded.
type MockFacilitator struct {
	// Balances is what each address can spend, in the asset's own units.
	// Settlement moves amounts between the entries here.
	Balances map[string]int64
	// VerifyErr and SettleErr, when set, fail the call the way an unreachable
	// facilitator does: the question could not be asked, which the gate must not
	// read as an answer of no.
	VerifyErr error
	SettleErr error

	mu        sync.Mutex
	settled   map[string]bool
	transfers []MockTransfer
	calls     []string
}

// Verify reports whether the mock would let this payment through.
func (m *MockFacilitator) Verify(_ context.Context, payment Payment, terms Requirements) (Verification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, "verify")
	if m.VerifyErr != nil {
		return Verification{}, m.VerifyErr
	}
	payload, refusal := m.check(payment, terms)
	if refusal != "" {
		return Verification{Valid: false, Reason: refusal}, nil
	}
	return Verification{Valid: true, Payer: payload.From}, nil
}

// Settle moves the mock money and reports the transfer.
func (m *MockFacilitator) Settle(_ context.Context, payment Payment, terms Requirements) (Settlement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls = append(m.calls, "settle")
	if m.SettleErr != nil {
		return Settlement{}, m.SettleErr
	}
	payload, refusal := m.check(payment, terms)
	if refusal != "" {
		return Settlement{Success: false, ErrorReason: refusal}, nil
	}

	key := PaymentKey(payment)
	if m.settled[key] {
		return Settlement{Success: false, ErrorReason: "this authorisation was already settled"}, nil
	}
	amount, _ := strconv.ParseInt(payload.Amount, 10, 64)

	if m.Balances == nil {
		m.Balances = map[string]int64{}
	}
	if m.settled == nil {
		m.settled = map[string]bool{}
	}
	m.Balances[payload.From] -= amount
	m.Balances[payload.PayTo] += amount
	m.settled[key] = true

	transfer := MockTransfer{
		From:        payload.From,
		To:          payload.PayTo,
		Asset:       payload.Asset,
		Amount:      amount,
		Transaction: "0x" + key[:16],
	}
	m.transfers = append(m.transfers, transfer)

	return Settlement{
		Success:     true,
		Transaction: transfer.Transaction,
		Network:     terms.Network,
		Payer:       payload.From,
	}, nil
}

// check returns the payload and an empty string when the payment is good for
// the terms, or the reason it is not.
func (m *MockFacilitator) check(payment Payment, terms Requirements) (MockPayload, string) {
	var payload MockPayload
	if err := json.Unmarshal(payment.Payload, &payload); err != nil {
		return payload, "payment payload is not a mock authorisation"
	}
	if payment.X402Version != Version {
		return payload, fmt.Sprintf("unsupported x402 version %d", payment.X402Version)
	}
	if payment.Scheme != terms.Scheme || payment.Network != terms.Network {
		return payload, fmt.Sprintf("payment is %s on %s, the terms are %s on %s",
			payment.Scheme, payment.Network, terms.Scheme, terms.Network)
	}
	if strings.TrimSpace(payload.From) == "" {
		return payload, "payment does not say who is paying"
	}
	if strings.TrimSpace(payload.Nonce) == "" {
		return payload, "payment authorisation has no nonce"
	}
	if payload.Asset != terms.Asset {
		return payload, fmt.Sprintf("payment is in asset %s, the terms ask for %s", payload.Asset, terms.Asset)
	}
	if payload.PayTo != terms.PayTo {
		return payload, fmt.Sprintf("payment pays %s, the terms pay %s", payload.PayTo, terms.PayTo)
	}
	amount, err := strconv.ParseInt(payload.Amount, 10, 64)
	if err != nil || amount < 0 {
		return payload, fmt.Sprintf("amount %q is not an integer in the asset's own units", payload.Amount)
	}
	required, err := strconv.ParseInt(terms.MaxAmountRequired, 10, 64)
	if err != nil {
		return payload, fmt.Sprintf("the terms ask for %q, which is not an amount", terms.MaxAmountRequired)
	}
	if amount != required {
		return payload, fmt.Sprintf("payment authorises %d, the terms ask for %d", amount, required)
	}
	if m.Balances[payload.From] < amount {
		return payload, "insufficient funds"
	}
	return payload, ""
}

// Balance reports what an address can still spend.
func (m *MockFacilitator) Balance(address string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Balances[address]
}

// Transfers returns the settled payments, in order.
func (m *MockFacilitator) Transfers() []MockTransfer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MockTransfer(nil), m.transfers...)
}

// Trail returns the calls made, for a readable assertion in one line.
func (m *MockFacilitator) Trail() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.calls, ",")
}
