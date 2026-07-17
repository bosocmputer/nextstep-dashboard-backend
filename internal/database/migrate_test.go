package database

import (
	"context"
	"os"
	"strings"
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
	if len(migrations) < 27 || !migrations[12].NoTransaction || !migrations[13].NoTransaction || !migrations[16].NoTransaction || !migrations[18].NoTransaction || !migrations[20].NoTransaction || !migrations[22].NoTransaction || !migrations[24].NoTransaction || !migrations[26].NoTransaction {
		t.Fatalf("snapshot indexes must use non-transactional concurrent migrations: %+v", migrations)
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
	var hasArchivedAt, acceptsArchived bool
	if err := pool.QueryRow(ctx, `
		select exists (
		  select 1 from information_schema.columns
		  where table_name = 'notification_schedules' and column_name = 'archived_at'
		)`).Scan(&hasArchivedAt); err != nil || !hasArchivedAt {
		t.Fatalf("notification_schedules.archived_at missing: exists=%v err=%v", hasArchivedAt, err)
	}
	if err := pool.QueryRow(ctx, `
		select pg_get_constraintdef(oid) like '%ARCHIVED%'
		from pg_constraint
		where conrelid = 'notification_schedules'::regclass
		  and conname = 'notification_schedules_status_check'`).Scan(&acceptsArchived); err != nil || !acceptsArchived {
		t.Fatalf("notification schedule status does not accept ARCHIVED: accepts=%v err=%v", acceptsArchived, err)
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
	var hasProgressPhase, hasRefreshPolicy, acceptsBackground bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'report_runs' and column_name = 'progress_phase')`).Scan(&hasProgressPhase); err != nil || !hasProgressPhase {
		t.Fatalf("report_runs.progress_phase missing: exists=%v err=%v", hasProgressPhase, err)
	}
	if err := pool.QueryRow(ctx, `select to_regclass('tenant_dashboard_refresh_policies') is not null`).Scan(&hasRefreshPolicy); err != nil || !hasRefreshPolicy {
		t.Fatalf("refresh policy table missing: exists=%v err=%v", hasRefreshPolicy, err)
	}
	if err := pool.QueryRow(ctx, `select pg_get_constraintdef(oid) like '%BACKGROUND%' from pg_constraint where conrelid = 'report_runs'::regclass and conname = 'report_runs_source_check'`).Scan(&acceptsBackground); err != nil || !acceptsBackground {
		t.Fatalf("report run source does not accept BACKGROUND: accepts=%v err=%v", acceptsBackground, err)
	}
	var hasTenantArchivedAt bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'tenants' and column_name = 'archived_at')`).Scan(&hasTenantArchivedAt); err != nil || !hasTenantArchivedAt {
		t.Fatalf("tenants.archived_at missing: exists=%v err=%v", hasTenantArchivedAt, err)
	}
	var hasMaterializationVersion, hasMaterializedPosition bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'notification_runs' and column_name = 'materialization_version')`).Scan(&hasMaterializationVersion); err != nil || !hasMaterializationVersion {
		t.Fatalf("notification_runs.materialization_version missing: exists=%v err=%v", hasMaterializationVersion, err)
	}
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'notification_run_reports' and column_name = 'position')`).Scan(&hasMaterializedPosition); err != nil || !hasMaterializedPosition {
		t.Fatalf("notification_run_reports.position missing: exists=%v err=%v", hasMaterializedPosition, err)
	}
	var hasTriggerKind, hasIncidents, hasOutbox, hasCursor, hasMaintenance bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'notification_runs' and column_name = 'trigger_kind')`).Scan(&hasTriggerKind); err != nil || !hasTriggerKind {
		t.Fatalf("notification_runs.trigger_kind missing: exists=%v err=%v", hasTriggerKind, err)
	}
	for table, destination := range map[string]*bool{
		"operational_incidents":           &hasIncidents,
		"operational_alert_outbox":        &hasOutbox,
		"operational_monitor_cursors":     &hasCursor,
		"operational_maintenance_windows": &hasMaintenance,
	} {
		if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.tables where table_schema = 'public' and table_name = $1)`, table).Scan(destination); err != nil || !*destination {
			t.Fatalf("%s missing: exists=%v err=%v", table, *destination, err)
		}
	}
	var hasQueryFingerprint, hasGenerationTables, hasHostCircuit, hasFairnessState, hasLeaseIndex bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'report_runs' and column_name = 'query_plan_fingerprint')`).Scan(&hasQueryFingerprint); err != nil || !hasQueryFingerprint {
		t.Fatalf("report_runs.query_plan_fingerprint missing: exists=%v err=%v", hasQueryFingerprint, err)
	}
	if err := pool.QueryRow(ctx, `select to_regclass('dashboard_generations') is not null and to_regclass('dashboard_generation_heads') is not null and to_regclass('report_run_chunks') is not null`).Scan(&hasGenerationTables); err != nil || !hasGenerationTables {
		t.Fatalf("generation or chunk tables missing: exists=%v err=%v", hasGenerationTables, err)
	}
	if err := pool.QueryRow(ctx, `select to_regclass('sml_host_circuits') is not null`).Scan(&hasHostCircuit); err != nil || !hasHostCircuit {
		t.Fatalf("sml_host_circuits missing: exists=%v err=%v", hasHostCircuit, err)
	}
	if err := pool.QueryRow(ctx, `select to_regclass('tenant_query_runtime') is not null`).Scan(&hasFairnessState); err != nil || !hasFairnessState {
		t.Fatalf("tenant_query_runtime missing: exists=%v err=%v", hasFairnessState, err)
	}
	if err := pool.QueryRow(ctx, `select to_regclass('report_runs_active_lease_expiry_idx') is not null`).Scan(&hasLeaseIndex); err != nil || !hasLeaseIndex {
		t.Fatalf("active lease recovery index missing: exists=%v err=%v", hasLeaseIndex, err)
	}
	var acceptsPositionTen bool
	if err := pool.QueryRow(ctx, `
		select pg_get_constraintdef(oid) ~ 'position.*>= 1.*position.*<= 10'
		from pg_constraint
		where conrelid = 'notification_schedule_reports'::regclass
		  and conname = 'notification_schedule_reports_position_check'`).Scan(&acceptsPositionTen); err != nil || !acceptsPositionTen {
		t.Fatalf("notification schedule position constraint is not 1..10: accepts=%v err=%v", acceptsPositionTen, err)
	}
	var hasReportEvidence, hasIncidentEvidence bool
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'report_runs' and column_name = 'failure_stage')`).Scan(&hasReportEvidence); err != nil || !hasReportEvidence {
		t.Fatalf("report_runs failure evidence missing: exists=%v err=%v", hasReportEvidence, err)
	}
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'operational_incident_events' and column_name = 'failure_stage')`).Scan(&hasIncidentEvidence); err != nil || !hasIncidentEvidence {
		t.Fatalf("operational incident failure evidence missing: exists=%v err=%v", hasIncidentEvidence, err)
	}
	var hasIncidentSubjects, hasActiveAffected bool
	if err := pool.QueryRow(ctx, `select to_regclass('operational_incident_subjects') is not null`).Scan(&hasIncidentSubjects); err != nil || !hasIncidentSubjects {
		t.Fatalf("operational incident subjects missing: exists=%v err=%v", hasIncidentSubjects, err)
	}
	if err := pool.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_name = 'operational_incidents' and column_name = 'active_affected_count')`).Scan(&hasActiveAffected); err != nil || !hasActiveAffected {
		t.Fatalf("operational incident active affected count missing: exists=%v err=%v", hasActiveAffected, err)
	}

	for _, table := range []string{
		"admin_users",
		"tenants",
		"report_runs",
		"report_run_rows",
		"notification_schedules",
		"line_delivery_outbox",
		"dashboard_refreshes",
		"dashboard_generations",
		"dashboard_generation_reports",
		"dashboard_generation_heads",
		"report_run_chunks",
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

func TestNonTransactionalMigrationStatementsExecuteSeparately(t *testing.T) {
	statements := nonTransactionalStatements("-- nextstep:no-transaction\ncreate index concurrently one on a(id);\ncreate index concurrently two on b(id);\n")
	if len(statements) != 2 || strings.Contains(statements[0], ";") || strings.Contains(statements[1], ";") {
		t.Fatalf("statements = %#v", statements)
	}
}
