package paygate402

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrNoWallet is returned by a Transport that was given nothing to pay with.
var ErrNoWallet = errors.New("paygate402: no wallet configured")

// maxChallenge bounds the 402 body read before anything is paid for. A
// challenge is a small JSON object, and a server that answers a megabyte of
// one is not a server to hand money to.
const maxChallenge = 1 << 20

// Wallet turns terms into a signed payment.
//
// It is on the client side what Facilitator is on the server side: the key that
// authorises a transfer lives behind it, and this package holds none.
type Wallet interface {
	// Pay returns a payment for these terms. The offer is the quote the 402
	// carried for them, or the zero Quote when the server signs none, so a
	// wallet can check the price it is about to authorise against what the
	// server put its signature on.
	Pay(ctx context.Context, terms Requirements, offer Quote) (Payment, error)
}

// Transport answers one 402 with one payment.
//
// On a 402 it asks the wallet for a payment for the terms the challenge named,
// repeats the request with X-PAYMENT and the X-PAYMENT-QUOTE that offer came
// on, and returns the second response. That is the whole of it: one payment,
// one retry. A 402 answering the retry comes back as it is rather than being
// paid for a second time, and a request that already carries a payment is not
// paid for at all — it has been paid for once already.
//
// A request whose body cannot be resent is never paid for. The retry is the
// same request again, so it needs the same body, and a bare stream has no
// second copy — net/http says so by leaving GetBody nil. That 402 is handed
// back untouched, terms and all, so a caller can buffer the body and pay
// deliberately. Paying first would buy a response for a request that can no
// longer be made.
type Transport struct {
	// Wallet signs the payments. Required.
	Wallet Wallet
	// Base is the transport underneath; http.DefaultTransport when nil.
	Base http.RoundTripper
	// Settled, when set, receives the X-PAYMENT-RESPONSE of a paid response,
	// decoded. A header that is absent or unreadable is not reported through it;
	// DecodeSettlement reads it for a caller who wants to see either.
	Settled func(Settlement)
}

// RoundTrip pays for one 402 and retries the request once.
func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.Wallet == nil {
		return nil, ErrNoWallet
	}

	response, err := t.base().RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusPaymentRequired {
		return response, err
	}
	if request.Header.Get(PaymentHeader) != "" || !resendable(request) {
		return response, nil
	}

	challenge, err := readChallenge(response)
	if err != nil {
		// A 402 this transport cannot read the terms off is somebody else's 402.
		return response, nil
	}

	payment, offer, err := t.pay(request.Context(), challenge, response.Header.Values(QuoteHeader))
	if err != nil {
		response.Body.Close()
		return nil, fmt.Errorf("paygate402: this 402 was not paid: %w", err)
	}
	header, err := EncodePayment(payment)
	if err != nil {
		response.Body.Close()
		return nil, err
	}

	retry := request.Clone(request.Context())
	retry.Header.Set(PaymentHeader, header)
	if offer != "" {
		retry.Header.Set(QuoteHeader, offer)
	}
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			response.Body.Close()
			return nil, err
		}
		retry.Body = body
	}
	response.Body.Close()

	paid, err := t.base().RoundTrip(retry)
	if err != nil {
		return nil, err
	}
	if t.Settled != nil {
		if settlement, err := DecodeSettlement(paid.Header.Get(ResponseHeader)); err == nil {
			t.Settled(settlement)
		}
	}
	return paid, nil
}

// pay asks the wallet for a payment, and returns it with the encoded offer it
// answers. The challenge may name several sets of terms, and the offers arrive
// in their order, so the first terms the wallet will pay for decides both.
func (t *Transport) pay(ctx context.Context, challenge Challenge, offers []string) (Payment, string, error) {
	var last error
	for i, terms := range challenge.Accepts {
		var encoded string
		var offer Quote
		if i < len(offers) {
			decoded, err := DecodeQuote(offers[i])
			if err != nil {
				last = err
				continue
			}
			encoded, offer = offers[i], decoded
		}
		payment, err := t.Wallet.Pay(ctx, terms, offer)
		if err != nil {
			last = err
			continue
		}
		return payment, encoded, nil
	}
	return Payment{}, "", last
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

// readChallenge takes the terms off a 402, leaving the body readable for a
// caller that ends up seeing the response after all.
func readChallenge(response *http.Response) (Challenge, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxChallenge))
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return Challenge{}, err
	}
	var challenge Challenge
	if err := json.Unmarshal(body, &challenge); err != nil {
		return Challenge{}, err
	}
	if len(challenge.Accepts) == 0 {
		return Challenge{}, errors.New("the challenge names no terms to pay against")
	}
	return challenge, nil
}

// resendable reports whether this request can be made a second time. A body
// that is a plain reader has no second copy, and net/http says so by leaving
// GetBody nil.
func resendable(request *http.Request) bool {
	return request.Body == nil || request.Body == http.NoBody || request.GetBody != nil
}
