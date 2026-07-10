package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type fakeOperationsAPI struct {
	quotaStatus  operations.LineQuotaStatus
	reportPage   operations.ReportRunPage
	deliveryPage operations.DeliveryPage
	auditPage    operations.AuditPage
	calls        int
}

func (fake *fakeOperationsAPI) GetLineQuota(context.Context, time.Time) (operations.LineQuotaStatus, error) {
	fake.calls++
	return fake.quotaStatus, nil
}

func (fake *fakeOperationsAPI) ListReportRuns(context.Context, operations.ReportRunFilter) (operations.ReportRunPage, error) {
	fake.calls++
	return fake.reportPage, nil
}

func (fake *fakeOperationsAPI) ListDeliveries(context.Context, operations.DeliveryFilter) (operations.DeliveryPage, error) {
	fake.calls++
	return fake.deliveryPage, nil
}

func (fake *fakeOperationsAPI) ListAudit(context.Context, operations.AuditFilter) (operations.AuditPage, error) {
	fake.calls++
	return fake.auditPage, nil
}

func TestAdminOperationsReturnRedactedHistoryPages(t *testing.T) {
	tenantID, deliveryID := uuid.New(), uuid.New()
	api := &fakeOperationsAPI{deliveryPage: operations.DeliveryPage{Data: []operations.Delivery{{
		ID: deliveryID, TenantID: tenantID, Status: "ACCEPTED", Attempt: 1, CreatedAt: time.Now(), ExpiresAt: time.Now().AddDate(1, 0, 0),
	}}}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/line-deliveries?tenantId="+tenantID.String(), nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || api.calls != 1 || !strings.Contains(response.Body.String(), deliveryID.String()) || strings.Contains(response.Body.String(), "recipientId") {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.calls, response.Body.String())
	}
}

func TestAdminOperationsRejectInvalidFiltersBeforeQuery(t *testing.T) {
	api := &fakeOperationsAPI{reportPage: operations.ReportRunPage{Data: []report.Run{}}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/report-runs?tenantId=not-a-uuid&status=UNKNOWN", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity || api.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.calls, response.Body.String())
	}
}

func TestAdminReadsSafeSharedLINEQuotaStatus(t *testing.T) {
	limit, consumed := 5000, 4200
	api := &fakeOperationsAPI{quotaStatus: operations.LineQuotaStatus{
		State: "READY", ProviderLimit: &limit, ProviderConsumed: &consumed, LocallyAccepted: 24, OperationalReservePercent: 10,
	}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/line-quota", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusOK || api.calls != 1 || !strings.Contains(body, `"providerConsumed":4200`) || strings.Contains(body, "token") {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.calls, body)
	}
}
