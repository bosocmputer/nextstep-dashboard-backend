package viewer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type fakeReportAccess struct {
	tenants []TenantAccess
	allowed bool
}

func (fake *fakeReportAccess) ListTenants(context.Context, uuid.UUID) ([]TenantAccess, error) {
	return fake.tenants, nil
}

func (fake *fakeReportAccess) CanAccessReport(context.Context, uuid.UUID, uuid.UUID, report.Key) (bool, error) {
	return fake.allowed, nil
}

type fakeViewerRunStore struct {
	enqueued           report.EnqueueInput
	run                report.Run
	rows               report.RowsPage
	dashboard          report.Dashboard
	latest             []DashboardSnapshot
	refresh            DashboardRefresh
	refreshResult      DashboardRefreshResult
	refreshInputs      []report.EnqueueInput
	refreshRecipient   uuid.UUID
	scheduledRecipient uuid.UUID
	cancelled          bool
	exactSnapshots     map[report.Key]DashboardSnapshot
	exactRequests      []SnapshotPeriodRequest
	revalidationCalls  int
}

func (fake *fakeViewerRunStore) Enqueue(_ context.Context, input report.EnqueueInput, _ time.Time) (report.Run, error) {
	fake.enqueued = input
	if fake.run.ID == uuid.Nil {
		fake.run = report.Run{
			ID: uuid.New(), TenantID: input.TenantID, ReportKey: input.ReportKey, Source: input.Source,
			RequestedByRecipient: input.RequestedByRecipient, Period: input.Period, Status: report.StatusQueued,
		}
	}
	return fake.run, nil
}

func (fake *fakeViewerRunStore) Get(context.Context, uuid.UUID, time.Time) (report.Run, error) {
	return fake.run, nil
}

func (fake *fakeViewerRunStore) CanAccessScheduledRun(_ context.Context, recipientID, runID uuid.UUID) (bool, error) {
	return fake.run.ID == runID && fake.scheduledRecipient == recipientID, nil
}

func (fake *fakeViewerRunStore) ListRows(context.Context, uuid.UUID, int, int, time.Time) (report.RowsPage, error) {
	return fake.rows, nil
}

func (fake *fakeViewerRunStore) GetDashboard(context.Context, uuid.UUID, uuid.UUID) (report.Dashboard, error) {
	return fake.dashboard, nil
}

func (fake *fakeViewerRunStore) ListLatestDashboards(context.Context, uuid.UUID, []report.Key) ([]DashboardSnapshot, error) {
	return fake.latest, nil
}

func (fake *fakeViewerRunStore) CreateDashboardRefresh(_ context.Context, recipientID, _ uuid.UUID, _ string, inputs []report.EnqueueInput, _ time.Time) (DashboardRefresh, error) {
	fake.refreshRecipient = recipientID
	fake.refreshInputs = inputs
	return fake.refresh, nil
}

func (fake *fakeViewerRunStore) GetDashboardRefresh(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) (DashboardRefresh, error) {
	return fake.refresh, nil
}

func (fake *fakeViewerRunStore) GetDashboardRefreshResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (DashboardRefreshResult, error) {
	return fake.refreshResult, nil
}

func (fake *fakeViewerRunStore) Cancel(context.Context, uuid.UUID, time.Time) (report.Run, error) {
	fake.cancelled = true
	fake.run.Status = report.StatusCancelled
	return fake.run, nil
}

func (fake *fakeViewerRunStore) RevalidateSnapshot(context.Context, uuid.UUID, report.Key, report.Period, time.Time) (ReportRevalidation, error) {
	fake.revalidationCalls++
	return ReportRevalidation{}, nil
}

func (fake *fakeViewerRunStore) GetExactSnapshotForPeriod(_ context.Context, _ uuid.UUID, key report.Key, _ report.Period, _ time.Time) (DashboardSnapshot, error) {
	if snapshot, ok := fake.exactSnapshots[key]; ok {
		return snapshot, nil
	}
	return DashboardSnapshot{}, report.ErrRunNotFound
}

func (fake *fakeViewerRunStore) GetExactSnapshotsForPeriods(_ context.Context, _ uuid.UUID, requests []SnapshotPeriodRequest, _ time.Time) (map[report.Key]DashboardSnapshot, error) {
	fake.exactRequests = append([]SnapshotPeriodRequest(nil), requests...)
	return fake.exactSnapshots, nil
}

func TestReportServiceExactOverviewReadsCacheWithoutEnqueueing(t *testing.T) {
	now := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	recipientID, tenantID := uuid.New(), uuid.New()
	access := &fakeReportAccess{allowed: true, tenants: []TenantAccess{{
		ID: tenantID, Timezone: "Asia/Bangkok", ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance},
	}}}
	salesSnapshot := DashboardSnapshot{RunID: uuid.New(), Dashboard: report.Dashboard{ReportKey: report.SalesGoodsServices}}
	store := &fakeViewerRunStore{exactSnapshots: map[report.Key]DashboardSnapshot{report.SalesGoodsServices: salesSnapshot}}
	service := NewReportService(access, store, func() time.Time { return now })

	overview, err := service.ExactOverview(context.Background(), recipientID, tenantID, DashboardRefreshInput{
		PeriodPreset: report.MonthToDate,
		ReportKeys:   []report.Key{report.SalesGoodsServices, report.StockBalance},
	})

	if err != nil || len(overview.Items) != 1 || overview.Items[0].RunID != salesSnapshot.RunID {
		t.Fatalf("ExactOverview() = %+v, %v", overview, err)
	}
	if len(store.exactRequests) != 2 || store.exactRequests[0].Period.DateFrom != "2026-07-01" || store.exactRequests[0].Period.DateTo != "2026-07-12" || store.exactRequests[1].Period.DateFrom != "2026-07-12" || store.exactRequests[1].Period.DateTo != "2026-07-12" {
		t.Fatalf("exact requests = %+v", store.exactRequests)
	}
	if store.revalidationCalls != 0 || store.enqueued.ReportKey != "" {
		t.Fatalf("cache-only lookup caused work: revalidations=%d enqueued=%+v", store.revalidationCalls, store.enqueued)
	}
}

func TestReportServiceCreatesFreshRunInTenantTimezone(t *testing.T) {
	now := time.Date(2026, 7, 10, 18, 30, 0, 0, time.UTC) // July 11 in Bangkok
	recipientID, tenantID := uuid.New(), uuid.New()
	access := &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}
	store := &fakeViewerRunStore{}
	service := NewReportService(access, store, func() time.Time { return now })

	run, err := service.Create(context.Background(), recipientID, tenantID, report.SalesGoodsServices, "viewer-refresh-001", CreateReportRunInput{PeriodPreset: report.Yesterday})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if run.ID == uuid.Nil || store.enqueued.Period.DateFrom != "2026-07-10" || store.enqueued.Period.DateTo != "2026-07-10" || store.enqueued.RequestedByRecipient == nil || *store.enqueued.RequestedByRecipient != recipientID {
		t.Fatalf("enqueued = %+v", store.enqueued)
	}
}

func TestReportServiceSnapshotFirstFeatureFlagFallsBackWithoutEnqueue(t *testing.T) {
	now := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	recipientID, tenantID := uuid.New(), uuid.New()
	access := &fakeReportAccess{allowed: true, tenants: []TenantAccess{{
		ID: tenantID, Timezone: "Asia/Bangkok", ReportKeys: []report.Key{report.SalesGoodsServices},
	}}}
	store := &fakeViewerRunStore{latest: []DashboardSnapshot{{
		RunID: uuid.New(), Dashboard: report.Dashboard{ReportKey: report.SalesGoodsServices, Version: "1.0.0"},
	}}}
	service := NewReportService(access, store, func() time.Time { return now }).ConfigureSnapshotFirst(false, nil)

	revalidation, err := service.Revalidate(context.Background(), recipientID, tenantID, report.SalesGoodsServices, CreateReportRunInput{PeriodPreset: report.MonthToDate})
	if err != nil || !revalidation.LegacyFallback || revalidation.Disposition != RevalidationDisabled {
		t.Fatalf("Revalidate() = %+v, %v", revalidation, err)
	}
	overview, err := service.RevalidateOverview(context.Background(), recipientID, tenantID, DashboardRefreshInput{})
	if err != nil || !overview.LegacyFallback || len(overview.Overview.Items) != 1 {
		t.Fatalf("RevalidateOverview() = %+v, %v", overview, err)
	}
	if store.enqueued.ReportKey != "" {
		t.Fatalf("feature-disabled fallback enqueued %+v", store.enqueued)
	}
}

func TestReportServiceRejectsAmbiguousAndUnauthorizedInputs(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	recipientID, tenantID := uuid.New(), uuid.New()
	from, to := "2026-07-01", "2026-07-10"
	future := "2026-07-11"
	for _, test := range []struct {
		name   string
		access *fakeReportAccess
		key    report.Key
		idem   string
		input  CreateReportRunInput
	}{
		{name: "tenant forbidden", access: &fakeReportAccess{allowed: true}, key: report.SalesGoodsServices, idem: "viewer-run-001", input: CreateReportRunInput{PeriodPreset: report.Yesterday}},
		{name: "permission forbidden", access: &fakeReportAccess{allowed: false, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.SalesGoodsServices, idem: "viewer-run-002", input: CreateReportRunInput{PeriodPreset: report.Yesterday}},
		{name: "unknown report", access: &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.Key("unknown"), idem: "viewer-run-003", input: CreateReportRunInput{PeriodPreset: report.Yesterday}},
		{name: "dates with preset", access: &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.SalesGoodsServices, idem: "viewer-run-004", input: CreateReportRunInput{PeriodPreset: report.Yesterday, DateFrom: &from, DateTo: &to}},
		{name: "short idempotency", access: &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.SalesGoodsServices, idem: "short", input: CreateReportRunInput{PeriodPreset: report.Yesterday}},
		{name: "future custom date", access: &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.SalesGoodsServices, idem: "viewer-run-005", input: CreateReportRunInput{PeriodPreset: report.Custom, DateFrom: &future, DateTo: &future}},
		{name: "date range rejects as of", access: &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.SalesGoodsServices, idem: "viewer-run-006", input: CreateReportRunInput{PeriodPreset: report.AsOfRun}},
		{name: "as of rejects range preset", access: &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.StockBalance, idem: "viewer-run-007", input: CreateReportRunInput{PeriodPreset: report.MonthToDate}},
		{name: "current only rejects historical", access: &fakeReportAccess{allowed: true, tenants: []TenantAccess{{ID: tenantID, Timezone: "Asia/Bangkok"}}}, key: report.StockReorder, idem: "viewer-run-008", input: CreateReportRunInput{PeriodPreset: report.Custom, DateFrom: &from, DateTo: &to}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewReportService(test.access, &fakeViewerRunStore{}, func() time.Time { return now })
			_, err := service.Create(context.Background(), recipientID, tenantID, test.key, test.idem, test.input)
			if !errors.Is(err, ErrReportForbidden) && !errors.Is(err, ErrReportInputInvalid) {
				t.Fatalf("Create() error = %v", err)
			}
		})
	}
}

func TestReportServiceBindsRunReadsAndCancellationToRequestingRecipient(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	recipientID, tenantID, runID := uuid.New(), uuid.New(), uuid.New()
	store := &fakeViewerRunStore{
		run:       report.Run{ID: runID, TenantID: tenantID, ReportKey: report.StockBalance, Source: report.SourceDashboard, RequestedByRecipient: &recipientID, Status: report.StatusSucceeded},
		rows:      report.RowsPage{Rows: []map[string]string{{"z": "2", "a": "1"}}, NextOrdinal: 1},
		dashboard: report.Dashboard{ReportKey: report.StockBalance, Version: "1.0.0"},
	}
	service := NewReportService(&fakeReportAccess{allowed: true}, store, func() time.Time { return now })

	if _, err := service.Get(context.Background(), recipientID, tenantID, report.StockBalance, runID); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	dashboard, err := service.GetDashboard(context.Background(), recipientID, tenantID, report.StockBalance, runID)
	if err != nil || dashboard.ReportKey != report.StockBalance {
		t.Fatalf("GetDashboard() = %+v, %v", dashboard, err)
	}
	page, err := service.ListRows(context.Background(), recipientID, tenantID, report.StockBalance, runID, "", 25)
	if err != nil || len(page.Columns) != 2 || page.Columns[0] != "a" || page.Columns[1] != "z" {
		t.Fatalf("ListRows() = %+v, %v", page, err)
	}
	if _, err := service.Cancel(context.Background(), recipientID, tenantID, report.StockBalance, runID); err != nil || !store.cancelled {
		t.Fatalf("Cancel() error = %v cancelled=%v", err, store.cancelled)
	}

	otherRecipient := uuid.New()
	if _, err := service.Get(context.Background(), otherRecipient, tenantID, report.StockBalance, runID); !errors.Is(err, ErrReportForbidden) {
		t.Fatalf("cross-recipient Get() error = %v", err)
	}
	if _, err := service.GetDashboard(context.Background(), otherRecipient, tenantID, report.StockBalance, runID); !errors.Is(err, ErrReportForbidden) {
		t.Fatalf("cross-recipient GetDashboard() error = %v", err)
	}
}

func TestReportServiceAllowsPermissionCheckedScheduledSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	recipientID, tenantID, runID := uuid.New(), uuid.New(), uuid.New()
	store := &fakeViewerRunStore{
		run:                report.Run{ID: runID, TenantID: tenantID, ReportKey: report.SalesGoodsServices, Source: report.SourceSchedule, Status: report.StatusSucceeded},
		dashboard:          report.Dashboard{ReportKey: report.SalesGoodsServices, Version: "1.0.0"},
		scheduledRecipient: recipientID,
	}
	service := NewReportService(&fakeReportAccess{allowed: true}, store, func() time.Time { return now })
	if _, err := service.Get(context.Background(), recipientID, tenantID, report.SalesGoodsServices, runID); err != nil {
		t.Fatalf("scheduled Get() error = %v", err)
	}
	if _, err := service.GetDashboard(context.Background(), recipientID, tenantID, report.SalesGoodsServices, runID); err != nil {
		t.Fatalf("scheduled GetDashboard() error = %v", err)
	}
	if _, err := service.Get(context.Background(), recipientID, uuid.New(), report.SalesGoodsServices, runID); !errors.Is(err, ErrReportForbidden) {
		t.Fatalf("cross-tenant scheduled Get() error = %v", err)
	}
	if _, err := service.Get(context.Background(), recipientID, tenantID, report.StockBalance, runID); !errors.Is(err, ErrReportForbidden) {
		t.Fatalf("wrong-report scheduled Get() error = %v", err)
	}
	if _, err := service.Get(context.Background(), uuid.New(), tenantID, report.SalesGoodsServices, runID); !errors.Is(err, ErrReportForbidden) {
		t.Fatalf("wrong-recipient scheduled Get() error = %v", err)
	}
}

func TestReportServiceBuildsPermissionFilteredExecutiveOverviewAndRefresh(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	recipientID, tenantID, refreshID := uuid.New(), uuid.New(), uuid.New()
	access := &fakeReportAccess{allowed: true, tenants: []TenantAccess{{
		ID: tenantID, Name: "วาวา", Timezone: "Asia/Bangkok",
		ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance, report.StockReorder},
	}}}
	store := &fakeViewerRunStore{
		latest:  []DashboardSnapshot{{RunID: uuid.New(), Dashboard: report.Dashboard{ReportKey: report.SalesGoodsServices, Version: "1.0.0"}}},
		refresh: DashboardRefresh{ID: refreshID, TenantID: tenantID, Status: DashboardRefreshQueued, Total: 3},
		refreshResult: DashboardRefreshResult{
			RefreshID: refreshID, TenantID: tenantID, Status: DashboardRefreshSucceeded,
			Items: []DashboardSnapshot{{RunID: uuid.New(), Dashboard: report.Dashboard{ReportKey: report.SalesGoodsServices, Version: "1.0.0"}}},
		},
	}
	service := NewReportService(access, store, func() time.Time { return now })

	overview, err := service.ExecutiveOverview(context.Background(), recipientID, tenantID)
	if err != nil || overview.TenantID != tenantID || overview.Timezone != "Asia/Bangkok" || len(overview.Items) != 1 {
		t.Fatalf("ExecutiveOverview() = %+v, %v", overview, err)
	}
	from, to := "2026-07-01", "2026-07-09"
	refresh, err := service.CreateDashboardRefresh(context.Background(), recipientID, tenantID, "overview-refresh-001", &DashboardRefreshInput{
		PeriodPreset: report.Custom, DateFrom: &from, DateTo: &to,
		ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance, report.StockReorder},
	})
	if err != nil || refresh.ID != refreshID || store.refreshRecipient != recipientID || len(store.refreshInputs) != 3 {
		t.Fatalf("CreateDashboardRefresh() = %+v, %v inputs=%+v", refresh, err, store.refreshInputs)
	}
	if store.refreshInputs[0].Period != (report.Period{Preset: report.Custom, DateFrom: from, DateTo: to}) ||
		store.refreshInputs[1].Period != (report.Period{Preset: report.Custom, DateFrom: to, DateTo: to}) ||
		store.refreshInputs[2].Period.Preset != report.AsOfRun {
		t.Fatalf("refresh periods = %+v", store.refreshInputs)
	}
	if _, err := service.CreateDashboardRefresh(context.Background(), recipientID, tenantID, "overview-refresh-002", &DashboardRefreshInput{}); !errors.Is(err, ErrReportInputInvalid) {
		t.Fatalf("empty refresh input error = %v", err)
	}
	if _, err := service.CreateDashboardRefresh(context.Background(), recipientID, tenantID, "overview-refresh-003", &DashboardRefreshInput{PeriodPreset: report.MonthToDate, ReportKeys: []report.Key{report.SalesGoodsServices}}); !errors.Is(err, ErrViewerContextChanged) {
		t.Fatalf("changed report context error = %v", err)
	}
	got, err := service.GetDashboardRefresh(context.Background(), recipientID, tenantID, refreshID)
	if err != nil || got.ID != refreshID {
		t.Fatalf("GetDashboardRefresh() = %+v, %v", got, err)
	}
	result, err := service.GetDashboardRefreshResult(context.Background(), recipientID, tenantID, refreshID)
	if err != nil || result.RefreshID != refreshID || len(result.Items) != 1 {
		t.Fatalf("GetDashboardRefreshResult() = %+v, %v", result, err)
	}
}

func TestReportServiceRejectsRefreshResultAfterPermissionChange(t *testing.T) {
	recipientID, tenantID, refreshID := uuid.New(), uuid.New(), uuid.New()
	store := &fakeViewerRunStore{refreshResult: DashboardRefreshResult{
		RefreshID: refreshID, TenantID: tenantID, Status: DashboardRefreshSucceeded,
		Items: []DashboardSnapshot{{RunID: uuid.New(), Dashboard: report.Dashboard{ReportKey: report.StockBalance, Version: "1.0.0"}}},
	}}
	service := NewReportService(&fakeReportAccess{allowed: true, tenants: []TenantAccess{{
		ID: tenantID, Timezone: "Asia/Bangkok", ReportKeys: []report.Key{report.SalesGoodsServices},
	}}}, store, time.Now)
	if _, err := service.GetDashboardRefreshResult(context.Background(), recipientID, tenantID, refreshID); !errors.Is(err, ErrReportForbidden) {
		t.Fatalf("GetDashboardRefreshResult() error = %v", err)
	}
}
