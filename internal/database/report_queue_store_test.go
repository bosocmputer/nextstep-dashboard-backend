package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestClaimAppliesTenantFairnessHostLimitAndScheduleReservation(t *testing.T) {
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
	// This integration test intentionally leaves claimed runs active while it
	// exercises the global and host limits. Remove residue from an interrupted
	// or previously failed invocation so those runs cannot influence a rerun.
	if _, err := pool.Exec(ctx, `delete from tenants where name = 'Queue test'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		update report_runs
		set status = 'CANCELLED', claimed_by = null, claimed_at = null,
		    lease_expires_at = null, finished_at = clock_timestamp(), updated_at = clock_timestamp()
		where status in ('QUEUED', 'CLAIMED', 'RUNNING')`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `delete from tenants where name = 'Queue test'`)
	}()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	store := NewReportStore(pool)
	store.globalQueryConcurrency = 4
	store.hostQueryConcurrency = 2

	tenantIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	runIDs := make(map[uuid.UUID]uuid.UUID, len(tenantIDs))
	for index, tenantID := range tenantIDs {
		insertQueueTestTenant(t, ctx, pool, tenantID, "https://fairness-shared.example.com:8443", now)
		runID := uuid.New()
		runIDs[tenantID] = runID
		insertQueueTestRun(t, ctx, pool, runID, tenantID, report.SourceSchedule, 100, time.Date(2020, 1, 1, 0, 0, index, 0, time.UTC), now)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_query_runtime (tenant_id, last_claimed_at, updated_at)
		values ($1, $3, $4), ($2, $4, $4)`, tenantIDs[0], tenantIDs[1], now.Add(-10*time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	first, err := store.Claim(ctx, "fairness-a", time.Minute, now)
	if err != nil || first.TenantID != tenantIDs[2] {
		t.Fatalf("first fair claim = %+v, %v", first, err)
	}
	second, err := store.Claim(ctx, "fairness-b", time.Minute, now)
	if err != nil || second.TenantID != tenantIDs[0] {
		t.Fatalf("second fair claim = %+v, %v", second, err)
	}
	// The third tenant shares the same canonical origin and must remain queued
	// after two active claims, regardless of work for unrelated hosts.
	_, thirdErr := store.Claim(ctx, "fairness-c", time.Minute, now)
	if thirdErr != nil && !errors.Is(thirdErr, report.ErrNoQueuedRun) {
		t.Fatalf("third claim error = %v", thirdErr)
	}
	var thirdStatus report.RunStatus
	if err := pool.QueryRow(ctx, `select status from report_runs where id = $1`, runIDs[tenantIDs[1]]).Scan(&thirdStatus); err != nil {
		t.Fatal(err)
	}
	if thirdStatus != report.StatusQueued {
		t.Fatalf("third same-host run status = %s", thirdStatus)
	}
	if _, err := store.Cancel(ctx, runIDs[tenantIDs[1]], now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	testEvidence := failure.Complete(failure.Evidence{SafeErrorCode: "TEST_COMPLETE", OccurredAt: now.Add(time.Second)})
	if err := store.Fail(ctx, first.ID, "fairness-a", testEvidence, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Fail(ctx, second.ID, "fairness-b", testEvidence, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	manualRuns := make([]uuid.UUID, 4)
	for index := range manualRuns {
		tenantID := uuid.New()
		insertQueueTestTenant(t, ctx, pool, tenantID, fmt.Sprintf("https://manual-%d.example.com:8443", index), now)
		manualRuns[index] = uuid.New()
		insertQueueTestRun(t, ctx, pool, manualRuns[index], tenantID, report.SourceDashboard, 90, time.Date(2020, 2, 1, 0, 0, index, 0, time.UTC), now)
	}
	claimedManual := make([]report.Run, 0, 3)
	for index := 0; index < 3; index++ {
		claimed, err := store.Claim(ctx, fmt.Sprintf("manual-%d", index), time.Minute, now.Add(2*time.Second))
		if err != nil || claimed.Source != report.SourceDashboard {
			t.Fatalf("manual claim %d = %+v, %v", index, claimed, err)
		}
		claimedManual = append(claimedManual, claimed)
	}
	scheduleTenantID := uuid.New()
	insertQueueTestTenant(t, ctx, pool, scheduleTenantID, "https://reserved-schedule.example.com:8443", now)
	scheduleRunID := uuid.New()
	insertQueueTestRun(t, ctx, pool, scheduleRunID, scheduleTenantID, report.SourceSchedule, 100, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), now)
	reserved, err := store.Claim(ctx, "schedule-reserved", time.Minute, now.Add(3*time.Second))
	if err != nil || reserved.ID != scheduleRunID {
		t.Fatalf("reserved schedule claim = %+v, %v", reserved, err)
	}
	var queuedManuals int
	if err := pool.QueryRow(ctx, `select count(*) from report_runs where id = any($1::uuid[]) and status = 'QUEUED'`, manualRuns).Scan(&queuedManuals); err != nil {
		t.Fatal(err)
	}
	if queuedManuals != 1 {
		t.Fatalf("queued manual runs = %d, want one reserved behind schedule", queuedManuals)
	}
}

func insertQueueTestTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, endpoint string, now time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at)
		values ($1, $2, 'Queue test', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "queue-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_sml_connections (
		  tenant_id, endpoint_url, database_name, config_file_name,
		  username_ciphertext, username_nonce, password_ciphertext, password_nonce,
		  encryption_key_id, readiness_status, last_tested_at
		) values ($1, $2, 'DATA', 'SMLConfigDATA.xml',
		          decode('11','hex'), decode('12','hex'), decode('13','hex'), decode('14','hex'),
		          'key', 'READY', $3)`, tenantID, endpoint, now); err != nil {
		t.Fatal(err)
	}
}

func insertQueueTestRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID, tenantID uuid.UUID, source report.Source, priority int, queuedAt, now time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status,
		  period_preset, period_from, period_to, queued_at, expires_at, result_kind, priority
		) values ($1, $2, 'cash_bank_receipts', $3, $4, 'QUEUED',
		          'YESTERDAY', '2026-07-14', '2026-07-14', $5, $6, 'SUMMARY', $7)`,
		runID, tenantID, source, "queue-run-"+runID.String(), queuedAt, now.Add(24*time.Hour), priority); err != nil {
		t.Fatal(err)
	}
}
