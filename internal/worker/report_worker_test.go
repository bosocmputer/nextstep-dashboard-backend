package worker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
)

type fakeRunStore struct {
	run              report.Run
	claimErr         error
	completed        *report.SummaryResult
	persistRows      bool
	retriedCode      string
	failedCode       string
	failCalls        int
	remoteFailCalls  int
	preflightRetries int
	preflightFails   int
	markRunning      int
	phases           []report.ProgressPhase
	uncertaintyUntil *time.Time
	chunkManifests   []report.ChunkManifest
	completedChunks  int
	extendLeaseErr   error
	extendLeaseCalls int
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
	store.extendLeaseCalls++
	return store.extendLeaseErr
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
func (store *fakeRunStore) RetryPreRequestFailure(_ context.Context, _ uuid.UUID, _, safeCode string, _, _ time.Time) error {
	store.preflightRetries++
	store.retriedCode = safeCode
	return nil
}
func (store *fakeRunStore) Fail(_ context.Context, _ uuid.UUID, _, safeCode, _ string, _ time.Time) error {
	store.failCalls++
	store.failedCode = safeCode
	return nil
}
func (store *fakeRunStore) FailRemoteUnknown(_ context.Context, _ uuid.UUID, _, safeCode, _ string, _ time.Time, until time.Time) error {
	store.remoteFailCalls++
	store.failedCode = safeCode
	store.uncertaintyUntil = &until
	return nil
}
func (store *fakeRunStore) FailPreRequestFailure(_ context.Context, _ uuid.UUID, _, safeCode, _ string, _ time.Time) error {
	store.preflightFails++
	store.failedCode = safeCode
	return nil
}
func (store *fakeRunStore) PrepareChunks(_ context.Context, _ uuid.UUID, _ string, manifests []report.ChunkManifest, _ time.Time) error {
	store.chunkManifests = manifests
	return nil
}
func (store *fakeRunStore) StartChunk(context.Context, uuid.UUID, string, int, time.Time) error {
	return nil
}
func (store *fakeRunStore) CompleteChunk(_ context.Context, _ uuid.UUID, _ string, _ int, _ any, _ int, _ time.Time) error {
	store.completedChunks++
	return nil
}
func (store *fakeRunStore) FailChunk(context.Context, uuid.UUID, string, int, string, time.Time) error {
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

func TestReportWorkerStopsWhenLeaseHeartbeatFails(t *testing.T) {
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	leaseErr := errors.New("lease ownership lost")
	store := &fakeRunStore{extendLeaseErr: leaseErr, run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.CashBankReceipts,
		Source: report.SourceSchedule, ResultKind: report.ResultSummary, Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-14", DateTo: "2026-07-14"},
	}}
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(ctx context.Context, _ sml.Connection, _ string) ([]map[string]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}), "lease-worker", func() time.Time { return now })
	worker.heartbeatInterval = time.Millisecond

	err := worker.ProcessOne(context.Background())
	if err == nil || !strings.Contains(err.Error(), leaseErr.Error()) {
		t.Fatalf("ProcessOne() error = %v, want lease failure", err)
	}
	if store.extendLeaseCalls == 0 || store.completed != nil || store.failCalls != 0 || store.remoteFailCalls != 0 {
		t.Fatalf("lease calls=%d completed=%+v fail=%d remote_fail=%d", store.extendLeaseCalls, store.completed, store.failCalls, store.remoteFailCalls)
	}
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

func TestReportWorkerRetriesOnlyOneInteractiveFailureBeforeRequestWasSent(t *testing.T) {
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		source    report.Source
		wantRetry int
		wantFail  int
	}{
		{name: "viewer retries once", source: report.SourceDashboard, wantRetry: 1},
		{name: "schedule fails occurrence immediately", source: report.SourceSchedule, wantFail: 1},
		{name: "background waits for next policy cycle", source: report.SourceBackground, wantFail: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeRunStore{run: report.Run{
				ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.CashBankReceipts,
				Source: test.source, ResultKind: report.ResultSummary, Status: report.StatusClaimed, Attempt: 1,
				Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-14", DateTo: "2026-07-14"},
			}}
			worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
				return sml.Connection{}, nil
			}), queryClientFunc(func(context.Context, sml.Connection, string) ([]map[string]string, error) {
				return nil, &sml.SafeError{Code: "SML_UNREACHABLE", Retryable: true, Phase: sml.BeforeRequestSent}
			}), "worker-a", func() time.Time { return now })

			if err := worker.ProcessOne(context.Background()); err != nil {
				t.Fatalf("ProcessOne() error = %v", err)
			}
			if store.preflightRetries != test.wantRetry || store.preflightFails != test.wantFail || store.remoteFailCalls != 0 || store.failCalls != 0 {
				t.Fatalf("preflightRetries=%d preflightFails=%d remote=%d plain=%d", store.preflightRetries, store.preflightFails, store.remoteFailCalls, store.failCalls)
			}
		})
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

func TestReportWorkerUsesBoundedSummaryPlanAndNeverPersistsRawRows(t *testing.T) {
	now := time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.StockBalance,
		Source: report.SourceDashboard, ResultKind: report.ResultSummary,
		Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.AsOfRun, DateFrom: "2026-07-12", DateTo: "2026-07-12"},
	}}
	boundedSQL := false
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(_ context.Context, _ sml.Connection, sql string) ([]map[string]string, error) {
		boundedSQL = strings.Contains(sql, "_metric_item_count") && strings.Contains(sql, "limit 20")
		return []map[string]string{{
			"ic_code": "I1", "ic_name": "สินค้า", "balance_amount": "100", "amount_in": "10", "amount_out": "5",
			"_metric_item_count": "500", "_metric_balance_amount": "10000", "_metric_amount_in": "1000", "_metric_amount_out": "500", "_metric_row_count": "500",
		}}, nil
	}), "worker-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if !boundedSQL || store.persistRows || store.completed == nil {
		t.Fatalf("bounded=%v persist=%v completed=%+v", boundedSQL, store.persistRows, store.completed)
	}
	if store.completed.RowCount != 500 || store.completed.Metrics["item_count"] != "500" || store.completed.Metrics["balance_amount"] != "10000.00" {
		t.Fatalf("summary = %+v", store.completed)
	}
}

func TestHeavySummarySharesOneFiveMinuteDeadlineAcrossCurrentAndComparison(t *testing.T) {
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: uuid.New(), ReportKey: report.StockBalance,
		Source: report.SourceSchedule, ResultKind: report.ResultSummary,
		Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.AsOfRun, DateFrom: "2026-07-15", DateTo: "2026-07-15"},
	}}
	deadlines := make([]time.Time, 0, 2)
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(ctx context.Context, _ sml.Connection, _ string) ([]map[string]string, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("heavy summary query has no deadline")
		}
		deadlines = append(deadlines, deadline)
		return []map[string]string{{
			"ic_code": "I1", "ic_name": "สินค้า", "balance_amount": "100", "amount_in": "10", "amount_out": "5",
			"_metric_item_count": "1", "_metric_balance_amount": "100", "_metric_amount_in": "10", "_metric_amount_out": "5", "_metric_row_count": "1",
		}}, nil
	}), "worker-a", func() time.Time { return now })

	started := time.Now()
	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(deadlines) != 2 {
		t.Fatalf("query deadlines = %v, want current and comparison", deadlines)
	}
	if delta := deadlines[1].Sub(deadlines[0]); delta < -10*time.Millisecond || delta > 10*time.Millisecond {
		t.Fatalf("current/comparison deadlines differ by %v", delta)
	}
	remaining := deadlines[0].Sub(started)
	if remaining < 4*time.Minute+55*time.Second || remaining > 5*time.Minute+time.Second {
		t.Fatalf("heavy total deadline remaining = %v", remaining)
	}
}

func TestReportWorkerRunsApprovedHeavyTargetInPersistedChunks(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: tenantID, ReportKey: report.StockBalance,
		Source: report.SourceBackground, ResultKind: report.ResultSummary,
		Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.AsOfRun, DateFrom: "2026-07-14", DateTo: "2026-07-14"},
	}}
	queries := 0
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(_ context.Context, _ sml.Connection, sql string) ([]map[string]string, error) {
		queries++
		if strings.Contains(sql, "select code as unit_key") {
			return []map[string]string{{"unit_key": "001"}, {"unit_key": "002"}}, nil
		}
		return []map[string]string{{
			"ic_code": "001", "ic_name": "สินค้า", "balance_amount": "100", "amount_in": "10", "amount_out": "5",
			"_metric_item_count": "2", "_metric_balance_amount": "100", "_metric_amount_in": "10", "_metric_amount_out": "5", "_metric_row_count": "2",
		}}, nil
	}), "worker-a", func() time.Time { return now }).
		ConfigureHeavyChunks(true, false, []string{tenantID.String() + "/stock_balance"})

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(store.chunkManifests) != 1 || store.completedChunks != 1 || store.completed == nil || store.persistRows || queries != 3 {
		t.Fatalf("manifest=%+v completedChunks=%d completed=%+v persist=%v queries=%d", store.chunkManifests, store.completedChunks, store.completed, store.persistRows, queries)
	}
}

func TestReportWorkerStopsChunksAndOpensCircuitWhenRemoteStateIsUnknown(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	store := &fakeRunStore{run: report.Run{
		ID: uuid.New(), TenantID: tenantID, ReportKey: report.StockBalance,
		Source: report.SourceBackground, ResultKind: report.ResultSummary,
		Status: report.StatusClaimed, Attempt: 1,
		Period: report.Period{Preset: report.AsOfRun, DateFrom: "2026-07-14", DateTo: "2026-07-14"},
	}}
	queries := 0
	worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
		return sml.Connection{}, nil
	}), queryClientFunc(func(_ context.Context, _ sml.Connection, sql string) ([]map[string]string, error) {
		queries++
		if queries == 1 {
			manifest := make([]map[string]string, 501)
			for index := range manifest {
				manifest[index] = map[string]string{"unit_key": fmt.Sprintf("%03d", index+1)}
			}
			return manifest, nil
		}
		return nil, &sml.SafeError{Code: "SML_TIMEOUT", Retryable: true, Phase: sml.RequestSentResultUnknown}
	}), "worker-a", func() time.Time { return now }).
		ConfigureHeavyChunks(true, false, []string{tenantID.String() + "/stock_balance"})

	if err := worker.ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if queries != 2 || store.uncertaintyUntil == nil || store.retriedCode != "" || store.failedCode != "SML_TIMEOUT" || store.completed != nil || store.remoteFailCalls != 1 || store.failCalls != 0 {
		t.Fatalf("queries=%d circuit=%v retry=%q fail=%q atomicFail=%d plainFail=%d completed=%+v", queries, store.uncertaintyUntil, store.retriedCode, store.failedCode, store.remoteFailCalls, store.failCalls, store.completed)
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

func TestReportWorkerAcceptsScientificNotationFromDATA1DetailFallback(t *testing.T) {
	now := time.Date(2026, 7, 15, 7, 22, 0, 0, time.UTC)
	tests := []struct {
		key    report.Key
		period report.Period
		rows   []map[string]string
	}{
		{
			key: report.StockBalance,
			period: report.Period{
				Preset: report.Yesterday, DateFrom: "2026-07-14", DateTo: "2026-07-14",
			},
			rows: []map[string]string{{
				"ic_code": "A", "ic_name": "สินค้า A", "balance_amount": "-5.00E-13", "balance_qty": "0E-20",
				"qty_in": "0E-14", "amount_in": "0", "qty_out": "0E-15", "amount_out": "0",
			}},
		},
		{
			key: report.StockReorder,
			period: report.Period{
				Preset: report.AsOfRun, DateFrom: "2026-07-15", DateTo: "2026-07-15",
			},
			rows: []map[string]string{{
				"ic_code": "A", "ic_name": "สินค้า A", "balance_qty": "0E-22", "purchase_point": "5.0000", "purchase_balance_qty": "0E-14",
			}},
		},
	}
	for _, test := range tests {
		t.Run(string(test.key), func(t *testing.T) {
			store := &fakeRunStore{run: report.Run{
				ID: uuid.New(), TenantID: uuid.New(), ReportKey: test.key, Source: report.SourceSchedule,
				ResultKind: report.ResultSummary, Status: report.StatusClaimed, Attempt: 1, Period: test.period,
			}}
			worker := NewReportWorker(store, connectionProviderFunc(func(context.Context, uuid.UUID) (sml.Connection, error) {
				return sml.Connection{}, nil
			}), queryClientFunc(func(context.Context, sml.Connection, string) ([]map[string]string, error) {
				return test.rows, nil
			}), "worker-a", func() time.Time { return now }).ConfigureSummaryQueries(false)

			if err := worker.ProcessOne(context.Background()); err != nil {
				t.Fatalf("ProcessOne() error = %v", err)
			}
			if store.failedCode != "" || store.completed == nil || store.completed.Dashboard == nil {
				t.Fatalf("failed=%q completed=%+v", store.failedCode, store.completed)
			}
		})
	}
}

func TestSafeFailureMessageExplainsReportOutputAndIncompleteSet(t *testing.T) {
	tests := map[string]string{
		"REPORT_OUTPUT_INVALID":  "ข้อมูลตัวเลขจาก SML อยู่ในรูปแบบที่ระบบไม่รองรับ",
		"REPORT_SET_INCOMPLETE":  "สร้างรายงานในรอบนี้ไม่ครบ ระบบจึงไม่ส่ง LINE",
		"SML_ZIP_FORMAT_INVALID": "Server ลูกค้าส่งผลลัพธ์กลับมาในรูปแบบ ZIP ที่ไม่ถูกต้อง",
		"SML_ZIP_EMPTY":          "Server ลูกค้าส่งผลลัพธ์ ZIP ที่ไม่มีข้อมูลกลับมา",
		"SML_ZIP_TOO_LARGE":      "ผลลัพธ์จาก Server ลูกค้ามีขนาดใหญ่เกินขอบเขตที่ปลอดภัย",
		"SML_ZIP_READ_FAILED":    "ระบบอ่านผลลัพธ์ ZIP จาก Server ลูกค้าไม่สำเร็จ",
		"SML_ZIP_INVALID":        "ผลลัพธ์ ZIP จาก Server ลูกค้าไม่สมบูรณ์",
	}
	for code, expected := range tests {
		if got := safeFailureMessage(code); got != expected {
			t.Fatalf("safeFailureMessage(%q) = %q, want %q", code, got, expected)
		}
	}
}
