package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

func (store *ReportStore) SetSourceConsistency(ctx context.Context, runID uuid.UUID, workerID string, consistency report.SourceConsistency, now time.Time) error {
	if consistency != report.ConsistencyStatement && consistency != report.ConsistencySerialWindow {
		return fmt.Errorf("direct source consistency is invalid")
	}
	result, err := store.pool.Exec(ctx, `
		update report_runs set execution_strategy = 'DIRECT', source_consistency = $3, updated_at = $4
		where id = $1 and claimed_by = $2 and status = 'RUNNING' and lease_expires_at >= $4`,
		runID, workerID, consistency, now)
	if err != nil {
		return fmt.Errorf("set report source consistency: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}

func (store *ReportStore) PrepareChunks(ctx context.Context, runID uuid.UUID, workerID string, manifests []report.ChunkManifest, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin chunk manifest: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		update report_runs
		set execution_strategy = 'CHUNKED', source_consistency = 'CHUNK_WINDOW',
		    progress_completed_chunks = 0, progress_total_chunks = $3, updated_at = $4
		where id = $1 and claimed_by = $2 and status = 'RUNNING' and lease_expires_at >= $4`, runID, workerID, len(manifests), now)
	if err != nil {
		return fmt.Errorf("mark chunked report: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	if _, err := tx.Exec(ctx, `delete from report_run_chunks where run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear previous chunk manifest: %w", err)
	}
	for _, manifest := range manifests {
		metadata, encodeErr := json.Marshal(map[string]any{"unitKeys": manifest.UnitKeys})
		if encodeErr != nil {
			return fmt.Errorf("encode chunk manifest: %w", encodeErr)
		}
		if _, err := tx.Exec(ctx, `
			insert into report_run_chunks (
			  run_id, chunk_no, chunk_key, cursor_from, cursor_to, unit_count, total_units,
			  metadata_json, created_at, updated_at
			) values ($1, $2, $3, $4, $5, $6, $6, $7, $8, $8)`,
			runID, manifest.Number, manifest.Key, manifest.CursorFrom, manifest.CursorTo, len(manifest.UnitKeys), metadata, now); err != nil {
			return fmt.Errorf("insert chunk manifest: %w", err)
		}
	}
	return tx.Commit(ctx)
}

func (store *ReportStore) StartChunk(ctx context.Context, runID uuid.UUID, workerID string, chunkNumber int, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_run_chunks chunk
		set status = 'RUNNING', attempt = chunk.attempt + 1, started_at = coalesce(chunk.started_at, $4), updated_at = $4
		from report_runs run
		where chunk.run_id = $1 and chunk.chunk_no = $3
		  and run.id = chunk.run_id and run.claimed_by = $2 and run.status = 'RUNNING' and run.lease_expires_at >= $4
		  and chunk.status = 'QUEUED'`, runID, workerID, chunkNumber, now)
	if err != nil {
		return fmt.Errorf("start report chunk: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}

func (store *ReportStore) CompleteChunk(ctx context.Context, runID uuid.UUID, workerID string, chunkNumber int, payload any, rowCount int, now time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode report chunk result: %w", err)
	}
	if len(encoded) > 16*1024*1024 {
		return fmt.Errorf("report chunk result exceeds 16 MiB")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete report chunk: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		update report_run_chunks chunk
		set status = 'SUCCEEDED', result_json = $5, row_count = $6,
		    finished_at = $4, updated_at = $4
		from report_runs run
		where chunk.run_id = $1 and chunk.chunk_no = $3
		  and run.id = chunk.run_id and run.claimed_by = $2 and run.status = 'RUNNING' and run.lease_expires_at >= $4
		  and chunk.status = 'RUNNING'`, runID, workerID, chunkNumber, now, encoded, rowCount)
	if err != nil {
		return fmt.Errorf("complete report chunk: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	progressResult, err := tx.Exec(ctx, `
		update report_runs set progress_completed_chunks = progress_completed_chunks + 1, updated_at = $3
		where id = $1 and claimed_by = $2 and status = 'RUNNING' and lease_expires_at >= $3`, runID, workerID, now)
	if err != nil {
		return fmt.Errorf("advance report chunk progress: %w", err)
	}
	if progressResult.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return tx.Commit(ctx)
}

func (store *ReportStore) FailChunk(ctx context.Context, runID uuid.UUID, workerID string, chunkNumber int, safeCode string, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_run_chunks chunk
		set status = 'FAILED', safe_error_code = $5, result_json = '{}'::jsonb,
		    finished_at = $4, updated_at = $4
		from report_runs run
		where chunk.run_id = $1 and chunk.chunk_no = $3
		  and run.id = chunk.run_id and run.claimed_by = $2 and run.status = 'RUNNING' and run.lease_expires_at >= $4
		  and chunk.status = 'RUNNING'`, runID, workerID, chunkNumber, now, safeCode)
	if err != nil {
		return fmt.Errorf("fail report chunk: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}
