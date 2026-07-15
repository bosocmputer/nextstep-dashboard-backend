package database

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublishedGenerationLookupIsAtomicAndInvalidatedBySMLVersion(t *testing.T) {
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
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	tenantID, recipientID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1, $2, 'Generation', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "generation-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_sml_connections (
		  tenant_id, endpoint_url, database_name, username_ciphertext, username_nonce,
		  password_ciphertext, password_nonce, encryption_key_id, version, readiness_status
		) values ($1, 'https://sml.example.test', 'DATA', '\x01', '\x02', '\x03', '\x04', 'test', 1, 'READY')`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into line_recipients (id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, encryption_key_id, status, verified_at)
		values ($1, $2, '\x01', '\x02', 'test', 'ACTIVE', $3)`, recipientID, []byte(recipientID.String()), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into tenant_memberships (tenant_id, recipient_id, status) values ($1, $2, 'ACTIVE')`, tenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into recipient_report_permissions (tenant_id, recipient_id, report_key) values ($1, $2, 'sales_goods_services')`, tenantID, recipientID); err != nil {
		t.Fatal(err)
	}

	period := report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-14"}
	requestedBy := recipientID
	store := NewReportStore(pool).ConfigureGenerationCache(true)
	refresh, err := store.CreateDashboardRefresh(ctx, recipientID, tenantID, "published-generation-001", []report.EnqueueInput{{
		TenantID: tenantID, ReportKey: report.SalesGoodsServices, Source: report.SourceDashboard,
		Period: period, RequestedByRecipient: &requestedBy,
	}}, now)
	if err != nil {
		t.Fatal(err)
	}
	dashboard := report.Dashboard{
		ReportKey: report.SalesGoodsServices, Version: "1.0.0", Period: period,
		Timezone: "Asia/Bangkok", KPIs: []report.DashboardMetric{}, Visualizations: []report.DashboardVisualization{},
		Quality: report.DashboardQuality{Status: "OK", Warnings: []string{}},
	}
	dashboardJSON, _ := json.Marshal(dashboard)
	runID := refresh.Runs[0].RunID
	if _, err := pool.Exec(ctx, `
		update report_runs set status = 'SUCCEEDED', dashboard_json = $2, dashboard_version = '1.0.0',
		  started_at = $3, source_started_at = $3, finished_at = $4, source_finished_at = $4
		where id = $1`, runID, dashboardJSON, now.Add(-time.Minute), now); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateDashboardGenerationsForRun(ctx, tx, runID, now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	requests := []viewer.SnapshotPeriodRequest{{ReportKey: report.SalesGoodsServices, Period: period}}
	overview, err := store.GetPublishedOverviewForPeriods(ctx, tenantID, requests, now.Add(time.Minute))
	if err != nil || overview.GenerationID == nil || len(overview.Items) != 1 || overview.Items[0].RunID != runID {
		t.Fatalf("exact overview=%+v err=%v", overview, err)
	}
	latest, err := store.GetLatestPublishedOverview(ctx, tenantID, []report.Key{report.SalesGoodsServices}, now.Add(time.Minute))
	if err != nil || latest.GenerationID == nil || *latest.GenerationID != *overview.GenerationID {
		t.Fatalf("latest overview=%+v err=%v", latest, err)
	}
	if _, err := pool.Exec(ctx, `update tenant_sml_connections set version = 2 where tenant_id = $1`, tenantID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetLatestPublishedOverview(ctx, tenantID, []report.Key{report.SalesGoodsServices}, now.Add(time.Minute)); !errors.Is(err, report.ErrRunNotFound) {
		t.Fatalf("stale data-source generation error=%v", err)
	}

	otherTenantID, otherGenerationID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1, $2, 'Other', 'Asia/Bangkok', 'ACTIVE', $3)`, otherTenantID, "generation-"+otherTenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `
		insert into dashboard_generations (
		  id, tenant_id, generation_key, status, period_preset, period_from, period_to,
		  report_set_hash, query_plan_set_fingerprint, data_source_version,
		  total, completed, published_at, finished_at
		) values ($1, $2, $3, 'PUBLISHED', 'CUSTOM', '2026-07-01', '2026-07-14',
		  $3, $3, 1, 1, 1, $4, $4)`, otherGenerationID, otherTenantID, fingerprint, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update dashboard_generation_heads set published_generation_id = $2 where tenant_id = $1`, tenantID, otherGenerationID); err == nil {
		t.Fatal("cross-tenant generation head was accepted")
	}
	if _, err := pool.Exec(ctx, `update dashboard_refreshes set generation_id = $2 where id = $1`, refresh.ID, otherGenerationID); err == nil {
		t.Fatal("cross-tenant refresh generation was accepted")
	}
}
