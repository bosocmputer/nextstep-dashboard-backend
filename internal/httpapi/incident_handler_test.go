package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/google/uuid"
)

type fakeIncidentAPI struct {
	incident sentinel.Incident
	detail   sentinel.IncidentDetail
	calls    int
}

func (fake *fakeIncidentAPI) List(context.Context, sentinel.IncidentFilter) (sentinel.IncidentPage, error) {
	fake.calls++
	return sentinel.IncidentPage{Data: []sentinel.Incident{fake.incident}}, nil
}
func (fake *fakeIncidentAPI) Get(context.Context, uuid.UUID) (sentinel.IncidentDetail, error) {
	fake.calls++
	return fake.detail, nil
}
func (fake *fakeIncidentAPI) Occurrences(context.Context, uuid.UUID, sentinel.OccurrenceFilter) (sentinel.OccurrencePage, error) {
	fake.calls++
	return sentinel.OccurrencePage{}, nil
}
func (fake *fakeIncidentAPI) Acknowledge(context.Context, uuid.UUID, int) (sentinel.Incident, error) {
	fake.calls++
	fake.incident.Status = sentinel.StatusAcknowledged
	return fake.incident, nil
}
func (fake *fakeIncidentAPI) AcceptRisk(context.Context, uuid.UUID, int, string) (sentinel.Incident, error) {
	fake.calls++
	fake.incident.Status = sentinel.StatusClosedAccepted
	return fake.incident, nil
}

func TestOperationalIncidentRoutesRequireAdminAndCSRFForMutations(t *testing.T) {
	id := uuid.New()
	api := &fakeIncidentAPI{incident: sentinel.Incident{ID: id, AlertRef: "NST-ABC123DEF456", Status: sentinel.StatusOpen, Severity: sentinel.SeverityP1, Version: 1}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Incidents: api})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/operational-incidents", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "NST-ABC123DEF456") {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/operational-incidents/"+id.String()+"/acknowledge", strings.NewReader(`{"version":1}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("X-CSRF-Token", "csrf")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ACKNOWLEDGED") {
		t.Fatalf("ack status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestOperationalIncidentMutationRejectsMissingCSRF(t *testing.T) {
	id := uuid.New()
	api := &fakeIncidentAPI{incident: sentinel.Incident{ID: id, AlertRef: "NST-ABC123DEF456", Status: sentinel.StatusOpen, Severity: sentinel.SeverityP1, Version: 1}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{csrfErr: auth.ErrInvalidCSRF}, Incidents: api})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/operational-incidents/"+id.String()+"/acknowledge", strings.NewReader(`{"version":1}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s, want forbidden", response.Code, response.Body.String())
	}
	if api.calls != 0 {
		t.Fatalf("incident mutation called %d times without valid CSRF", api.calls)
	}
}

type fakeWatchdog struct{ status sentinel.WatchdogStatus }

func (fake fakeWatchdog) Status() sentinel.WatchdogStatus { return fake.status }

func TestWatchdogHealthReturns503ForDegradedStateWithoutDatabase(t *testing.T) {
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), Watchdog: fakeWatchdog{status: sentinel.WatchdogStatus{Status: "degraded", CheckedAt: time.Now(), SafeErrorCodes: []string{"SENTINEL_HEARTBEAT_STALE"}}}})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/health/watchdog", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "SENTINEL_HEARTBEAT_STALE") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
