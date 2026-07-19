package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tablequery"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTableQueryStoreQueryReportRunsWithTenantJoin(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	now := time.Date(2026, 7, 19, 9, 30, 0, 0, time.UTC)
	tenantID := uuid.New()
	runID := uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at)
		values ($1, $2, 'Report Runs Query Tenant', 'Asia/Bangkok', 'ACTIVE', $3)`,
		tenantID, "report-runs-query-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
			id, tenant_id, report_key, source, idempotency_key, status,
			period_preset, period_from, period_to, queued_at, expires_at, created_at, updated_at
		) values ($1, $2, $3, 'DASHBOARD', $4, 'QUEUED', 'YESTERDAY', '2026-07-18', '2026-07-18', $5, $6, $5, $5)`,
		runID, tenantID, report.SalesGoodsServices, "report-runs-query-"+runID.String(), now, now.Add(24*time.Hour)); err != nil {
		t.Fatalf("seed report run: %v", err)
	}

	items, total, err := NewTableQueryStore(pool).QueryReportRuns(ctx, tablequery.ReportRunsInput{
		CommonInput: tablequery.CommonInput{Page: 0, PageSize: 25},
		Filters:     tablequery.ReportRunFilters{TenantID: &tenantID},
	}, now)
	if err != nil {
		t.Fatalf("QueryReportRuns() error = %v", err)
	}
	if total != 1 || len(items) != 1 || items[0].Run.ID != runID || items[0].TenantName != "Report Runs Query Tenant" {
		t.Fatalf("QueryReportRuns() items = %+v, total = %d", items, total)
	}
}
