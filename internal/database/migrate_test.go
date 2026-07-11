package database

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestEmbeddedMigrationsAreSequential(t *testing.T) {
	migrations, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations() error = %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("expected at least one migration")
	}
	for index, migration := range migrations {
		wantVersion := index + 1
		if migration.Version != wantVersion {
			t.Fatalf("migration %q version = %d, want %d", migration.Name, migration.Version, wantVersion)
		}
		if migration.Checksum == "" {
			t.Fatalf("migration %q has empty checksum", migration.Name)
		}
	}
}

func TestMigrateCreatesFoundationAndIsIdempotent(t *testing.T) {
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
		t.Fatalf("Migrate() first run error = %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() second run error = %v", err)
	}

	var reportCount int
	if err := pool.QueryRow(ctx, `select count(*) from report_definitions`).Scan(&reportCount); err != nil {
		t.Fatalf("count report definitions: %v", err)
	}
	if reportCount != 10 {
		t.Fatalf("report definition count = %d, want 10", reportCount)
	}
	var hasDashboardJSON bool
	if err := pool.QueryRow(ctx, `
		select exists (
		  select 1 from information_schema.columns
		  where table_name = 'report_runs' and column_name = 'dashboard_json'
		)`).Scan(&hasDashboardJSON); err != nil || !hasDashboardJSON {
		t.Fatalf("report_runs.dashboard_json missing: exists=%v err=%v", hasDashboardJSON, err)
	}
	var hasConfigFileName bool
	if err := pool.QueryRow(ctx, `
		select exists (
		  select 1 from information_schema.columns
		  where table_name = 'tenant_sml_connections' and column_name = 'config_file_name'
		)`).Scan(&hasConfigFileName); err != nil || !hasConfigFileName {
		t.Fatalf("tenant_sml_connections.config_file_name missing: exists=%v err=%v", hasConfigFileName, err)
	}
	var hasPermissionsVersion bool
	if err := pool.QueryRow(ctx, `
		select exists (
		  select 1 from information_schema.columns
		  where table_name = 'tenant_memberships' and column_name = 'permissions_version'
		)`).Scan(&hasPermissionsVersion); err != nil || !hasPermissionsVersion {
		t.Fatalf("tenant_memberships.permissions_version missing: exists=%v err=%v", hasPermissionsVersion, err)
	}
	var acceptsPositionTen bool
	if err := pool.QueryRow(ctx, `
		select pg_get_constraintdef(oid) ilike '%position >= 1%position <= 10%'
		from pg_constraint
		where conrelid = 'notification_schedule_reports'::regclass
		  and conname = 'notification_schedule_reports_position_check'`).Scan(&acceptsPositionTen); err != nil || !acceptsPositionTen {
		t.Fatalf("notification schedule position constraint is not 1..10: accepts=%v err=%v", acceptsPositionTen, err)
	}

	for _, table := range []string{
		"admin_users",
		"tenants",
		"report_runs",
		"report_run_rows",
		"notification_schedules",
		"line_delivery_outbox",
		"dashboard_refreshes",
		"recipient_invitations",
		"audit_logs",
	} {
		var exists bool
		if err := pool.QueryRow(ctx, `select to_regclass($1) is not null`, table).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("table %s was not created", table)
		}
	}
}
