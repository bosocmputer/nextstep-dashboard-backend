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

func TestChunkStorePersistsManifestAndMonotonicProgressBehindLease(t *testing.T) {
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
	tenantID, runID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1, $2, 'Chunk', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "chunk-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, result_kind, idempotency_key, status,
		  period_preset, period_from, period_to, claimed_by, lease_expires_at,
		  queued_at, started_at, expires_at, created_at, updated_at
		) values ($1, $2, 'stock_balance', 'BACKGROUND', 'SUMMARY', 'chunk-store-001', 'RUNNING',
		  'AS_OF_RUN', '2026-07-14', '2026-07-14', 'worker-a', $3, $4, $4, $5, $4, $4)`,
		runID, tenantID, now.Add(time.Minute), now, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	store := NewReportStore(pool)
	manifest := []report.ChunkManifest{
		{Number: 1, Key: "000001:aaaaaaaaaaaaaaaa", CursorFrom: "001", CursorTo: "500", UnitKeys: []string{"001", "500"}},
		{Number: 2, Key: "000002:bbbbbbbbbbbbbbbb", CursorFrom: "501", CursorTo: "999", UnitKeys: []string{"501", "999"}},
	}
	if err := store.PrepareChunks(ctx, runID, "worker-a", manifest, now); err != nil {
		t.Fatal(err)
	}
	if err := store.StartChunk(ctx, runID, "worker-a", 1, now); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteChunk(ctx, runID, "worker-a", 1, map[string]any{"safe": true}, 2, now); err != nil {
		t.Fatal(err)
	}
	if err := store.StartChunk(ctx, runID, "worker-a", 2, now); err != nil {
		t.Fatal(err)
	}
	if err := store.FailChunk(ctx, runID, "worker-a", 2, "SML_TIMEOUT", now); err != nil {
		t.Fatal(err)
	}
	var strategy report.ExecutionStrategy
	var consistency report.SourceConsistency
	var completed, total int
	if err := pool.QueryRow(ctx, `select execution_strategy, source_consistency, progress_completed_chunks, progress_total_chunks from report_runs where id = $1`, runID).Scan(&strategy, &consistency, &completed, &total); err != nil {
		t.Fatal(err)
	}
	if strategy != report.ExecutionChunked || consistency != report.ConsistencyChunkWindow || completed != 1 || total != 2 {
		t.Fatalf("strategy=%s consistency=%s progress=%d/%d", strategy, consistency, completed, total)
	}
}
