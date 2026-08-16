package paygate402

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The client is tested against a stand-in facilitator served in-process: the
// requests it makes are real HTTP, and their shape is what a facilitator sees.
func TestHTTPFacilitatorSpeaksTheExpectedShape(t *testing.T) {
	var seen facilitatorRequest
	var path, auth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		auth = r.Header.Get("authorization")
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("facilitator received unreadable JSON: %v", err)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"isValid":true,"payer":"0xabc"}`))
	}))
	defer server.Close()

	facilitator := &HTTPFacilitator{BaseURL: server.URL, Authorization: "Bearer token"}
	verification, err := facilitator.Verify(context.Background(), payment("1"), terms())
	if err != nil {
		t.Fatal(err)
	}

	if !verification.Valid || verification.Payer != "0xabc" {
		t.Errorf("verification = %+v", verification)
	}
	if path != "/verify" {
		t.Errorf("path = %q, want /verify", path)
	}
	if auth != "Bearer token" {
		t.Errorf("authorization = %q, want it passed through", auth)
	}
	if seen.X402Version != Version || seen.PaymentRequirements.PayTo != terms().PayTo {
		t.Errorf("request body = %+v, want the payment and the terms together", seen)
	}
}

func TestHTTPFacilitatorSettles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/settle") {
			t.Errorf("path = %q, want /settle", r.URL.Path)
		}
		w.Write([]byte(`{"success":true,"transaction":"0xfeed","network":"base"}`))
	}))
	defer server.Close()

	facilitator := &HTTPFacilitator{BaseURL: server.URL}
	settlement, err := facilitator.Settle(context.Background(), payment("1"), terms())
	if err != nil {
		t.Fatal(err)
	}
	if !settlement.Success || settlement.Transaction != "0xfeed" {
		t.Errorf("settlement = %+v", settlement)
	}
}

// A facilitator that answers with an error status must not be read as a
// verdict: "we could not ask" and "the answer is no" are different outcomes and
// the gate treats them differently.
func TestHTTPFacilitatorReportsBadStatusAsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream exploded", http.StatusInternalServerError)
	}))
	defer server.Close()

	facilitator := &HTTPFacilitator{BaseURL: server.URL}
	if _, err := facilitator.Verify(context.Background(), payment("1"), terms()); err == nil {
		t.Error("a 500 from the facilitator was read as a verdict")
	}
}

func TestRequirementsValidateNamesEveryMissingField(t *testing.T) {
	err := Requirements{Scheme: "exact"}.Validate()
	if err == nil {
		t.Fatal("incomplete terms were accepted")
	}
	for _, field := range []string{"asset", "maxAmountRequired", "network", "payTo", "resource"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error does not mention %q: %v", field, err)
		}
	}
	if err := terms().Validate(); err != nil {
		t.Errorf("complete terms were refused: %v", err)
	}
}

func TestEncodeAndDecodeRoundTrip(t *testing.T) {
	encoded, err := EncodePayment(payment("1"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodePayment(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Scheme != "exact" || string(decoded.Payload) != `{"nonce":"1"}` {
		t.Errorf("decoded = %+v", decoded)
	}

	// Clients that strip base64 padding are still understood.
	if _, err := DecodePayment(strings.TrimRight(encoded, "=")); err != nil {
		t.Errorf("unpadded base64 was refused: %v", err)
	}
}
