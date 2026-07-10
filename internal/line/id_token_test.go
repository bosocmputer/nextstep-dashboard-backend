package line

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIDTokenVerifierUsesLINEEndpointAndValidatesClaims(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || request.Form.Get("id_token") != "opaque-id-token-value" || request.Form.Get("client_id") != "2010662588" {
			t.Fatalf("unexpected verify request: method=%s form=%v", request.Method, request.Form)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"iss":"https://access.line.me","sub":"U123456789","aud":"2010662588","exp":1783674000,"iat":1783670000,"name":"เจ้าของร้าน","future_property":"allowed"}`))
	}))
	defer server.Close()
	verifier := NewIDTokenVerifier("2010662588", server.URL, 2*time.Second, func() time.Time { return now })

	identity, err := verifier.Verify(context.Background(), "opaque-id-token-value")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if identity.Subject != "U123456789" || identity.DisplayName != "เจ้าของร้าน" {
		t.Fatalf("identity = %+v", identity)
	}
}

func TestIDTokenVerifierRejectsInvalidAudienceIssuerExpiryAndLeakyErrors(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	responses := []string{
		`{"iss":"https://access.line.me","sub":"U1","aud":"wrong","exp":1783674000}`,
		`{"iss":"https://attacker.example","sub":"U1","aud":"2010662588","exp":1783674000}`,
		`{"iss":"https://access.line.me","sub":"U1","aud":"2010662588","exp":1}`,
	}
	for _, body := range responses {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { _, _ = response.Write([]byte(body)) }))
		verifier := NewIDTokenVerifier("2010662588", server.URL, time.Second, func() time.Time { return now })
		_, err := verifier.Verify(context.Background(), "secret-token-value")
		server.Close()
		if err == nil || strings.Contains(err.Error(), "secret-token-value") || strings.Contains(err.Error(), body) {
			t.Fatalf("unsafe verification error: %v", err)
		}
	}
}

func TestIDTokenVerifierClassifiesProviderFailures(t *testing.T) {
	for _, test := range []struct {
		status    int
		retryable bool
	}{
		{status: 400, retryable: false},
		{status: 429, retryable: true},
		{status: 500, retryable: true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
			response.WriteHeader(test.status)
			_, _ = response.Write([]byte(`{"error_description":"raw provider detail"}`))
		}))
		verifier := NewIDTokenVerifier("2010662588", server.URL, time.Second, time.Now)
		_, err := verifier.Verify(context.Background(), "opaque-id-token-value")
		server.Close()
		var safeError *SafeError
		if !errors.As(err, &safeError) || safeError.Retryable != test.retryable || strings.Contains(err.Error(), "raw provider detail") {
			t.Fatalf("status %d error = %v", test.status, err)
		}
	}
}
