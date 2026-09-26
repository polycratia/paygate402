package paygate402

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AllowanceHeader reports how many free requests a client has left after the
// one it just made. It is this package's own header, not one x402 defines: a
// client that knows when the free tier runs out can have a payment ready
// instead of learning about it from a refusal.
const AllowanceHeader = "X-PAYMENT-ALLOWANCE"

// ErrNoPrice means the pricing table says nothing about this route.
var ErrNoPrice = errors.New("no price is declared for this route")

// Route is one line of a pricing table: what a pattern costs.
type Route struct {
	// Pattern is an http.ServeMux pattern — "/report", "/reports/{id}",
	// "POST /jobs", or "/" for everything else. Required.
	Pattern string
	// Amount is the price, in the asset's own units. Required.
	Amount string
	// Description travels on the 402, for a client that shows a human what it
	// is about to pay for.
	Description string
	// Resource, when set, is the URL the 402 names instead of the one the
	// request arrived at.
	Resource string
}

// Prices is a pricing table: the terms everything on this server is sold on,
// and what each route costs.
//
// The patterns are matched by an http.ServeMux, so method, wildcards and
// precedence are the standard library's rather than a second dialect invented
// here: "/reports/summary" beats "/reports/{id}" for the same reason it would
// in any Go server.
type Prices struct {
	// Terms is the template each price is filled into: the scheme, the network,
	// the asset, the recipient — everything that does not change from route to
	// route. The amount and the resource come from the route.
	Terms Requirements
	// Routes lists what is priced. A route that is not here is not priced, and
	// the gate serves it without payment.
	Routes []Route
	// BaseURL, when set, is the origin the resources are addressed by, with the
	// request's path appended. Behind a proxy that terminates TLS the scheme a
	// request arrived under is not the scheme the client used, so set it there.
	BaseURL string

	once sync.Once
	mux  *http.ServeMux
	err  error
}

// pricedRoute carries a price through the mux. It is never served: the mux is
// used for the matching it already does well, not for its dispatch.
type pricedRoute struct{ route Route }

func (pricedRoute) ServeHTTP(http.ResponseWriter, *http.Request) {}

// Price returns the terms for a request. A route the table does not mention
// comes back as ErrNoPrice, which is not a failure: it is a route that costs
// nothing.
func (p *Prices) Price(r *http.Request) (Requirements, error) {
	p.once.Do(p.compile)
	if p.err != nil {
		return Requirements{}, p.err
	}
	handler, _ := p.mux.Handler(r)
	priced, ok := handler.(pricedRoute)
	if !ok {
		return Requirements{}, fmt.Errorf("%w: %s", ErrNoPrice, r.URL.Path)
	}

	terms := p.Terms
	terms.MaxAmountRequired = priced.route.Amount
	if priced.route.Description != "" {
		terms.Description = priced.route.Description
	}
	terms.Resource = p.resource(priced.route, r)
	return terms, terms.Validate()
}

func (p *Prices) compile() {
	if len(p.Routes) == 0 {
		p.err = errors.New("paygate402: the pricing table has no routes")
		return
	}
	mux := http.NewServeMux()
	for _, route := range p.Routes {
		if strings.TrimSpace(route.Pattern) == "" {
			p.err = errors.New("paygate402: a price was declared without a route pattern")
			return
		}
		if err := register(mux, route); err != nil {
			p.err = err
			return
		}
	}
	p.mux = mux
}

// register adds one route, turning the mux's panic on a bad or repeated pattern
// into an error. A pricing table is configuration, and wrong configuration
// should fail the way the rest of this gate's does — as the operator's mistake,
// not the client's.
func register(mux *http.ServeMux, route Route) (err error) {
	defer func() {
		if problem := recover(); problem != nil {
			err = fmt.Errorf("paygate402: route %q: %v", route.Pattern, problem)
		}
	}()
	mux.Handle(route.Pattern, pricedRoute{route: route})
	return nil
}

func (p *Prices) resource(route Route, r *http.Request) string {
	switch {
	case route.Resource != "":
		return route.Resource
	case p.BaseURL != "":
		joined, err := url.JoinPath(p.BaseURL, r.URL.Path)
		if err != nil {
			return p.BaseURL
		}
		return joined
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.Path
}

// AllowanceStore holds how much of its free tier each client has used.
//
// It shares MemoryStore's limit: a counter in one process is one process's
// count, and several replicas mean several allowances. That is why it is an
// interface.
type AllowanceStore interface {
	// Take consumes one free request and reports whether there was one to take
	// and how many are left after it.
	Take(client string, limit int, window time.Duration) (bool, int)
	// Return gives one back, for work that was not delivered.
	Return(client string)
}

// Allowance is a free tier: the requests a client may make before the gate
// starts asking for payment.
//
// It is a courtesy rather than a limit. Before anyone has paid, the only thing
// a request carries is the address it came from, and a new address is a new
// allowance. It exists so that an agent can try an endpoint before paying for
// it, and it is worth what a fresh address costs.
type Allowance struct {
	// Requests is how many a client gets. Zero means no free tier at all.
	Requests int
	// Every is the period the count resets over. Zero means it never resets: the
	// allowance is a lifetime one.
	Every time.Duration
	// Client names who is being counted. ClientIP is used when it is nil.
	Client func(*http.Request) string
	// Counts holds the tally. A counter in this process is used when it is nil.
	Counts AllowanceStore

	once     sync.Once
	fallback *AllowanceCounter
}

// take spends one free request, and reports who spent it and what is left.
func (a *Allowance) take(r *http.Request) (string, int, bool) {
	if a == nil || a.Requests <= 0 {
		return "", 0, false
	}
	client := a.client(r)
	if client == "" {
		return "", 0, false
	}
	granted, left := a.counts().Take(client, a.Requests, a.Every)
	return client, left, granted
}

// give hands a free request back.
func (a *Allowance) give(client string) {
	if a == nil || client == "" {
		return
	}
	a.counts().Return(client)
}

func (a *Allowance) client(r *http.Request) string {
	if a.Client != nil {
		return a.Client(r)
	}
	return ClientIP(r)
}

func (a *Allowance) counts() AllowanceStore {
	if a.Counts != nil {
		return a.Counts
	}
	a.once.Do(func() { a.fallback = NewAllowanceCounter() })
	return a.fallback
}

// ClientIP identifies a client by the address its request came from.
//
// Forwarded headers are not read: anyone can set one, and a free tier that
// counts a header the client controls counts nothing. Behind a proxy, give
// Allowance a Client function that reads the header the proxy itself sets — or
// key the tier on an API key, which is a client saying who it is on purpose.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// AllowanceCounter keeps the free tier's tally in this process.
type AllowanceCounter struct {
	mu   sync.Mutex
	used map[string]*allowanceEntry
	now  func() time.Time
}

type allowanceEntry struct {
	used   int
	resets time.Time
}

// NewAllowanceCounter returns an empty counter.
func NewAllowanceCounter() *AllowanceCounter {
	return &AllowanceCounter{used: map[string]*allowanceEntry{}, now: time.Now}
}

// Take consumes one free request and reports whether there was one to take.
func (c *AllowanceCounter) Take(client string, limit int, window time.Duration) (bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	c.sweep(now)

	entry := c.used[client]
	if entry == nil || (window > 0 && !now.Before(entry.resets)) {
		entry = &allowanceEntry{}
		if window > 0 {
			entry.resets = now.Add(window)
		}
		c.used[client] = entry
	}
	if entry.used >= limit {
		return false, 0
	}
	entry.used++
	return true, limit - entry.used
}

// Return gives one free request back.
func (c *AllowanceCounter) Return(client string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.used[client]; entry != nil && entry.used > 0 {
		entry.used--
	}
}

// sweep drops the clients whose period has closed. It runs on write, on the
// requests that matter, so no background goroutine has to exist. A lifetime
// allowance has no period and is never swept — that is what makes it a
// lifetime one.
func (c *AllowanceCounter) sweep(now time.Time) {
	for client, entry := range c.used {
		if !entry.resets.IsZero() && now.After(entry.resets) {
			delete(c.used, client)
		}
	}
}
