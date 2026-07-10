package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
)

type fakeViewerReportAPI struct {
	created      report.Run
	createErr    error
	got          report.Run
	getErr       error
	rows         viewer.ReportRows
	rowsErr      error
	dashboard    report.Dashboard
	dashboardErr error
	overview     viewer.ExecutiveOverview
	overviewErr  error
	refresh      viewer.DashboardRefresh
	refreshErr   error
	cancelled    report.Run
	cancelErr    error
	createCall   int
}

func (fake *fakeViewerReportAPI) Create(context.Context, uuid.UUID, uuid.UUID, report.Key, string, viewer.CreateReportRunInput) (report.Run, error) {
	fake.createCall++
	return fake.created, fake.createErr
}

func (fake *fakeViewerReportAPI) Get(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID) (report.Run, error) {
	return fake.got, fake.getErr
}

func (fake *fakeViewerReportAPI) ListRows(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID, string, int) (viewer.ReportRows, error) {
	return fake.rows, fake.rowsErr
}

func (fake *fakeViewerReportAPI) GetDashboard(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID) (report.Dashboard, error) {
	return fake.dashboard, fake.dashboardErr
}

func (fake *fakeViewerReportAPI) ExecutiveOverview(context.Context, uuid.UUID, uuid.UUID) (viewer.ExecutiveOverview, error) {
	return fake.overview, fake.overviewErr
}

func (fake *fakeViewerReportAPI) CreateDashboardRefresh(context.Context, uuid.UUID, uuid.UUID, string) (viewer.DashboardRefresh, error) {
	return fake.refresh, fake.refreshErr
}

func (fake *fakeViewerReportAPI) GetDashboardRefresh(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (viewer.DashboardRefresh, error) {
	return fake.refresh, fake.refreshErr
}

func (fake *fakeViewerReportAPI) Cancel(context.Context, uuid.UUID, uuid.UUID, report.Key, uuid.UUID) (report.Run, error) {
	return fake.cancelled, fake.cancelErr
}

func TestViewerCreatesFreshReportRunWithCSRFAndIdempotency(t *testing.T) {
	recipientID, tenantID, runID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	authAPI := &fakeViewerAPI{authenticated: viewer.AuthenticatedViewer{RecipientID: recipientID}}
	reportAPI := &fakeViewerReportAPI{created: report.Run{
		ID: runID, TenantID: tenantID, ReportKey: report.SalesGoodsServices, Status: report.StatusQueued,
		Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"}, QueuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}}
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: authAPI, ViewerReports: reportAPI,
	})
	path := "/api/v1/viewer/tenants/" + tenantID.String() + "/reports/sales_goods_services/runs"
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"periodPreset":"YESTERDAY"}`))
	request.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "viewer-csrf")
	request.Header.Set("Idempotency-Key", "viewer-open-001")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || reportAPI.createCall != 1 || !strings.Contains(response.Body.String(), runID.String()) || strings.Contains(response.Body.String(), "Idempotency") {
		t.Fatalf("status = %d calls=%d body=%s", response.Code, reportAPI.createCall, response.Body.String())
	}
}

func TestViewerReportRunRejectsMissingIdempotencyBeforeQueueing(t *testing.T) {
	tenantID := uuid.New()
	authAPI := &fakeViewerAPI{authenticated: viewer.AuthenticatedViewer{RecipientID: uuid.New()}}
	reportAPI := &fakeViewerReportAPI{}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: authAPI, ViewerReports: reportAPI})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/viewer/tenants/"+tenantID.String()+"/reports/stock_balance/runs", strings.NewReader(`{"periodPreset":"AS_OF_RUN"}`))
	request.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "viewer-csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity || reportAPI.createCall != 0 {
		t.Fatalf("status = %d calls=%d body=%s", response.Code, reportAPI.createCall, response.Body.String())
	}
}

func TestViewerReportRowsMapExpiredDataToGone(t *testing.T) {
	tenantID, runID := uuid.New(), uuid.New()
	authAPI := &fakeViewerAPI{authenticated: viewer.AuthenticatedViewer{RecipientID: uuid.New()}}
	reportAPI := &fakeViewerReportAPI{rowsErr: report.ErrRunRowsExpired}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: authAPI, ViewerReports: reportAPI})
	path := "/api/v1/viewer/tenants/" + tenantID.String() + "/reports/stock_balance/runs/" + runID.String() + "/rows"
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusGone || !strings.Contains(response.Body.String(), `"code":"REPORT_ROWS_EXPIRED"`) {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestViewerReturnsAuthorizedExecutiveDashboard(t *testing.T) {
	tenantID, runID := uuid.New(), uuid.New()
	authAPI := &fakeViewerAPI{authenticated: viewer.AuthenticatedViewer{RecipientID: uuid.New()}}
	reportAPI := &fakeViewerReportAPI{dashboard: report.Dashboard{
		ReportKey: report.StockBalance, Version: "1.0.0", Timezone: "Asia/Bangkok",
		KPIs: []report.DashboardMetric{{Key: "balance_amount", Label: "มูลค่าสต็อก", Value: "500.00", Unit: report.UnitTHB}},
	}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: authAPI, ViewerReports: reportAPI})
	path := "/api/v1/viewer/tenants/" + tenantID.String() + "/reports/stock_balance/runs/" + runID.String() + "/dashboard"
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"reportKey":"stock_balance"`) || !strings.Contains(response.Body.String(), `"balance_amount"`) {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}

func TestViewerExecutiveOverviewAndRefreshEndpoints(t *testing.T) {
	tenantID, refreshID := uuid.New(), uuid.New()
	authAPI := &fakeViewerAPI{authenticated: viewer.AuthenticatedViewer{RecipientID: uuid.New()}}
	reportAPI := &fakeViewerReportAPI{
		overview: viewer.ExecutiveOverview{TenantID: tenantID, Timezone: "Asia/Bangkok", Items: []viewer.DashboardSnapshot{}},
		refresh:  viewer.DashboardRefresh{ID: refreshID, TenantID: tenantID, Status: viewer.DashboardRefreshQueued, Total: 10},
	}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: authAPI, ViewerReports: reportAPI})

	overviewRequest := httptest.NewRequest(http.MethodGet, "/api/v1/viewer/tenants/"+tenantID.String()+"/executive-overview", nil)
	overviewRequest.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	overviewResponse := httptest.NewRecorder()
	handler.ServeHTTP(overviewResponse, overviewRequest)
	if overviewResponse.Code != http.StatusOK || !strings.Contains(overviewResponse.Body.String(), `"timezone":"Asia/Bangkok"`) {
		t.Fatalf("overview status=%d body=%s", overviewResponse.Code, overviewResponse.Body.String())
	}

	refreshRequest := httptest.NewRequest(http.MethodPost, "/api/v1/viewer/tenants/"+tenantID.String()+"/executive-overview/refreshes", nil)
	refreshRequest.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	refreshRequest.Header.Set("X-CSRF-Token", "viewer-csrf")
	refreshRequest.Header.Set("Idempotency-Key", "overview-refresh-001")
	refreshResponse := httptest.NewRecorder()
	handler.ServeHTTP(refreshResponse, refreshRequest)
	if refreshResponse.Code != http.StatusAccepted || !strings.Contains(refreshResponse.Body.String(), refreshID.String()) {
		t.Fatalf("refresh status=%d body=%s", refreshResponse.Code, refreshResponse.Body.String())
	}

	statusRequest := httptest.NewRequest(http.MethodGet, "/api/v1/viewer/tenants/"+tenantID.String()+"/executive-overview/refreshes/"+refreshID.String(), nil)
	statusRequest.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK || !strings.Contains(statusResponse.Body.String(), `"total":10`) {
		t.Fatalf("status=%d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
}

func TestViewerReportRunHidesCrossRecipientRun(t *testing.T) {
	tenantID, runID := uuid.New(), uuid.New()
	authAPI := &fakeViewerAPI{authenticated: viewer.AuthenticatedViewer{RecipientID: uuid.New()}}
	reportAPI := &fakeViewerReportAPI{getErr: viewer.ErrReportForbidden}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: authAPI, ViewerReports: reportAPI})
	path := "/api/v1/viewer/tenants/" + tenantID.String() + "/reports/stock_balance/runs/" + runID.String()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "viewer-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), runID.String()) {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
}
