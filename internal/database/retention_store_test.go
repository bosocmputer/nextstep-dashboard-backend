package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/retention"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRetentionStoreApplies24Hour90DayAnd365DayBoundaries(t *testing.T) {
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
	tenantID, dashboardRunID, scheduledRunID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at)
		values ($1, $2, 'Retention', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "retention-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := now.Add(-100 * 24 * time.Hour)
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
		  period_from, period_to, summary_json, reconciliation_json, queued_at, finished_at,
		  expires_at, created_at, updated_at
		) values
		($1, $3, 'sales_goods_services', 'DASHBOARD', 'retention-dashboard-001', 'SUCCEEDED', 'CUSTOM', '2026-01-01', '2026-01-01', '{"total":"1"}', '{"status":"OK"}', $4, $4, $4, $4, $4),
		($2, $3, 'stock_balance', 'SCHEDULE', 'retention-schedule-001', 'SUCCEEDED', 'AS_OF_RUN', '2026-01-01', '2026-01-01', '{"total":"2"}', '{"status":"OK"}', $4, $4, $4, $4, $4)`,
		dashboardRunID, scheduledRunID, tenantID, oldSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into report_run_rows (run_id, tenant_id, ordinal, row_json, created_at, expires_at)
		values ($1, $2, 1, '{"doc":"1"}', $3, $4)`, dashboardRunID, tenantID, now.Add(-48*time.Hour), now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into audit_logs (tenant_id, actor_type, action, resource_type, result, created_at, expires_at)
		values ($1, 'SYSTEM', 'OLD_EVENT', 'TEST', 'SUCCESS', $2, $2)`, tenantID, now.Add(-366*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into idempotency_requests (actor_scope, idempotency_key, request_hash, created_at, expires_at)
		values ('test', 'retention-key', decode('01','hex'), $1, $1)`, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	counts, err := NewRetentionStore(pool).Run(ctx, retention.ProductionPolicy(), now)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if counts.ReportRows != 1 || counts.ReportRuns != 1 || counts.ScrubbedReportRuns != 1 || counts.AuditLogs != 1 || counts.IdempotencyRequests != 1 {
		t.Fatalf("Run() counts = %+v", counts)
	}
	var dashboardExists bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from report_runs where id = $1)`, dashboardRunID).Scan(&dashboardExists); err != nil {
		t.Fatal(err)
	}
	var scheduledSummary string
	if err := pool.QueryRow(ctx, `select summary_json::text from report_runs where id = $1`, scheduledRunID).Scan(&scheduledSummary); err != nil {
		t.Fatal(err)
	}
	if dashboardExists || scheduledSummary != "{}" {
		t.Fatalf("dashboardExists=%v scheduledSummary=%s", dashboardExists, scheduledSummary)
	}
}
