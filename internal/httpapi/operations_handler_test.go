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

func TestAdminOperationsReturnTenantNamesForReportAndAuditHistory(t *testing.T) {
	tenantID := uuid.New()
	tenantName := "ร้านตัวอย่าง"
	api := &fakeOperationsAPI{
		reportPage: operations.ReportRunPage{Data: []operations.ReportRun{{
			Run:        report.Run{ID: uuid.New(), TenantID: tenantID, ReportKey: report.SalesGoodsServices, Status: report.StatusSucceeded},
			TenantName: tenantName,
		}}},
		auditPage: operations.AuditPage{Data: []operations.AuditEvent{{
			ID: uuid.New(), TenantID: &tenantID, TenantName: &tenantName, ActorType: "ADMIN", Action: "TENANT_UPDATED", Result: "SUCCESS", CreatedAt: time.Now(),
		}}},
	}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})

	for _, path := range []string{"/api/v1/admin/report-runs", "/api/v1/admin/audit-logs"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"tenantName":"ร้านตัวอย่าง"`) {
			t.Fatalf("path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
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
		ID: deliveryID, TenantID: tenantID, TenantName: "ร้านตัวอย่าง", RecipientName: "เจ้าของร้าน",
		ReportKeys: []report.Key{report.SalesGoodsServices}, ReportCount: 1,
		Status: "ACCEPTED", Attempt: 1, CreatedAt: time.Now(), ExpiresAt: time.Now().AddDate(1, 0, 0),
	}}}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/line-deliveries?tenantId="+tenantID.String(), nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusOK || api.calls != 1 || !strings.Contains(body, deliveryID.String()) ||
		!strings.Contains(body, `"tenantName":"ร้านตัวอย่าง"`) || !strings.Contains(body, `"recipientDisplayName":"เจ้าของร้าน"`) ||
		!strings.Contains(body, `"reportKeys":["sales_goods_services"]`) || !strings.Contains(body, `"reportCount":1`) ||
		strings.Contains(body, "recipientId") || strings.Contains(body, "StoredRecipient") {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, api.calls, response.Body.String())
	}
}

func TestAdminOperationsRejectInvalidFiltersBeforeQuery(t *testing.T) {
	api := &fakeOperationsAPI{reportPage: operations.ReportRunPage{Data: []operations.ReportRun{}}}
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
