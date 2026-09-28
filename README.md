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

## Pricing per route

A gate in front of one resource can carry its terms directly. A gate in front of
several declares a price per route pattern instead:

```go
gate := &paygate402.Gate{
	Prices: &paygate402.Prices{
		Terms: paygate402.Requirements{
			Scheme:            "exact",
			Network:           "base",
			PayTo:             merchantAddress,
			Asset:             usdcAddress,
			MaxTimeoutSeconds: 60,
		},
		Routes: []paygate402.Route{
			{Pattern: "/report", Amount: "10000", Description: "One generated report"},
			{Pattern: "/reports/{id}", Amount: "2000"},
			{Pattern: "POST /jobs", Amount: "50000"},
		},
		BaseURL: "https://api.example.com",
	},
	Facilitator: facilitator,
}

http.Handle("/", gate.Handler(api))
```

`Terms` is what does not change from route to route — the scheme, the network,
the asset, the recipient — and each route supplies the amount. `Accepts` and
`Prices` are two ways of saying the same thing, so setting both is refused
rather than settled by a precedence rule nobody would remember.

The patterns are matched by an `http.ServeMux`, so method, wildcards and
precedence are the standard library's rather than a second dialect invented
here: `/reports/summary` beats `/reports/{id}` for the same reason it would in
any Go server. A malformed or repeated pattern is the operator's mistake and
comes back as a `500`, like the rest of this gate's misconfiguration.

**A route the table does not mention is served free.** The table is the whole
statement of what is sold here, so a path missing from it is unpriced rather
than closed — a health check needs no line saying it costs nothing.

The resource each `402` names is `BaseURL` with the request's path appended, or
the route's own `Resource` when it has one. With neither, the request's own
scheme and host are used — which is wrong behind a proxy that terminates TLS,
because the scheme a request arrived under is not the scheme the client used.

## A free tier

```go
gate.Free = &paygate402.Allowance{Requests: 5, Every: 24 * time.Hour}
```

Each client gets five requests before any payment is asked for, and every free
response says how many are left:

```console
X-PAYMENT-ALLOWANCE: 4
```

That header is this package's own, not one x402 defines: a client that knows
when the free tier runs out can have a payment ready instead of learning about
it from a refusal.

The allowance is spent before the `X-PAYMENT` header is even read, so a client
that pays while it still has free requests is not charged — the payment is left
unspent rather than taken. A free request that produced nothing is given back,
for the same reason work that failed is never charged for.

`Every` is the period the count resets over; zero makes the allowance a lifetime
one. Clients are told apart by the address the request came from. Forwarded
headers are not read, because anyone can set one and a free tier that counts a
header the client controls counts nothing. Behind a proxy, give `Allowance` a
`Client` function that reads the header the proxy itself sets — or key the tier
on an API key, which is a client saying who it is on purpose.

It is a courtesy rather than a limit. Before anyone has paid, the only thing a
request carries is the address it came from, and a new address is a new
allowance; it exists so that an agent can try an endpoint before paying for it.
The tally lives in this process unless `Counts` is set, and shares
`MemoryStore`'s limit: several replicas mean several allowances.

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

## Testing without a chain

Verification goes through the `Facilitator` interface, so nothing here needs a
chain to be exercised. Two stand-ins come with the package.

`StaticFacilitator` answers from a fixed script and never looks at the payment.
That is what a test about ordering, status codes or replay wants: the verdict is
the fixture.

`MockFacilitator` behaves like a facilitator instead. It keeps balances in
memory, reads the payload it defines, and checks the payer, the amount, the
asset and the recipient — the fields a web layer cannot honestly check — before
moving mock money.

```go
facilitator := &paygate402.MockFacilitator{
	Balances: map[string]int64{"0xc0ffee": 50_000},
}

payment, err := paygate402.MockPayment(terms, "0xc0ffee")
header, err := paygate402.EncodePayment(payment)
// …send that header through the gate, then:
facilitator.Balance("0xc0ffee") // 40_000, and the merchant has the charge
```

It signs nothing and verifies no signature, so it proves the plumbing rather
than the cryptography. That is the point of the seam: the cryptography lives on
the other side of it.

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

A spent payment does not have to be remembered forever. It has to be remembered
until the offer behind it stops standing, plus a margin for the difference
between the clock that stamped the quote and the clock that reads it:
`ReplayWindowFor(quote, time.Now(), 0)` is that arithmetic. Shorter leaves a
window open; longer only makes the store grow.

The default store lives in the process. That is right for one instance and
wrong for several — with more than one replica a payment could be replayed once
per replica — so `SeenStore` is an interface and a shared implementation drops
straight in.

`FileStore` puts the same store in a file, so a restart does not hand a captured
header a second chance. It is an append-only log, rewritten when the expired
entries outnumber the live ones, and entries whose window has closed are dropped
on load as well as on every write.

```go
store, err := paygate402.NewFileStore("/var/lib/paygate402/seen")
defer store.Close()
gate.Seen = store
```

A store that cannot write goes on refusing replays from memory — a full disk
must not open the gate — and says so through `store.Err()`, because the
protection has quietly become the weaker one. One file is still one machine.

## Quotes

A quote is the priced offer behind a `402`: an amount, an asset, the moment the
offer stops standing, and a nonce that makes it one of a kind. It is signed with
this server's own key, so an offer that comes back can be checked against what
was actually offered rather than against what a client says was offered.

```go
signer := &paygate402.QuoteSigner{Key: secret, TTL: time.Minute}

quote, err := signer.Issue(terms)
// …later, with the quote a client returned:
err = signer.Verify(quote) // the signature first, then the expiry
```

The signature covers a canonical form rather than the JSON: a domain tag, then
every set field in a fixed order, each part written as its length followed by
its bytes. Lengths rather than separators mean no value can be read as two
fields, and a field left empty is not written at all — so a field added in a
later version leaves the signed bytes of an older quote exactly as they were,
and signatures issued before it keep verifying. The expiry is signed as whole
Unix seconds.

That signature is this server's, not a chain's. It says "these were my terms",
and nothing about whether anyone paid.

### Quotes on the gate

Giving the gate a signer makes it offer a quote with every `402` and check the
one that comes back:

```go
gate.Quotes = &paygate402.QuoteSigner{Key: secret, TTL: time.Minute}
```

The `402` then carries one `X-PAYMENT-QUOTE` header per accepted term — base64
of the signed quote, in the order of `accepts`. The client repeats the request
with `X-PAYMENT` and the `X-PAYMENT-QUOTE` it chose, and the gate checks three
things before the facilitator is asked anything: that this server signed the
offer, that the offer has not run out, and that it is the offer for the terms
being paid for. A signature alone would only say the quote was issued here, not
that it was issued for what is being bought now.

With a signer in place the replay window stops being a fixed hour and becomes
the offer's own: a payment is remembered until the quote behind it could no
longer be presented, plus the safety margin.

The gate without a signer is unchanged — it takes any payment that matches its
terms, which is the right shape when prices are static and a stale offer costs
nothing.

## Status

Early, and pinned to a moving target: x402 is young, and this speaks
`x402Version: 1`. A payload announcing any other version is refused with a clear
message rather than interpreted hopefully.

| | |
|---|---|
| Implemented | 402 challenge, `X-PAYMENT` codec, terms matching, a price per route pattern, a free tier per client, replay protection in memory or in a file, facilitator client, a mock facilitator with in-memory balances, settlement ordering, payer on the context, signed quotes with a canonical serialisation, quotes carried on the 402 and checked on the way back |
| Delegated | signature verification, allowance checks, settlement — all to the facilitator |
| Not yet | multiple concurrent schemes per resource, a price that depends on more of the request than its route, a replay store or free-tier counter shared between replicas, `X-PAYMENT-RESPONSE` verification on the client side |

Go 1.24 or newer. No dependencies outside the standard library.

## Development

```bash
make test   # go vet + go test ./...
make demo   # the transcript above
```

## License

MIT

Built by [polycratia](https://polycratia.com).
