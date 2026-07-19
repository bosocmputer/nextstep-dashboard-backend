package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReportStoreIdempotencyLeaseCompletionAndCursorRows(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	recipientID := uuid.New()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1, 'report-store', 'Report Store', 'Asia/Bangkok', 'ACTIVE', $2)`, tenantID, now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into line_recipients (id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, display_name_ciphertext, display_name_nonce, encryption_key_id, status, verified_at)
		values ($1, decode('11', 'hex'), decode('12', 'hex'), decode('13', 'hex'), decode('14', 'hex'), decode('15', 'hex'), 'key', 'ACTIVE', $2)`, recipientID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into tenant_memberships (tenant_id, recipient_id, status) values ($1, $2, 'ACTIVE')`, tenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into recipient_report_permissions (tenant_id, recipient_id, report_key) values ($1, $2, 'sales_goods_services')`, tenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	store := NewReportStore(pool)
	if _, err := store.ListLatestDashboards(ctx, tenantID, []report.Key{report.SalesGoodsServices}); err != nil {
		t.Fatalf("ListLatestDashboards() empty query error = %v", err)
	}
	if _, err := store.GetExactSnapshotsForPeriods(ctx, tenantID, []viewer.SnapshotPeriodRequest{{
		ReportKey: report.SalesGoodsServices,
		Period:    report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-10"},
	}}, now); err != nil {
		t.Fatalf("GetExactSnapshotsForPeriods() empty query error = %v", err)
	}
	input := report.EnqueueInput{
		TenantID: tenantID, ReportKey: report.SalesGoodsServices, Source: report.SourceDashboard,
		IdempotencyKey: "dashboard-open-001", Period: report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-10"},
		RequestedByRecipient: &recipientID,
	}
	created, err := store.Enqueue(ctx, input, now)
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	replayed, err := store.Enqueue(ctx, input, now)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("Enqueue() replay = %+v, %v", replayed, err)
	}
	if _, err := pool.Exec(ctx, `delete from recipient_report_permissions where tenant_id = $1 and recipient_id = $2 and report_key = $3`, tenantID, recipientID, report.SalesGoodsServices); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(ctx, input, now); !errors.Is(err, report.ErrRunForbidden) {
		t.Fatalf("revoked permission replay error = %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into recipient_report_permissions (tenant_id, recipient_id, report_key) values ($1, $2, $3)`, tenantID, recipientID, report.SalesGoodsServices); err != nil {
		t.Fatal(err)
	}
	concurrentInput := input
	concurrentInput.IdempotencyKey = "dashboard-concurrent-001"
	const concurrentRequests = 8
	var wait sync.WaitGroup
	results := make(chan report.Run, concurrentRequests)
	errorsSeen := make(chan error, concurrentRequests)
	for range concurrentRequests {
		wait.Add(1)
		go func() {
			defer wait.Done()
			run, err := store.Enqueue(ctx, concurrentInput, now.Add(time.Second))
			results <- run
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	var concurrentRunID uuid.UUID
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent Enqueue() error = %v", err)
		}
	}
	for result := range results {
		if concurrentRunID == uuid.Nil {
			concurrentRunID = result.ID
		}
		if result.ID != concurrentRunID {
			t.Fatalf("concurrent Enqueue() IDs differ: %s and %s", concurrentRunID, result.ID)
		}
	}
	const concurrentViewers = 20
	backgroundResults := make(chan report.Run, concurrentViewers)
	backgroundErrors := make(chan error, concurrentViewers)
	wait = sync.WaitGroup{}
	for index := range concurrentViewers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			run, enqueueErr := store.Enqueue(ctx, report.EnqueueInput{
				TenantID: tenantID, ReportKey: report.StockBalance, Source: report.SourceBackground,
				ResultKind: report.ResultSummary, Priority: 20, ExecutionKey: "shared-stock-summary-period",
				IdempotencyKey: fmt.Sprintf("background-viewer-%03d", index),
				Period:         report.Period{Preset: report.Custom, DateFrom: "2026-07-10", DateTo: "2026-07-10"},
			}, now.Add(1500*time.Millisecond))
			backgroundResults <- run
			backgroundErrors <- enqueueErr
		}(index)
	}
	wait.Wait()
	close(backgroundResults)
	close(backgroundErrors)
	for enqueueErr := range backgroundErrors {
		if enqueueErr != nil {
			t.Fatalf("coalesced background Enqueue() error = %v", enqueueErr)
		}
	}
	var sharedBackgroundRunID uuid.UUID
	for result := range backgroundResults {
		if sharedBackgroundRunID == uuid.Nil {
			sharedBackgroundRunID = result.ID
		}
		if result.ID != sharedBackgroundRunID {
			t.Fatalf("20 concurrent viewers created multiple background runs: %s and %s", sharedBackgroundRunID, result.ID)
		}
	}
	changed := input
	changed.ReportKey = report.CashBankReceipts
	if _, err := store.Enqueue(ctx, changed, now); !errors.Is(err, report.ErrRunIdempotencyConflict) {
		t.Fatalf("changed idempotency input error = %v", err)
	}
	unauthorizedRecipientID := uuid.New()
	unauthorized := input
	unauthorized.IdempotencyKey = "dashboard-forbidden-001"
	unauthorized.RequestedByRecipient = &unauthorizedRecipientID
	if _, err := store.Enqueue(ctx, unauthorized, now); !errors.Is(err, report.ErrRunForbidden) {
		t.Fatalf("unauthorized dashboard run error = %v", err)
	}

	type claimOutcome struct {
		run report.Run
		err error
	}
	claimOutcomes := make(chan claimOutcome, 2)
	for _, workerID := range []string{"worker-a", "worker-b"} {
		go func(workerID string) {
			claimed, claimErr := store.Claim(ctx, workerID, 30*time.Second, now)
			claimOutcomes <- claimOutcome{run: claimed, err: claimErr}
		}(workerID)
	}
	var claimed report.Run
	noQueueCount := 0
	for range 2 {
		outcome := <-claimOutcomes
		if outcome.err == nil {
			claimed = outcome.run
		} else if errors.Is(outcome.err, report.ErrNoQueuedRun) {
			noQueueCount++
		} else {
			t.Fatalf("concurrent Claim() error = %v", outcome.err)
		}
	}
	if claimed.ID != created.ID || claimed.Status != report.StatusClaimed || claimed.Attempt != 1 || noQueueCount != 1 {
		t.Fatalf("concurrent Claim() = %+v, noQueue=%d", claimed, noQueueCount)
	}
	if err := store.MarkRunning(ctx, claimed.ID, claimed.ClaimedBy, 30*time.Second, now); err != nil {
		t.Fatalf("MarkRunning() error = %v", err)
	}
	summary := report.SummaryResult{
		Metrics:        map[string]string{"document_count": "2", "total_amount": "30.00"},
		Rows:           []map[string]string{{"doc_no": "S1"}, {"doc_no": "S2"}, {"doc_no": "S3"}},
		RowCount:       3,
		Reconciliation: map[string]any{"status": "OK"},
		Dashboard: &report.Dashboard{
			ReportKey: report.SalesGoodsServices, Version: "1.0.0",
			Period: input.Period, ComparisonPeriod: report.Period{Preset: report.Custom, DateFrom: "2026-06-21", DateTo: "2026-06-30"},
			Timezone: "Asia/Bangkok", Quality: report.DashboardQuality{Status: "OK", Warnings: []string{}},
			KPIs: []report.DashboardMetric{{Key: "total_amount", Label: "ยอดขาย", Value: "30.00", Unit: report.UnitTHB, Comparison: report.MetricComparison{Availability: report.ComparisonAvailable, PreviousValue: "20.00"}}},
		},
	}
	if err := store.Complete(ctx, claimed.ID, claimed.ClaimedBy, summary, true, now.Add(time.Second)); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	completed, err := store.Get(ctx, claimed.ID, now.Add(time.Second))
	if err != nil || completed.Status != report.StatusSucceeded || completed.RowCount != 3 {
		t.Fatalf("Get() = %+v, %v", completed, err)
	}
	storedDashboard, err := store.GetDashboard(ctx, tenantID, claimed.ID)
	if err != nil || storedDashboard.ReportKey != report.SalesGoodsServices || len(storedDashboard.KPIs) != 1 || !storedDashboard.GeneratedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("GetDashboard() = %+v, %v", storedDashboard, err)
	}
	firstPage, err := store.ListRows(ctx, claimed.ID, 0, 2, now.Add(time.Second))
	if err != nil || len(firstPage.Rows) != 2 || !firstPage.HasMore || firstPage.NextOrdinal != 2 {
		t.Fatalf("ListRows() first = %+v, %v", firstPage, err)
	}
	secondPage, err := store.ListRows(ctx, claimed.ID, firstPage.NextOrdinal, 2, now.Add(time.Second))
	if err != nil || len(secondPage.Rows) != 1 || secondPage.HasMore {
		t.Fatalf("ListRows() second = %+v, %v", secondPage, err)
	}
	queried, err := store.QueryRows(ctx, claimed.ID, report.RowsQueryInput{
		Filters: []report.RowFilter{{ColumnKey: "doc_no", Operator: report.RowFilterEquals, Value: "S2"}}, Page: 0, PageSize: 25,
	}, now.Add(time.Second))
	if err != nil || queried.Total != 1 || len(queried.Rows) != 1 || queried.Rows[0]["doc_no"] != "S2" {
		t.Fatalf("QueryRows() = %+v, %v", queried, err)
	}
	if _, err := store.ListRows(ctx, claimed.ID, 0, 25, now.Add(25*time.Hour)); !errors.Is(err, report.ErrRunRowsExpired) {
		t.Fatalf("expired ListRows() error = %v", err)
	}
	cancelInput := input
	cancelInput.IdempotencyKey = "dashboard-cancel-001"
	cancelRun, err := store.Enqueue(ctx, cancelInput, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("enqueue cancellation run: %v", err)
	}
	cancelled, err := store.Cancel(ctx, cancelRun.ID, now.Add(3*time.Second))
	if err != nil || cancelled.Status != report.StatusCancelled {
		t.Fatalf("Cancel() = %+v, %v", cancelled, err)
	}
	replayedCancel, err := store.Cancel(ctx, cancelRun.ID, now.Add(4*time.Second))
	if err != nil || replayedCancel.Status != report.StatusCancelled {
		t.Fatalf("Cancel() replay = %+v, %v", replayedCancel, err)
	}
	if _, err := store.Cancel(ctx, completed.ID, now.Add(4*time.Second)); !errors.Is(err, report.ErrRunNotCancellable) {
		t.Fatalf("completed cancellation error = %v", err)
	}
}
