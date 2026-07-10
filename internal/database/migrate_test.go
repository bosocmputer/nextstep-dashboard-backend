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
	var hasConfigFileName bool
	if err := pool.QueryRow(ctx, `
		select exists (
		  select 1 from information_schema.columns
		  where table_name = 'tenant_sml_connections' and column_name = 'config_file_name'
		)`).Scan(&hasConfigFileName); err != nil || !hasConfigFileName {
		t.Fatalf("tenant_sml_connections.config_file_name missing: exists=%v err=%v", hasConfigFileName, err)
	}

	for _, table := range []string{
		"admin_users",
		"tenants",
		"report_runs",
		"report_run_rows",
		"notification_schedules",
		"line_delivery_outbox",
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
