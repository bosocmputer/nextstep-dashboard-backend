package database

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDashboardRefreshStoreRejectsChangedReplayAndReturnsExactPartialResult(t *testing.T) {
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
	now := time.Date(2026, 7, 12, 2, 0, 0, 0, time.UTC)
	tenantID, recipientID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1, $2, 'Refresh Store', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "refresh-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into line_recipients (id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, display_name_ciphertext, display_name_nonce, encryption_key_id, status, verified_at)
		values ($1, $2, $3, $4, $5, $6, 'key', 'ACTIVE', $7)`, recipientID, []byte(recipientID.String()), []byte("cipher"), []byte("nonce"), []byte("display"), []byte("display-nonce"), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into tenant_memberships (tenant_id, recipient_id, status) values ($1, $2, 'ACTIVE')`, tenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into recipient_report_permissions (tenant_id, recipient_id, report_key) values ($1, $2, 'sales_goods_services'), ($1, $2, 'stock_balance')`, tenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	requestedBy := recipientID
	inputs := []report.EnqueueInput{
		{TenantID: tenantID, ReportKey: report.SalesGoodsServices, Source: report.SourceDashboard, Period: report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-10"}, RequestedByRecipient: &requestedBy},
		{TenantID: tenantID, ReportKey: report.StockBalance, Source: report.SourceDashboard, Period: report.Period{Preset: report.Custom, DateFrom: "2026-07-10", DateTo: "2026-07-10"}, RequestedByRecipient: &requestedBy},
	}
	store := NewReportStore(pool)
	created, err := store.CreateDashboardRefresh(ctx, recipientID, tenantID, "refresh-store-001", inputs, now)
	if err != nil {
		t.Fatalf("CreateDashboardRefresh() error = %v", err)
	}
	replayed, err := store.CreateDashboardRefresh(ctx, recipientID, tenantID, "refresh-store-001", inputs, now)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("CreateDashboardRefresh() replay = %+v, %v", replayed, err)
	}
	changed := append([]report.EnqueueInput(nil), inputs...)
	changed[0].Period.DateFrom = "2026-07-02"
	if _, err := store.CreateDashboardRefresh(ctx, recipientID, tenantID, "refresh-store-001", changed, now); !errors.Is(err, report.ErrRunIdempotencyConflict) {
		t.Fatalf("changed refresh replay error = %v", err)
	}

	dashboard := report.Dashboard{
		ReportKey: report.SalesGoodsServices, Version: "1.0.0",
		Period: inputs[0].Period, ComparisonPeriod: report.Period{Preset: report.Custom, DateFrom: "2026-06-21", DateTo: "2026-06-30"},
		Timezone: "Asia/Bangkok", KPIs: []report.DashboardMetric{}, Visualizations: []report.DashboardVisualization{}, Quality: report.DashboardQuality{Status: "OK", Warnings: []string{}},
	}
	dashboardJSON, _ := json.Marshal(dashboard)
	for _, run := range created.Runs {
		if run.ReportKey == report.SalesGoodsServices {
			if _, err := pool.Exec(ctx, `update report_runs set status = 'SUCCEEDED', dashboard_json = $2, dashboard_version = '1.0.0', finished_at = $3 where id = $1`, run.RunID, dashboardJSON, now); err != nil {
				t.Fatal(err)
			}
		} else if _, err := pool.Exec(ctx, `update report_runs set status = 'FAILED', safe_error_code = 'SML_TIMEOUT', finished_at = $2 where id = $1`, run.RunID, now); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.GetDashboardRefreshResult(ctx, recipientID, tenantID, created.ID)
	if err != nil || result.Status != "PARTIAL" || len(result.Items) != 1 || len(result.Failures) != 1 || result.Failures[0].SafeErrorCode != "SML_TIMEOUT" {
		t.Fatalf("GetDashboardRefreshResult() = %+v, %v", result, err)
	}
	if _, err := store.GetDashboardRefreshResult(ctx, uuid.New(), tenantID, created.ID); !errors.Is(err, report.ErrRunNotFound) {
		t.Fatalf("cross-recipient result error = %v", err)
	}
}
