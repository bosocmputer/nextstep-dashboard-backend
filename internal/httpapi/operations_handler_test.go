package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type fakeOperationsAPI struct {
	quotaStatus    operations.LineQuotaStatus
	reportPage     operations.ReportRunPage
	reportDetail   operations.ReportRunDetail
	deliveryPage   operations.DeliveryPage
	auditPage      operations.AuditPage
	calls          int
	reportFilter   operations.ReportRunFilter
	deliveryFilter operations.DeliveryFilter
	auditFilter    operations.AuditFilter
}

func TestAdminReportRunDetailReturnsThaiEvidenceAndLINEImpact(t *testing.T) {
	runID, tenantID := uuid.New(), uuid.New()
	now := time.Date(2026, 7, 16, 11, 0, 4, 0, time.UTC)
	evidence := failure.Complete(failure.Evidence{
		Version: 1, Level: failure.LevelConfirmed, Category: failure.CategoryJavaWSConnectivity,
		Stage: failure.StageConnectJavaWS, TransportPhase: failure.PhaseBeforeRequestSent,
		OccurredAt: now, SafeErrorCode: failure.CodeSMLUnreachable,
	})
	api := &fakeOperationsAPI{reportDetail: operations.ReportRunDetail{
		ReportRun: operations.ReportRun{Run: report.Run{
			ID: runID, TenantID: tenantID, ReportKey: report.StockBalance, Status: report.StatusFailed,
			SafeErrorCode: failure.CodeSMLUnreachable, FailureEvidence: &evidence, QueuedAt: now, UpdatedAt: now,
		}, TenantName: "ร้านตัวอย่าง", FailureSummary: &evidence},
		Impact:      failure.Impact{ReportsTotal: 10, ReportsFailed: 1, ReportsCancelled: 9, Notification: failure.NotificationNotCreatedIncompleteSet},
		TriggerKind: "SCHEDULED",
	}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/report-runs/"+runID.String(), nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, "ติดต่อ Java Web Service") || !strings.Contains(body, `"reportsCancelled":9`) || !strings.Contains(body, "NOT_CREATED_INCOMPLETE_REPORT_SET") || strings.Contains(body, "endpoint") {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
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

func (fake *fakeOperationsAPI) ListReportRuns(_ context.Context, filter operations.ReportRunFilter) (operations.ReportRunPage, error) {
	fake.calls++
	fake.reportFilter = filter
	return fake.reportPage, nil
}

func (fake *fakeOperationsAPI) GetReportRunDetail(context.Context, uuid.UUID, time.Time) (operations.ReportRunDetail, error) {
	fake.calls++
	return fake.reportDetail, nil
}

func (fake *fakeOperationsAPI) ListDeliveries(_ context.Context, filter operations.DeliveryFilter) (operations.DeliveryPage, error) {
	fake.calls++
	fake.deliveryFilter = filter
	return fake.deliveryPage, nil
}

func (fake *fakeOperationsAPI) ListAudit(_ context.Context, filter operations.AuditFilter) (operations.AuditPage, error) {
	fake.calls++
	fake.auditFilter = filter
	return fake.auditPage, nil
}

func TestAdminOperationsParseTypedTableFilters(t *testing.T) {
	tenantID, recipientID := uuid.New(), uuid.New()
	tests := []struct {
		name   string
		path   string
		assert func(*testing.T, *fakeOperationsAPI)
	}{
		{
			name: "report runs",
			path: "/api/v1/admin/report-runs?tenantId=" + tenantID.String() + "&status=FAILED&reportKey=stock_balance&source=SCHEDULE&dateFrom=2026-07-01&dateTo=2026-07-19",
			assert: func(t *testing.T, api *fakeOperationsAPI) {
				filter := api.reportFilter
				if filter.ReportKey == nil || *filter.ReportKey != report.StockBalance || filter.Source == nil || *filter.Source != report.SourceSchedule || filter.CreatedFrom == nil || filter.CreatedTo == nil {
					t.Fatalf("report filter = %+v", filter)
				}
			},
		},
		{
			name: "deliveries",
			path: "/api/v1/admin/line-deliveries?tenantId=" + tenantID.String() + "&status=FAILED_PERMANENT&recipientId=" + recipientID.String() + "&dateFrom=2026-07-01&dateTo=2026-07-19",
			assert: func(t *testing.T, api *fakeOperationsAPI) {
				filter := api.deliveryFilter
				if filter.Status == nil || *filter.Status != "FAILED_PERMANENT" || filter.RecipientID == nil || *filter.RecipientID != recipientID || filter.CreatedFrom == nil || filter.CreatedTo == nil {
					t.Fatalf("delivery filter = %+v", filter)
				}
			},
		},
		{
			name: "audit",
			path: "/api/v1/admin/audit-logs?tenantId=" + tenantID.String() + "&actorType=ADMIN&action=TENANT_UPDATED&result=SUCCESS&dateFrom=2026-07-01&dateTo=2026-07-19",
			assert: func(t *testing.T, api *fakeOperationsAPI) {
				filter := api.auditFilter
				if filter.ActorType == nil || *filter.ActorType != "ADMIN" || filter.Action == nil || *filter.Action != "TENANT_UPDATED" || filter.Result == nil || *filter.Result != "SUCCESS" || filter.CreatedFrom == nil || filter.CreatedTo == nil {
					t.Fatalf("audit filter = %+v", filter)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &fakeOperationsAPI{}
			handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK || api.calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, api.calls, response.Body.String())
			}
			test.assert(t, api)
		})
	}
}

func TestAdminOperationsRejectInvalidTypedTableFilters(t *testing.T) {
	paths := []string{
		"/api/v1/admin/report-runs?reportKey=unknown",
		"/api/v1/admin/report-runs?source=VIEWER",
		"/api/v1/admin/line-deliveries?status=UNKNOWN",
		"/api/v1/admin/line-deliveries?recipientId=invalid",
		"/api/v1/admin/audit-logs?actorType=ROOT",
		"/api/v1/admin/audit-logs?action=not-valid",
		"/api/v1/admin/audit-logs?result=UNKNOWN",
		"/api/v1/admin/audit-logs?dateFrom=2026-07-20&dateTo=2026-07-19",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			api := &fakeOperationsAPI{}
			handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{}, Operations: api})
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnprocessableEntity || api.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, api.calls, response.Body.String())
			}
		})
	}
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
