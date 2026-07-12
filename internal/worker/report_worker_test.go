package worker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
)

type fakeRunStore struct {
	run         report.Run
	claimErr    error
	completed   *report.SummaryResult
	persistRows bool
	retriedCode string
	failedCode  string
	markRunning int
	phases      []report.ProgressPhase
}

func (store *fakeRunStore) Claim(context.Context, string, time.Duration, time.Time) (report.Run, error) {
	return store.run, store.claimErr
}
func (store *fakeRunStore) MarkRunning(context.Context, uuid.UUID, string, time.Duration, time.Time) error {
	store.markRunning++
	return nil
}
func (store *fakeRunStore) UpdateProgress(_ context.Context, _ uuid.UUID, _ string, phase report.ProgressPhase, _, _ int, _ time.Time) error {
	store.phases = append(store.phases, phase)
	return nil
}
func (store *fakeRunStore) ExtendLease(context.Context, uuid.UUID, string, time.Duration, time.Time) error {
	return nil
}
func (store *fakeRunStore) Complete(_ context.Context, _ uuid.UUID, _ string, summary report.SummaryResult, persistRows bool, _ time.Time) error {
	store.completed = &summary
	store.persistRows = persistRows
	return nil
}
func (store *fakeRunStore) Retry(_ context.Context, _ uuid.UUID, _, safeCode string, _, _ time.Time) error {
	store.retriedCode = safeCode
	return nil
}
func (store *fakeRunStore) Fail(_ context.Context, _ uuid.UUID, _, safeCode, _ string, _ time.Time) error {
	store.failedCode = safeCode
	return nil
}

type connectionProviderFunc func(context.Context, uuid.UUID) (sml.Connection, error)

func (provider connectionProviderFunc) Open(ctx context.Context, tenantID uuid.UUID) (sml.Connection, error) {
	return provider(ctx, tenantID)
}

type queryClientFunc func(context.Context, sml.Connection, string) ([]map[string]string, error)

func (query queryClientFunc) Query(ctx context.Context, connection sml.Connection, sql string) ([]map[string]string, error) {
	return query(ctx, connection, sql)
}

func TestReportWorkerCompletesFreshDashboardRunAndPersistsRows(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.SalesGoodsServices,
		Source: report.SourceDashboard, Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-10"},
	}}
	queries := 0
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{EndpointURL: "http://10.0.0.8/service"}, nil
	}), queryClientFunc(func(_ context.Context, _ sml.Connection, sql string) ([]map[string]string, error) {
		queries++
		if queries%2 == 1 {
			return []map[string]string{{"doc_date": "2026-07-01", "doc_no": "S1", "total_amount": "30.00"}}, nil
		}
		return []map[string]string{{"doc_date": "2026-07-01", "doc_no": "S1", "item_code": "I1", "item_name": "สินค้า 1", "sum_amount": "30.00"}}, nil
	}), "worker-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if store.markRunning != 1 || store.completed == nil || !store.persistRows || store.completed.Metrics["total_amount"] != "30.00" || queries != 4 {
		t.Fatalf("worker result: running=%d completed=%+v persist=%v queries=%d", store.markRunning, store.completed, store.persistRows, queries)
	}
	if store.completed.Dashboard == nil || len(store.completed.Dashboard.KPIs) < 2 || len(store.completed.Dashboard.Visualizations) == 0 {
		t.Fatalf("dashboard result = %+v", store.completed.Dashboard)
	}
}

func TestReportWorkerDoesNotRetryTimeoutAfterQueryDispatch(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.CashBankReceipts,
		Source: report.SourceSchedule, Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
	}}
	deadlineChecked := false
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(ctx context.Context, _ sml.Connection, _ string) ([]map[string]string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < 25*time.Second || time.Until(deadline) > 31*time.Second {
			t.Fatalf("scheduled query deadline = %v", deadline)
		}
		deadlineChecked = true
		return nil, &sml.SafeError{Code: "SML_TIMEOUT", Retryable: true}
	}), "worker-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if !deadlineChecked || store.retriedCode != "" || store.failedCode != "SML_TIMEOUT" || store.completed != nil {
		t.Fatalf("retry=%q failed=%q completed=%+v", store.retriedCode, store.failedCode, store.completed)
	}
}

func TestReportWorkerPublishesMonotonicHonestProgressPhases(t *testing.T) {
	now := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.StockReorder,
		Source: report.SourceBackground, ResultKind: report.ResultSummary,
		Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.AsOfRun, DateFrom: "2026-07-12", DateTo: "2026-07-12"},
	}}
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(context.Context, sml.Connection, string) ([]map[string]string, error) {
		return []map[string]string{{"ic_code": "I1", "ic_name": "สินค้า", "balance_qty": "0", "purchase_point": "1"}}, nil
	}), "worker-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	want := []report.ProgressPhase{report.ProgressConnecting, report.ProgressQueryingCurrent, report.ProgressBuildingDashboard, report.ProgressSavingResult}
	if !reflect.DeepEqual(store.phases, want) {
		t.Fatalf("progress phases = %v, want %v", store.phases, want)
	}
	if store.persistRows {
		t.Fatal("background summary persisted detail rows")
	}
}

func TestReportWorkerKeepsCurrentDashboardWhenComparisonQueryFails(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.SalesGoodsServices,
		Source: report.SourceDashboard, Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
	}}
	queries := 0
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(context.Context, sml.Connection, string) ([]map[string]string, error) {
		queries++
		switch queries {
		case 1:
			return []map[string]string{{"doc_date": "2026-07-09", "doc_no": "S1", "total_amount": "30.00"}}, nil
		case 2:
			return []map[string]string{{"doc_date": "2026-07-09", "doc_no": "S1", "item_code": "I1", "item_name": "สินค้า 1", "sum_amount": "30.00"}}, nil
		default:
			return nil, &sml.SafeError{Code: "SML_TIMEOUT", Retryable: true}
		}
	}), "worker-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if store.completed == nil || store.failedCode != "" || store.retriedCode != "" || queries != 3 {
		t.Fatalf("completed=%+v failed=%q retried=%q queries=%d", store.completed, store.failedCode, store.retriedCode, queries)
	}
	if store.completed.Dashboard == nil || store.completed.Dashboard.Quality.Status != "WARNING" || store.completed.Dashboard.KPIs[0].Comparison.Availability != report.ComparisonUnavailable {
		t.Fatalf("dashboard = %+v", store.completed.Dashboard)
	}
}

func TestReportWorkerDoesNotPublishFullPriorDayAsTodayToNowComparison(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.SalesGoodsServices,
		Source: report.SourceDashboard, Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.TodayToNow, DateFrom: "2026-07-10", DateTo: "2026-07-10"},
	}}
	queries := 0
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(context.Context, sml.Connection, string) ([]map[string]string, error) {
		queries++
		if queries == 1 {
			return []map[string]string{{"doc_date": "2026-07-10", "doc_no": "S1", "total_amount": "30.00"}}, nil
		}
		return []map[string]string{{"doc_date": "2026-07-10", "doc_no": "S1", "item_code": "I1", "item_name": "สินค้า 1", "sum_amount": "30.00"}}, nil
	}), "worker-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if store.completed == nil || store.completed.Dashboard == nil || queries != 2 {
		t.Fatalf("completed=%+v queries=%d", store.completed, queries)
	}
	dashboard := store.completed.Dashboard
	if dashboard.Quality.Status != "OK" || len(dashboard.Quality.Warnings) != 0 {
		t.Fatalf("quality = %+v", dashboard.Quality)
	}
	for _, metric := range dashboard.KPIs {
		if metric.Comparison.Availability != report.ComparisonUnavailable {
			t.Fatalf("comparison = %+v", metric.Comparison)
		}
	}
	for _, visualization := range dashboard.Visualizations {
		for _, series := range visualization.Series {
			if series.Key == "previous" {
				t.Fatalf("misleading previous-day series published: %+v", visualization)
			}
		}
	}
}

func TestReportWorkerDoesNotQueryHistoricalComparisonForCurrentOnlyReorder(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.StockReorder,
		Source: report.SourceDashboard, Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.AsOfRun, DateFrom: "2026-07-10", DateTo: "2026-07-10"},
	}}
	queries := 0
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(context.Context, sml.Connection, string) ([]map[string]string, error) {
		queries++
		return []map[string]string{{"ic_code": "I1", "ic_name": "สินค้า 1", "balance_qty": "2", "purchase_point": "5"}}, nil
	}), "worker-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if store.completed == nil || store.completed.Dashboard == nil || queries != 1 {
		t.Fatalf("completed=%+v queries=%d", store.completed, queries)
	}
	if got := store.completed.Dashboard.KPIs[0].Comparison.Availability; got != report.ComparisonUnavailable {
		t.Fatalf("comparison availability = %s", got)
	}
}

func TestReportWorkerFailsMalformedOutputAndSurfacesEmptyQueue(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.CashBankPayments,
		Source: report.SourceDashboard, Status: report.StatusClaimed, Attempt: 3,
		Period: report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-10"},
	}}
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(context.Context, sml.Connection, string) ([]map[string]string, error) {
		return []map[string]string{{"total_amount": "bad"}}, nil
	}), "worker-a", func() time.Time { return now })
	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if store.failedCode != "REPORT_OUTPUT_INVALID" {
		t.Fatalf("failed code = %q", store.failedCode)
	}

	emptyWorker := NewReportWorker(&fakeRunStore{claimErr: report.ErrNoQueuedRun}, nil, nil, "worker-a", func() time.Time { return now })
	if err := emptyWorker.ProcessOne(context.Background()); !errors.Is(err, report.ErrNoQueuedRun) {
		t.Fatalf("empty queue error = %v", err)
	}
}
