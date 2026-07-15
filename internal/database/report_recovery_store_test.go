package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRecoverExpiredLeasesCommitsAndFailsScheduledOccurrence(t *testing.T) {
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
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	claimedTenantID, scheduledTenantID := uuid.New(), uuid.New()
	defer func() {
		_, _ = pool.Exec(context.Background(), `delete from tenants where id = any($1::uuid[])`, []uuid.UUID{claimedTenantID, scheduledTenantID})
	}()
	for _, item := range []struct {
		id   uuid.UUID
		slug string
	}{
		{claimedTenantID, "recover-claimed-" + claimedTenantID.String()},
		{scheduledTenantID, "recover-scheduled-" + scheduledTenantID.String()},
	} {
		if _, err := pool.Exec(ctx, `
			insert into tenants (id, slug, name, timezone, status, access_ends_at)
			values ($1, $2, 'Recovery', 'Asia/Bangkok', 'ACTIVE', $3)`, item.id, item.slug, now.AddDate(1, 0, 0)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			insert into tenant_sml_connections (
			  tenant_id, endpoint_url, database_name, config_file_name,
			  username_ciphertext, username_nonce, password_ciphertext, password_nonce,
			  encryption_key_id, readiness_status, last_tested_at
			) values ($1, 'https://sml.example.com:8443', 'DATA', 'SMLConfigDATA.xml',
			          decode('11','hex'), decode('12','hex'), decode('13','hex'), decode('14','hex'),
			          'key', 'READY', $2)`, item.id, now); err != nil {
			t.Fatal(err)
		}
	}

	claimedRunID := uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status,
		  period_preset, period_from, period_to, claimed_by, claimed_at,
		  lease_expires_at, queued_at, expires_at, result_kind, priority
		) values ($1, $2, 'sales_goods_services', 'BACKGROUND', $3, 'CLAIMED',
		          'YESTERDAY', '2026-07-14', '2026-07-14', 'dead-worker', $4,
		          $5, $4, $6, 'SUMMARY', 30)`,
		claimedRunID, claimedTenantID, "recover-claimed-"+claimedRunID.String(), now.Add(-2*time.Minute), now.Add(-time.Minute), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	scheduleID, notificationRunID := uuid.New(), uuid.New()
	failedRunID, siblingRunID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into notification_schedules (
		  id, tenant_id, name, status, local_time, timezone, period_preset, version
		) values ($1, $2, 'Recovery schedule', 'ACTIVE', '17:00', 'Asia/Bangkok', 'YESTERDAY', 1)`, scheduleID, scheduledTenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into notification_runs (
		  id, tenant_id, schedule_id, scheduled_for, status, materialization_version
		) values ($1, $2, $3, $4, 'COLLECTING', 2)`, notificationRunID, scheduledTenantID, scheduleID, now.Add(-5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status,
		  period_preset, period_from, period_to, claimed_by, claimed_at,
		  lease_expires_at, queued_at, expires_at, result_kind, priority
		) values
		  ($1, $3, 'stock_balance', 'SCHEDULE', $4, 'RUNNING',
		   'YESTERDAY', '2026-07-14', '2026-07-14', 'dead-worker', $6, $7, $6, $8, 'SUMMARY', 100),
		  ($2, $3, 'sales_goods_services', 'SCHEDULE', $5, 'QUEUED',
		   'YESTERDAY', '2026-07-14', '2026-07-14', null, null, null, $6, $8, 'SUMMARY', 100)`,
		failedRunID, siblingRunID, scheduledTenantID,
		"recover-running-"+failedRunID.String(), "recover-sibling-"+siblingRunID.String(),
		now.Add(-2*time.Minute), now.Add(-time.Minute), now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into notification_run_reports (notification_run_id, report_key, report_run_id, position)
		values ($1, 'stock_balance', $2, 1), ($1, 'sales_goods_services', $3, 2)`, notificationRunID, failedRunID, siblingRunID); err != nil {
		t.Fatal(err)
	}

	store := NewReportStore(pool)
	recovered, err := store.RecoverExpiredLeases(ctx, now)
	if err != nil {
		t.Fatalf("RecoverExpiredLeases() error = %v", err)
	}
	if recovered.RequeuedClaimed < 1 || recovered.FailedRunning < 1 || recovered.CancelledSiblings < 1 {
		t.Fatalf("recovered = %+v", recovered)
	}
	var claimedStatus, failedStatus, failedCode, siblingStatus, notificationStatus, notificationCode string
	if err := pool.QueryRow(ctx, `select status from report_runs where id = $1`, claimedRunID).Scan(&claimedStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select status, safe_error_code from report_runs where id = $1`, failedRunID).Scan(&failedStatus, &failedCode); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select status from report_runs where id = $1`, siblingRunID).Scan(&siblingStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select status, safe_error_code from notification_runs where id = $1`, notificationRunID).Scan(&notificationStatus, &notificationCode); err != nil {
		t.Fatal(err)
	}
	if claimedStatus != string(report.StatusQueued) || failedStatus != string(report.StatusFailed) || failedCode != "REPORT_LEASE_EXPIRED" || siblingStatus != string(report.StatusCancelled) || notificationStatus != "FAILED" || notificationCode != "REPORT_SET_INCOMPLETE" {
		t.Fatalf("claimed=%s failed=%s/%s sibling=%s notification=%s/%s", claimedStatus, failedStatus, failedCode, siblingStatus, notificationStatus, notificationCode)
	}
	var circuitOpen bool
	if err := pool.QueryRow(ctx, `select open_until > $2 from tenant_sml_circuits where tenant_id = $1`, scheduledTenantID, now).Scan(&circuitOpen); err != nil || !circuitOpen {
		t.Fatalf("tenant circuit open=%v err=%v", circuitOpen, err)
	}
	// A second pass is idempotent and cannot create duplicate terminal effects.
	second, err := store.RecoverExpiredLeases(ctx, now.Add(time.Second))
	if err != nil || second != (report.LeaseRecovery{}) {
		t.Fatalf("second recovery = %+v, %v", second, err)
	}
}
