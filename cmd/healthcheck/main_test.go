package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckRequiresSuccessfulReadinessResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if err := check(server.URL, server.Client()); err == nil {
		t.Fatal("expected a non-200 readiness response to fail")
	}
}
