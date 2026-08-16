// Command example runs a paid endpoint against a stand-in facilitator, so the
// whole flow can be watched without a chain or an account anywhere.
//
//	go run ./example
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/polycratia/paygate402"
)

func main() {
	gate := &paygate402.Gate{
		Accepts: []paygate402.Requirements{{
			Scheme:            "exact",
			Network:           "base",
			MaxAmountRequired: "10000", // 0.01 USDC, in the asset's own units
			Resource:          "https://api.example.com/report",
			Description:       "One generated report",
			MimeType:          "application/json",
			PayTo:             "0x0000000000000000000000000000000000000001",
			Asset:             "0x0000000000000000000000000000000000000002",
			MaxTimeoutSeconds: 60,
		}},
		// A real deployment points HTTPFacilitator at a facilitator service.
		Facilitator: &paygate402.StaticFacilitator{
			Verification: paygate402.Verification{Valid: true, Payer: "0xc0ffee"},
			Settlement: paygate402.Settlement{
				Success: true, Transaction: "0xdeadbeef", Network: "base",
			},
		},
	}

	report := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payer, _ := paygate402.Payer(r.Context())
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"report":"quarterly numbers","paidBy":%q}`, payer)
	})

	server := httptest.NewServer(gate.Handler(report))
	defer server.Close()

	fmt.Println("--- without payment ---")
	show(request(server.URL, ""))

	fmt.Println("\n--- with payment ---")
	header, _ := paygate402.EncodePayment(paygate402.Payment{
		X402Version: paygate402.Version,
		Scheme:      "exact",
		Network:     "base",
		Payload:     []byte(`{"signature":"0x…","authorization":{"nonce":"01"}}`),
	})
	show(request(server.URL, header))

	fmt.Println("\n--- replaying the same payment ---")
	show(request(server.URL, header))
}

func request(url, payment string) *http.Response {
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if payment != "" {
		req.Header.Set(paygate402.PaymentHeader, payment)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	return response
}

func show(response *http.Response) {
	defer response.Body.Close()
	body := make([]byte, 512)
	n, _ := response.Body.Read(body)
	fmt.Printf("status: %d\n", response.StatusCode)
	if settlement := response.Header.Get(paygate402.ResponseHeader); settlement != "" {
		fmt.Printf("%s: %s\n", paygate402.ResponseHeader, settlement)
	}
	fmt.Printf("body: %s\n", body[:n])
}
