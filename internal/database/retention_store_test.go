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
	tenantID, dashboardRunID, scheduledRunID, recipientID, refreshID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at)
		values ($1, $2, 'Retention', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "retention-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into line_recipients (
		  id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, encryption_key_id, status
		) values ($1, decode('21','hex'), decode('22','hex'), decode('232323232323232323232323','hex'), 'key', 'ACTIVE')`, recipientID); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := now.Add(-100 * 24 * time.Hour)
	generationID := uuid.New()
	generationKey := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := pool.Exec(ctx, `
		insert into dashboard_generations (
		  id, tenant_id, generation_key, status, period_preset, period_from, period_to,
		  request_json, report_set_hash, query_plan_set_fingerprint, data_source_version,
		  projection, source_consistency, total, completed, published_at, finished_at, created_at, updated_at
		) values ($1, $2, $3, 'PUBLISHED', 'CUSTOM', '2026-01-01', '2026-01-01', '[]', $3, $3, 0,
		  'SUMMARY', 'SERIAL_WINDOW', 1, 1, $4, $4, $4, $4)`, generationID, tenantID, generationKey, oldSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
		  period_from, period_to, summary_json, reconciliation_json, dashboard_version, dashboard_json, queued_at, finished_at,
		  expires_at, created_at, updated_at
		) values
		($1, $3, 'sales_goods_services', 'DASHBOARD', 'retention-dashboard-001', 'SUCCEEDED', 'CUSTOM', '2026-01-01', '2026-01-01', '{"total":"1"}', '{"status":"OK"}', '1.0.0', '{"reportKey":"sales_goods_services"}', $4, $4, $4, $4, $4),
		($2, $3, 'stock_balance', 'SCHEDULE', 'retention-schedule-001', 'SUCCEEDED', 'AS_OF_RUN', '2026-01-01', '2026-01-01', '{"total":"2"}', '{"status":"OK"}', '1.0.0', '{"reportKey":"stock_balance"}', $4, $4, $4, $4, $4)`,
		dashboardRunID, scheduledRunID, tenantID, oldSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into dashboard_generation_reports (generation_id, tenant_id, report_key, report_run_id, position)
		values ($1, $2, 'sales_goods_services', $3, 1)`, generationID, tenantID, dashboardRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into dashboard_generation_heads (tenant_id, generation_key, published_generation_id)
		values ($1, $2, $3)`, tenantID, generationKey, generationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into dashboard_refreshes (
		  id, tenant_id, requested_by_recipient_id, idempotency_key, status, total, completed, created_at, updated_at, finished_at
		) values ($1, $2, $3, 'retention-refresh-001', 'SUCCEEDED', 1, 1, $4, $4, $4)`, refreshID, tenantID, recipientID, oldSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into dashboard_refresh_runs (refresh_id, report_key, report_run_id)
		values ($1, 'sales_goods_services', $2)`, refreshID, dashboardRunID); err != nil {
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
	if counts.ReportRows != 1 || counts.ReportRuns != 1 || counts.DashboardRefreshes != 1 || counts.DashboardGenerations != 1 || counts.ScrubbedReportRuns != 1 || counts.AuditLogs != 1 || counts.IdempotencyRequests != 1 {
		t.Fatalf("Run() counts = %+v", counts)
	}
	var dashboardExists, refreshExists bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from report_runs where id = $1)`, dashboardRunID).Scan(&dashboardExists); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select exists(select 1 from dashboard_refreshes where id = $1)`, refreshID).Scan(&refreshExists); err != nil {
		t.Fatal(err)
	}
	var publishedGenerationID *uuid.UUID
	if err := pool.QueryRow(ctx, `select published_generation_id from dashboard_generation_heads where tenant_id = $1 and generation_key = $2`, tenantID, generationKey).Scan(&publishedGenerationID); err != nil {
		t.Fatal(err)
	}
	var scheduledSummary, scheduledDashboard string
	var scheduledDashboardVersion *string
	if err := pool.QueryRow(ctx, `select summary_json::text, dashboard_json::text, dashboard_version from report_runs where id = $1`, scheduledRunID).Scan(&scheduledSummary, &scheduledDashboard, &scheduledDashboardVersion); err != nil {
		t.Fatal(err)
	}
	if dashboardExists || refreshExists || publishedGenerationID != nil || scheduledSummary != "{}" || scheduledDashboard != "{}" || scheduledDashboardVersion != nil {
		t.Fatalf("dashboardExists=%v refreshExists=%v publishedGeneration=%v scheduledSummary=%s scheduledDashboard=%s version=%v", dashboardExists, refreshExists, publishedGenerationID, scheduledSummary, scheduledDashboard, scheduledDashboardVersion)
	}
}
