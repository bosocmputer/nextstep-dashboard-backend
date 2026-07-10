package worker

import (
	"context"
	"errors"
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
}

func (store *fakeRunStore) Claim(context.Context, string, time.Duration, time.Time) (report.Run, error) {
	return store.run, store.claimErr
}
func (store *fakeRunStore) MarkRunning(context.Context, uuid.UUID, string, time.Duration, time.Time) error {
	store.markRunning++
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

func TestReportWorkerRetriesRetryableSMLFailureWithoutPublishingRows(t *testing.T) {
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
	if !deadlineChecked || store.retriedCode != "SML_TIMEOUT" || store.failedCode != "" || store.completed != nil {
		t.Fatalf("retry=%q failed=%q completed=%+v", store.retriedCode, store.failedCode, store.completed)
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
