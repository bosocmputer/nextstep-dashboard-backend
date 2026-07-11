package database

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/delivery"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/notification"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMaterializeDueScheduleIsAtomicAndSingleClaim(t *testing.T) {
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
	tenantID, recipientID, secondRecipientID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at)
		values ($1, $2, 'Execution Store', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "execution-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_sml_connections (
		  tenant_id, endpoint_url, database_name, username_ciphertext, username_nonce,
		  password_ciphertext, password_nonce, encryption_key_id, readiness_status, last_tested_at
		) values ($1, 'http://10.0.0.9/service', 'shop', decode('41','hex'), decode('42','hex'), decode('43','hex'), decode('44','hex'), 'key', 'READY', $2)`, tenantID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into line_recipients (id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, display_name_ciphertext, display_name_nonce, encryption_key_id, status, verified_at)
		values ($1, $3, decode('45','hex'), decode('46','hex'), decode('47','hex'), decode('48','hex'), 'key', 'ACTIVE', $4),
		       ($2, $5, decode('49','hex'), decode('4a','hex'), decode('4b','hex'), decode('4c','hex'), 'key', 'ACTIVE', $4)`,
		recipientID, secondRecipientID, recipientID[:], now, secondRecipientID[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into tenant_memberships (tenant_id, recipient_id, status) values ($1, $2, 'ACTIVE'), ($1, $3, 'ACTIVE')`, tenantID, recipientID, secondRecipientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into recipient_report_permissions (tenant_id, recipient_id, report_key)
		values ($1, $2, 'sales_goods_services'), ($1, $2, 'stock_balance'),
		       ($1, $3, 'sales_goods_services'), ($1, $3, 'stock_balance')`, tenantID, recipientID, secondRecipientID); err != nil {
		t.Fatal(err)
	}
	store := NewScheduleStore(pool)
	input := schedule.Input{
		Name: "Due Morning", DaysOfWeek: []int{int(now.In(time.FixedZone("ICT", 7*60*60)).Weekday())},
		LocalTime: "14:59", Timezone: "Asia/Bangkok", PeriodPreset: report.Yesterday,
		ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance}, RecipientIDs: []uuid.UUID{recipientID, secondRecipientID},
	}
	created, err := store.Create(ctx, []byte("admin"), "create-due", "schedule-due-001", tenantID, input, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	scheduledFor := now.Add(-time.Minute)
	if _, err := store.Activate(ctx, []byte("admin"), "activate-due", tenantID, created.ID, scheduledFor, now.Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	const contenders = 2
	results := make(chan schedule.Execution, contenders)
	errorsSeen := make(chan error, contenders)
	var wait sync.WaitGroup
	for contender := 0; contender < contenders; contender++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			execution, err := store.MaterializeDue(ctx, "scheduler-"+string(rune('a'+index)), now)
			results <- execution
			errorsSeen <- err
		}(contender)
	}
	wait.Wait()
	close(results)
	close(errorsSeen)
	successes, emptyClaims := 0, 0
	var execution schedule.Execution
	for err := range errorsSeen {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, schedule.ErrNoDueSchedule):
			emptyClaims++
		default:
			t.Fatalf("MaterializeDue() error = %v", err)
		}
	}
	for result := range results {
		if result.ID != uuid.Nil {
			execution = result
		}
	}
	if successes != 1 || emptyClaims != 1 || execution.Status != schedule.ExecutionCollecting || len(execution.ReportRunIDs) != 2 {
		t.Fatalf("successes=%d empty=%d execution=%+v", successes, emptyClaims, execution)
	}
	var notificationCount, reportCount int
	if err := pool.QueryRow(ctx, `select count(*) from notification_runs where schedule_id = $1`, created.ID).Scan(&notificationCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from report_runs where tenant_id = $1 and source = 'SCHEDULE'`, tenantID).Scan(&reportCount); err != nil {
		t.Fatal(err)
	}
	if notificationCount != 1 || reportCount != 2 {
		t.Fatalf("notificationCount=%d reportCount=%d", notificationCount, reportCount)
	}
	for _, runID := range execution.ReportRunIDs {
		var key report.Key
		if err := pool.QueryRow(ctx, `select report_key from report_runs where id = $1`, runID).Scan(&key); err != nil {
			t.Fatal(err)
		}
		metrics := map[string]string{}
		for index, metric := range mustDefinition(t, key).LineMetrics {
			metrics[metric.Key] = []string{"2", "30.00"}[index]
		}
		metricsJSON, _ := json.Marshal(metrics)
		if _, err := pool.Exec(ctx, `
			update report_runs
			set status = 'SUCCEEDED', summary_json = $2, row_count = 2, finished_at = $3, expires_at = $4, updated_at = $3
			where id = $1`, runID, metricsJSON, now.Add(time.Second), now.Add(90*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	notificationStore := NewNotificationStore(pool)
	work, err := notificationStore.Claim(ctx, "notification-a", time.Minute, now.Add(6*time.Second))
	if err != nil || len(work.Reports) != 2 || len(work.Targets) != 2 || work.Pending {
		t.Fatalf("Claim() = %+v, %v", work, err)
	}
	if _, err := pool.Exec(ctx, `
		delete from recipient_report_permissions
		where tenant_id = $1 and recipient_id = $2 and report_key = 'stock_balance'`, tenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	prepared := make([]notification.PreparedDelivery, 0, len(work.Targets))
	for _, target := range work.Targets {
		prepared = append(prepared, notification.PreparedDelivery{
			ID: uuid.New(), RecipientID: target.RecipientID, RetryKey: uuid.New(),
			ReferenceHash: []byte("reference-" + target.RecipientID.String()), Payload: json.RawMessage(`{"type":"flex"}`),
			ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance},
		})
	}
	if err := notificationStore.Publish(ctx, work.ID, "notification-a", prepared, false, now.Add(7*time.Second)); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	var deliveryCount, outboxCount, linkCount int
	var deliveredRecipientID uuid.UUID
	if err := pool.QueryRow(ctx, `select count(*), min(recipient_id::text)::uuid from line_deliveries where notification_run_id = $1`, work.ID).Scan(&deliveryCount, &deliveredRecipientID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from line_delivery_outbox where tenant_id = $1`, tenantID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from delivery_access_links where tenant_id = $1`, tenantID).Scan(&linkCount); err != nil {
		t.Fatal(err)
	}
	if deliveryCount != 1 || outboxCount != 1 || linkCount != 1 || deliveredRecipientID != secondRecipientID {
		t.Fatalf("deliveryCount=%d outboxCount=%d linkCount=%d recipient=%s", deliveryCount, outboxCount, linkCount, deliveredRecipientID)
	}
	deliveryStore := NewDeliveryStore(pool)
	deliveryWork, err := deliveryStore.Claim(ctx, "delivery-a", time.Minute, now.Add(8*time.Second))
	if err != nil || deliveryWork.RetryKey == uuid.Nil || deliveryWork.Attempt != 1 || !deliveryWork.TenantActive {
		t.Fatalf("delivery Claim() = %+v, %v", deliveryWork, err)
	}
	persistedRetryKey := deliveryWork.RetryKey
	if err := deliveryStore.Retry(ctx, deliveryWork.ID, "delivery-a", "LINE_PUSH_UNCERTAIN", true, now.Add(9*time.Second), now.Add(8*time.Second)); err != nil {
		t.Fatalf("delivery Retry() error = %v", err)
	}
	deliveryWork, err = deliveryStore.Claim(ctx, "delivery-b", time.Minute, now.Add(10*time.Second))
	if err != nil || deliveryWork.RetryKey != persistedRetryKey || deliveryWork.Attempt != 2 {
		t.Fatalf("delivery retry Claim() = %+v, %v", deliveryWork, err)
	}
	if err := deliveryStore.Accept(ctx, deliveryWork.ID, "delivery-b", "line-request-1", now.Add(11*time.Second)); err != nil {
		t.Fatalf("delivery Accept() error = %v", err)
	}
	var deliveryStatus, notificationStatus string
	var quotaAccepted int
	if err := pool.QueryRow(ctx, `select status from line_deliveries where id = $1`, deliveryWork.ID).Scan(&deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select status from notification_runs where id = $1`, work.ID).Scan(&notificationStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select locally_accepted from line_monthly_quota where quota_month = $1`, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)).Scan(&quotaAccepted); err != nil {
		t.Fatal(err)
	}
	if deliveryStatus != "ACCEPTED" || notificationStatus != "COMPLETED" || quotaAccepted != 1 {
		t.Fatalf("deliveryStatus=%s notificationStatus=%s quotaAccepted=%d", deliveryStatus, notificationStatus, quotaAccepted)
	}
	if _, err := deliveryStore.Claim(ctx, "delivery-c", time.Minute, now.Add(12*time.Second)); !errors.Is(err, delivery.ErrNoDeliveryReady) {
		t.Fatalf("completed outbox reclaimed: %v", err)
	}
	operationsStore := NewOperationsStore(pool)
	reportPage, err := operationsStore.ListReportRuns(ctx, operations.ReportRunFilter{TenantID: &tenantID, PageSize: 25, Now: now.Add(12 * time.Second)})
	if err != nil || len(reportPage.Data) != 2 || reportPage.Data[0].TenantName != "Execution Store" {
		t.Fatalf("ListReportRuns() = %+v, %v", reportPage, err)
	}
	deliveryPage, err := operationsStore.ListDeliveries(ctx, operations.DeliveryFilter{TenantID: &tenantID, PageSize: 25})
	if err != nil || len(deliveryPage.Data) != 1 || deliveryPage.Data[0].Status != "ACCEPTED" ||
		deliveryPage.Data[0].TenantName != "Execution Store" || deliveryPage.Data[0].StoredRecipient.ID != deliveredRecipientID {
		t.Fatalf("ListDeliveries() = %+v, %v", deliveryPage, err)
	}
	auditPage, err := operationsStore.ListAudit(ctx, operations.AuditFilter{TenantID: &tenantID, PageSize: 25})
	if err != nil || len(auditPage.Data) < 2 || auditPage.Data[0].TenantName == nil || *auditPage.Data[0].TenantName != "Execution Store" {
		t.Fatalf("ListAudit() = %+v, %v", auditPage, err)
	}
	testExecution, err := store.MaterializeTest(ctx, []byte("admin"), "test-send-request", "schedule-test-send-001", tenantID, created.ID, now.Add(20*time.Second))
	if err != nil || testExecution.Status != schedule.ExecutionCollecting || len(testExecution.ReportRunIDs) != 2 {
		t.Fatalf("MaterializeTest() = %+v, %v", testExecution, err)
	}
	replayedExecution, err := store.MaterializeTest(ctx, []byte("admin"), "test-send-replay", "schedule-test-send-001", tenantID, created.ID, now.Add(21*time.Second))
	if err != nil || replayedExecution.ID != testExecution.ID {
		t.Fatalf("MaterializeTest() replay = %+v, %v", replayedExecution, err)
	}
	var testAuditCount int
	if err := pool.QueryRow(ctx, `
		select count(*) from audit_logs
		where tenant_id = $1 and action = 'SCHEDULE_TEST_SEND_ENQUEUED'`, tenantID).Scan(&testAuditCount); err != nil || testAuditCount != 1 {
		t.Fatalf("test send audit count=%d err=%v", testAuditCount, err)
	}
}

func mustDefinition(t *testing.T, key report.Key) report.Definition {
	t.Helper()
	definition, ok := report.DefinitionFor(key)
	if !ok {
		t.Fatalf("missing report definition %s", key)
	}
	return definition
}
