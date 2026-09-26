package paygate402

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const someClient = "192.0.2.10:54321"

// priceTemplate is terms() with the two fields a route supplies taken out.
func priceTemplate() Requirements {
	template := terms()
	template.MaxAmountRequired = ""
	template.Resource = ""
	return template
}

func prices() *Prices {
	return &Prices{
		Terms: priceTemplate(),
		Routes: []Route{
			{Pattern: "/report", Amount: "10000"},
			{Pattern: "/reports/{id}", Amount: "2500", Description: "One stored report"},
			{Pattern: "/reports/summary", Amount: "50000"},
		},
	}
}

func pricedGate(f Facilitator) *Gate {
	return &Gate{Prices: prices(), Facilitator: f, Seen: NewMemoryStore()}
}

func serve(t *testing.T, g *Gate, next http.Handler, path, paymentHeader, from string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://api.example.com"+path, nil)
	r.RemoteAddr = from
	if paymentHeader != "" {
		r.Header.Set(PaymentHeader, paymentHeader)
	}
	w := httptest.NewRecorder()
	g.Handler(next).ServeHTTP(w, r)
	return w
}

func visitFrom(t *testing.T, g *Gate, path, paymentHeader, from string) *httptest.ResponseRecorder {
	t.Helper()
	return serve(t, g, paidHandler(), path, paymentHeader, from)
}

func visit(t *testing.T, g *Gate, path, paymentHeader string) *httptest.ResponseRecorder {
	t.Helper()
	return visitFrom(t, g, path, paymentHeader, someClient)
}

func offeredTerms(t *testing.T, response *httptest.ResponseRecorder) Requirements {
	t.Helper()
	if response.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402; body %s", response.Code, response.Body)
	}
	var challenge Challenge
	if err := json.Unmarshal(response.Body.Bytes(), &challenge); err != nil {
		t.Fatalf("challenge is not JSON: %v", err)
	}
	if len(challenge.Accepts) != 1 {
		t.Fatalf("the challenge offered %d sets of terms, want the one this route is priced at",
			len(challenge.Accepts))
	}
	return challenge.Accepts[0]
}

func TestEachRouteIsChargedAtItsOwnPrice(t *testing.T) {
	g := pricedGate(&StaticFacilitator{})

	// The literal beats the wildcard, as it would in any Go server.
	for path, want := range map[string]string{
		"/report":          "10000",
		"/reports/17":      "2500",
		"/reports/summary": "50000",
	} {
		t.Run(path, func(t *testing.T) {
			offered := offeredTerms(t, visit(t, g, path, ""))
			if offered.MaxAmountRequired != want {
				t.Errorf("price = %q, want %q", offered.MaxAmountRequired, want)
			}
			if offered.Resource != "https://api.example.com"+path {
				t.Errorf("resource = %q, want the URL the request arrived at", offered.Resource)
			}
			if offered.PayTo != terms().PayTo || offered.Asset != terms().Asset {
				t.Errorf("terms = %+v, want the table's common terms filled in", offered)
			}
		})
	}

	if got := offeredTerms(t, visit(t, g, "/reports/17", "")).Description; got != "One stored report" {
		t.Errorf("description = %q, want the route's own", got)
	}
}

// The table is the whole statement of what is sold here, so a route missing
// from it is free rather than closed.
func TestARouteWithNoPriceIsNotGated(t *testing.T) {
	facilitator := &StaticFacilitator{}
	response := visit(t, pricedGate(facilitator), "/health", "")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want the unpriced route served; body %s", response.Code, response.Body)
	}
	if facilitator.Trail() != "" {
		t.Errorf("calls = %q: an unpriced route was put to the facilitator", facilitator.Trail())
	}
}

func TestAPaymentForAPricedRouteIsServed(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: true, Payer: "0xabc"},
		Settlement:   Settlement{Success: true, Transaction: "0xdeadbeef"},
	}
	response := visit(t, pricedGate(facilitator), "/reports/17", header(t, payment("1")))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", response.Code, response.Body)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want verify,settle", facilitator.Trail())
	}
}

func TestABaseURLNamesTheResourceInsteadOfTheRequest(t *testing.T) {
	g := pricedGate(&StaticFacilitator{})
	g.Prices.BaseURL = "https://api.example.com/v1"

	got := offeredTerms(t, visit(t, g, "/report", "")).Resource
	if got != "https://api.example.com/v1/report" {
		t.Errorf("resource = %q, want it under the declared origin", got)
	}
}

func TestABrokenPricingTableIsTheOperatorsMistake(t *testing.T) {
	cases := map[string]*Prices{
		"no routes":                 {Terms: priceTemplate()},
		"a route without a pattern": {Terms: priceTemplate(), Routes: []Route{{Amount: "10000"}}},
		"a route without a price":   {Terms: priceTemplate(), Routes: []Route{{Pattern: "/report"}}},
		"the same pattern twice": {Terms: priceTemplate(), Routes: []Route{
			{Pattern: "/report", Amount: "1"},
			{Pattern: "/report", Amount: "2"},
		}},
	}
	for name, table := range cases {
		t.Run(name, func(t *testing.T) {
			g := &Gate{Prices: table, Facilitator: &StaticFacilitator{}, Seen: NewMemoryStore()}
			if response := visit(t, g, "/report", ""); response.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500: this is the operator's mistake, not the client's", response.Code)
			}
		})
	}
}

func TestAGateCannotBothListAndPriceItsTerms(t *testing.T) {
	g := &Gate{Accepts: []Requirements{terms()}, Prices: prices(), Facilitator: &StaticFacilitator{}}
	response := visit(t, g, "/report", "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	if !strings.Contains(response.Body.String(), "not both") {
		t.Errorf("the operator was not told what is wrong: %s", response.Body)
	}
}

// The free tier is spent before anything else happens: a client that does not
// have to pay yet is not asked to, and the facilitator is not troubled.
func TestAFreeAllowanceIsSpentBeforeAnyChargeBegins(t *testing.T) {
	facilitator := &StaticFacilitator{
		Verification: Verification{Valid: true},
		Settlement:   Settlement{Success: true},
	}
	g := pricedGate(facilitator)
	g.Free = &Allowance{Requests: 2}

	for spent := 1; spent <= 2; spent++ {
		response := visit(t, g, "/report", "")
		if response.Code != http.StatusOK {
			t.Fatalf("free request %d: status = %d, body %s", spent, response.Code, response.Body)
		}
		if got, want := response.Header().Get(AllowanceHeader), strconv.Itoa(2-spent); got != want {
			t.Errorf("%s = %q, want %q left", AllowanceHeader, got, want)
		}
	}
	if facilitator.Trail() != "" {
		t.Errorf("calls = %q: a free request was put to the facilitator", facilitator.Trail())
	}

	if exhausted := visit(t, g, "/report", ""); exhausted.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 once the allowance is gone", exhausted.Code)
	}
	if paid := visit(t, g, "/report", header(t, payment("1"))); paid.Code != http.StatusOK {
		t.Fatalf("paid request: status = %d, body %s", paid.Code, paid.Body)
	}
	if facilitator.Trail() != "verify,settle" {
		t.Errorf("calls = %q, want the paid request verified and settled", facilitator.Trail())
	}
}

func TestTheAllowanceIsCountedPerClient(t *testing.T) {
	g := pricedGate(&StaticFacilitator{})
	g.Free = &Allowance{Requests: 1}

	if first := visitFrom(t, g, "/report", "", "192.0.2.10:1"); first.Code != http.StatusOK {
		t.Fatalf("status = %d, want the first request served free", first.Code)
	}
	if again := visitFrom(t, g, "/report", "", "192.0.2.10:2"); again.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want the same client charged from its second request", again.Code)
	}
	if other := visitFrom(t, g, "/report", "", "192.0.2.11:1"); other.Code != http.StatusOK {
		t.Errorf("status = %d, want another client to have its own allowance", other.Code)
	}
}

// Work that failed is not charged for, and a free request is a charge like any
// other.
func TestAFreeRequestIsGivenBackWhenTheWorkFails(t *testing.T) {
	g := pricedGate(&StaticFacilitator{})
	g.Free = &Allowance{Requests: 1}

	broken := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream is down", http.StatusServiceUnavailable)
	})
	failed := serve(t, g, broken, "/report", "", someClient)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want the handler's own failure to pass through", failed.Code)
	}
	if retried := visit(t, g, "/report", ""); retried.Code != http.StatusOK {
		t.Errorf("status = %d, want the free request back after work that produced nothing", retried.Code)
	}
}

func TestAnAllowanceCanBeKeyedOnSomethingOtherThanAnAddress(t *testing.T) {
	g := pricedGate(&StaticFacilitator{})
	g.Free = &Allowance{
		Requests: 1,
		Client:   func(r *http.Request) string { return r.Header.Get("authorization") },
	}

	keyed := func(key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil)
		r.Header.Set("authorization", key)
		w := httptest.NewRecorder()
		g.Handler(paidHandler()).ServeHTTP(w, r)
		return w
	}

	if first := keyed("key-one"); first.Code != http.StatusOK {
		t.Fatalf("status = %d, want the first request on this key served free", first.Code)
	}
	if again := keyed("key-one"); again.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want the second request on the same key charged", again.Code)
	}
	if other := keyed("key-two"); other.Code != http.StatusOK {
		t.Errorf("status = %d, want another key to have its own allowance", other.Code)
	}
}

func TestAnAllowanceRefillsWhenItsPeriodIsOver(t *testing.T) {
	counter := NewAllowanceCounter()
	clock := time.Unix(1800000000, 0)
	counter.now = func() time.Time { return clock }

	g := pricedGate(&StaticFacilitator{})
	g.Free = &Allowance{Requests: 1, Every: time.Hour, Counts: counter}

	if first := visit(t, g, "/report", ""); first.Code != http.StatusOK {
		t.Fatalf("status = %d, want the free request served", first.Code)
	}
	if second := visit(t, g, "/report", ""); second.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 inside the same period", second.Code)
	}
	clock = clock.Add(2 * time.Hour)
	if later := visit(t, g, "/report", ""); later.Code != http.StatusOK {
		t.Errorf("status = %d, want the allowance back in the next period", later.Code)
	}
}

// A lifetime allowance has no period, so nothing but a return gives it back.
func TestALifetimeAllowanceNeverRefillsOnItsOwn(t *testing.T) {
	counter := NewAllowanceCounter()
	clock := time.Unix(1800000000, 0)
	counter.now = func() time.Time { return clock }

	if granted, left := counter.Take("client", 1, 0); !granted || left != 0 {
		t.Fatalf("take = %v, %d; want the one free request granted and none left", granted, left)
	}
	clock = clock.Add(100 * time.Hour)
	if granted, _ := counter.Take("client", 1, 0); granted {
		t.Error("a lifetime allowance came back after a while")
	}
	counter.Return("client")
	if granted, _ := counter.Take("client", 1, 0); !granted {
		t.Error("a returned free request was not there to take again")
	}
}

// The free tier belongs to the gate, not to the pricing table: a gate with
// fixed terms has one too.
func TestAGateWithFixedTermsCanAlsoHaveAFreeTier(t *testing.T) {
	facilitator := &StaticFacilitator{}
	g := gate(facilitator)
	g.Free = &Allowance{Requests: 1}

	if first := request(t, g, ""); first.Code != http.StatusOK {
		t.Fatalf("status = %d, want the free request served", first.Code)
	}
	if second := request(t, g, ""); second.Code != http.StatusPaymentRequired {
		t.Errorf("status = %d, want 402 once the allowance is gone", second.Code)
	}
	if facilitator.Trail() != "" {
		t.Errorf("calls = %q: a free request was put to the facilitator", facilitator.Trail())
	}
}

func TestClientIPIsTheAddressWithoutThePort(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://api.example.com/report", nil)
	r.RemoteAddr = someClient
	if got := ClientIP(r); got != "192.0.2.10" {
		t.Errorf("client = %q, want the address without the port", got)
	}
	r.RemoteAddr = "not-an-address"
	if got := ClientIP(r); got != "not-an-address" {
		t.Errorf("client = %q, want the address as it stands when it has no port", got)
	}
}
