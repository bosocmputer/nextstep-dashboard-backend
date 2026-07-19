package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tablequery"
	"github.com/google/uuid"
)

type fakeTableQueryAPI struct {
	reportRunsInput tablequery.ReportRunsInput
	reportRuns      tablequery.ReportRunsResult
	calls           int
}

type fakeOccurrenceTableQueryAPI struct{ err error }

func (fake fakeOccurrenceTableQueryAPI) QueryOccurrences(context.Context, uuid.UUID, tablequery.OccurrencesInput) (tablequery.OccurrencesResult, error) {
	return tablequery.OccurrencesResult{}, fake.err
}

func TestOccurrenceQueryReturnsNotFoundWithoutLeakingAnotherResource(t *testing.T) {
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, TableQueries: fakeOccurrenceTableQueryAPI{err: sentinel.ErrNotFound}})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/operational-incidents/"+uuid.NewString()+"/occurrences/query", strings.NewReader(`{"page":0,"pageSize":25,"filters":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "valid-csrf")
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), `"code":"INCIDENT_NOT_FOUND"`) {
		t.Fatalf("not found response = %d body=%s", response.Code, response.Body.String())
	}
}

func (fake *fakeTableQueryAPI) QueryReportRuns(_ context.Context, input tablequery.ReportRunsInput) (tablequery.ReportRunsResult, error) {
	fake.calls++
	fake.reportRunsInput = input
	return fake.reportRuns, nil
}

func TestReportRunsQueryRequiresCSRFAndUsesTypedPageContract(t *testing.T) {
	api := &fakeTableQueryAPI{reportRuns: tablequery.ReportRunsResult{PageMeta: tablequery.NewPageMeta(2, 50, 101)}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, TableQueries: api})

	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/report-runs/query", strings.NewReader(`{"page":2,"pageSize":50,"globalSearch":"stock","filters":{"statuses":["FAILED"]}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "valid-csrf")
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || api.calls != 1 {
		t.Fatalf("query response = %d calls=%d body=%s", response.Code, api.calls, response.Body.String())
	}
	if api.reportRunsInput.Page != 2 || api.reportRunsInput.PageSize != 50 || api.reportRunsInput.GlobalSearch != "stock" || len(api.reportRunsInput.Filters.Statuses) != 1 {
		t.Fatalf("query input = %+v", api.reportRunsInput)
	}
	if !strings.Contains(response.Body.String(), `"totalPages":3`) {
		t.Fatalf("query response body = %s", response.Body.String())
	}
}

func TestReportRunsQueryRejectsInvalidPageWithoutCallingService(t *testing.T) {
	api := &fakeTableQueryAPI{}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, TableQueries: api})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/report-runs/query", strings.NewReader(`{"page":-1,"pageSize":25,"filters":{}}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "valid-csrf")
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || api.calls != 0 {
		t.Fatalf("invalid query response = %d calls=%d body=%s", response.Code, api.calls, response.Body.String())
	}
}
