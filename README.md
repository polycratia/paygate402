# paygate402

An x402 payment gate for an HTTP handler.

The protocol is short. An unpaid request comes back as `402` with the terms the
server accepts; the client repeats it with an `X-PAYMENT` header; the server has
that payment verified and settled, and only then serves the response.

```console
$ go run ./example
--- without payment ---
status: 402
body: {"x402Version":1,"error":"payment required","accepts":[{"scheme":"exact","network":"base","maxAmountRequired":"10000","resource":"https://api.example.com/report",…}]}

--- with payment ---
status: 200
X-PAYMENT-RESPONSE: eyJzdWNjZXNzIjp0cnVlLCJ0cmFuc2FjdGlvbiI6IjB4ZGVhZGJlZWYiLCJuZXR3b3JrIjoiYmFzZSJ9
body: {"report":"quarterly numbers","paidBy":"0xc0ffee"}

--- replaying the same payment ---
status: 402
body: {"x402Version":1,"error":"this payment has already been used",…}
```

## Use

```go
gate := &paygate402.Gate{
	Accepts: []paygate402.Requirements{{
		Scheme:            "exact",
		Network:           "base",
		MaxAmountRequired: "10000",
		Resource:          "https://api.example.com/report",
		PayTo:             merchantAddress,
		Asset:             usdcAddress,
		MaxTimeoutSeconds: 60,
	}},
	Facilitator: &paygate402.HTTPFacilitator{BaseURL: "https://facilitator.example.com"},
}

http.Handle("/report", gate.Handler(reportHandler))
```

Inside the handler, `paygate402.Payer(r.Context())` gives the address the
facilitator said paid — its claim, not this package's finding.

## Where the boundary is

**This package does not check signatures and does not move money.** In x402
that is a facilitator's job: it reads the scheme-specific payload, recovers the
signature, checks the allowance, and submits the transfer. That boundary is
honoured here rather than blurred. A middleware that decoded some base64 and
called it verification would be worse than no middleware, because it would look
like a paywall while charging nobody.

So the payment payload stays opaque, and matching compares only what a web layer
can honestly compare: the scheme and the network. The amount, the asset and the
recipient are checked by the component that can read the payload.

## The two orderings that matter

**Settle after the handler, serve after settlement.** The handler runs into a
buffer. If the work fails, nothing is charged; if settlement fails, nothing is
served. A client never receives a paid response for a payment that did not
settle, and never pays for a `503`.

**An unreachable facilitator is a `502`, not a `402`.** "We could not ask" and
"the answer is no" are different outcomes. Returning `402` when the facilitator
is down tells a client to pay a second time for something they may already have
paid for.

## Replay

A captured `X-PAYMENT` header is refused the second time. The replay key is a
digest of the whole payload rather than a nonce field, because every scheme
carries its own payload shape and reaching into one to find "the nonce" breaks
as soon as a new scheme appears.

The key is recorded only after verification passes. A payment the facilitator
rejected never touched the chain, and the client who fixes their allowance may
legitimately resend the same signed payload. Once settlement has been attempted
the payment stays spent, even on failure — the ambiguous case must never risk a
double charge.

The default store lives in the process. That is right for one instance and
wrong for several — with more than one replica a payment could be replayed once
per replica — so `SeenStore` is an interface and a shared implementation drops
straight in.

## Status

Early, and pinned to a moving target: x402 is young, and this speaks
`x402Version: 1`. A payload announcing any other version is refused with a clear
message rather than interpreted hopefully.

| | |
|---|---|
| Implemented | 402 challenge, `X-PAYMENT` codec, terms matching, replay protection, facilitator client, settlement ordering, payer on the context |
| Delegated | signature verification, allowance checks, settlement — all to the facilitator |
| Not yet | multiple concurrent schemes per resource, dynamic pricing per request, a shared replay store, `X-PAYMENT-RESPONSE` verification on the client side |

Go 1.24 or newer. No dependencies outside the standard library.

## Development

```bash
make test   # go vet + go test ./...
make demo   # the transcript above
```

## License

MIT
