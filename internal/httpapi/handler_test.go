package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type readinessFunc func(context.Context) error

func (f readinessFunc) Ping(ctx context.Context) error { return f(ctx) }

func TestLiveReturnsStableContractAndRequestID(t *testing.T) {
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil })})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health/live", nil)
	request.Header.Set("X-Request-ID", "request-123")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Request-ID"); got != "request-123" {
		t.Fatalf("X-Request-ID = %q", got)
	}
	if body := response.Body.String(); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("body = %s", body)
	}
}

func TestReadyReturnsServiceUnavailableWithoutLeakingDependencyError(t *testing.T) {
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error {
		return errors.New("postgres password=do-not-leak")
	})})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/health/ready", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, `"code":"SERVICE_UNAVAILABLE"`) {
		t.Fatalf("body = %s", body)
	}
	if strings.Contains(body, "do-not-leak") {
		t.Fatalf("dependency error leaked: %s", body)
	}
}

func TestUnknownRouteUsesProblemContract(t *testing.T) {
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil })})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); !strings.Contains(body, `"code":"NOT_FOUND"`) {
		t.Fatalf("body = %s", body)
	}
}
