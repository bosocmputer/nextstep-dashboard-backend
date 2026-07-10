package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ReportStore struct {
	pool *pgxpool.Pool
}

func NewReportStore(pool *pgxpool.Pool) *ReportStore {
	return &ReportStore{pool: pool}
}

func (store *ReportStore) Enqueue(ctx context.Context, input report.EnqueueInput, now time.Time) (report.Run, error) {
	if _, ok := report.DefinitionFor(input.ReportKey); !ok || len(input.IdempotencyKey) < 8 || len(input.IdempotencyKey) > 200 {
		return report.Run{}, report.ErrRunIdempotencyConflict
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return report.Run{}, fmt.Errorf("begin enqueue report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lockScope := input.TenantID.String() + ":" + string(input.Source) + ":" + input.IdempotencyKey
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, lockScope); err != nil {
		return report.Run{}, fmt.Errorf("lock report idempotency key: %w", err)
	}

	existing, replayErr := findRunByIdempotency(ctx, tx, input.TenantID, input.Source, input.IdempotencyKey, now)
	if replayErr == nil && (existing.ReportKey != input.ReportKey || existing.Period.Preset != input.Period.Preset || existing.Period.DateFrom != input.Period.DateFrom || existing.Period.DateTo != input.Period.DateTo) {
		return report.Run{}, report.ErrRunIdempotencyConflict
	}
	if replayErr != nil && !errors.Is(replayErr, report.ErrRunNotFound) {
		return report.Run{}, replayErr
	}

	var allowed bool
	if err := tx.QueryRow(ctx, `select status = 'ACTIVE' and access_ends_at > $2 from tenants where id = $1`, input.TenantID, now).Scan(&allowed); errors.Is(err, pgx.ErrNoRows) || !allowed {
		return report.Run{}, report.ErrRunNotFound
	} else if err != nil {
		return report.Run{}, fmt.Errorf("validate report tenant: %w", err)
	}
	if input.Source == report.SourceDashboard {
		if input.RequestedByRecipient == nil {
			return report.Run{}, report.ErrRunForbidden
		}
		if err := tx.QueryRow(ctx, `
			select exists (
			  select 1
			  from recipient_report_permissions p
			  join tenant_memberships m on m.tenant_id = p.tenant_id and m.recipient_id = p.recipient_id and m.status = 'ACTIVE'
			  join line_recipients r on r.id = p.recipient_id and r.status = 'ACTIVE'
			  where p.tenant_id = $1 and p.recipient_id = $2 and p.report_key = $3
			)`, input.TenantID, input.RequestedByRecipient, input.ReportKey).Scan(&allowed); err != nil {
			return report.Run{}, fmt.Errorf("validate dashboard report permission: %w", err)
		}
		if !allowed {
			return report.Run{}, report.ErrRunForbidden
		}
	}

	if replayErr == nil {
		if err := tx.Commit(ctx); err != nil {
			return report.Run{}, fmt.Errorf("commit report replay: %w", err)
		}
		return existing, nil
	}

	if input.Source == report.SourceDashboard {
		var activeCount int
		if err := tx.QueryRow(ctx, `
			select count(*) from report_runs
			where tenant_id = $1 and source = 'DASHBOARD'
			  and status in ('QUEUED', 'CLAIMED', 'RUNNING')`, input.TenantID).Scan(&activeCount); err != nil {
			return report.Run{}, fmt.Errorf("count active report runs: %w", err)
		}
		if activeCount >= 2 {
			return report.Run{}, report.ErrRunConcurrencyLimit
		}
	}

	paramsJSON, _ := json.Marshal(map[string]string{"dateFrom": input.Period.DateFrom, "dateTo": input.Period.DateTo})
	row := tx.QueryRow(ctx, `
		insert into report_runs (
		  tenant_id, report_key, source, idempotency_key, status, period_preset,
		  period_from, period_to, params_json, requested_by_recipient_id,
		  queued_at, expires_at, created_at, updated_at
		) values ($1, $2, $3, $4, 'QUEUED', $5, nullif($6, '')::date, nullif($7, '')::date, $8, $9, $10, $11, $10, $10)
		returning `+reportRunColumns,
		input.TenantID, input.ReportKey, input.Source, input.IdempotencyKey, input.Period.Preset,
		input.Period.DateFrom, input.Period.DateTo, paramsJSON, input.RequestedByRecipient, now, now.Add(24*time.Hour),
	)
	created, err := scanReportRun(row, now)
	if err != nil {
		return report.Run{}, fmt.Errorf("insert report run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return report.Run{}, fmt.Errorf("commit enqueue report: %w", err)
	}
	return created, nil
}

func (store *ReportStore) Cancel(ctx context.Context, runID uuid.UUID, now time.Time) (report.Run, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return report.Run{}, fmt.Errorf("begin cancel report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	run, err := scanReportRun(tx.QueryRow(ctx, `select `+reportRunColumns+` from report_runs where id = $1 for update`, runID), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.Run{}, report.ErrRunNotFound
	}
	if err != nil {
		return report.Run{}, fmt.Errorf("lock report cancellation: %w", err)
	}
	if run.Status == report.StatusCancelled {
		if err := tx.Commit(ctx); err != nil {
			return report.Run{}, fmt.Errorf("commit report cancellation replay: %w", err)
		}
		return run, nil
	}
	if run.Status != report.StatusQueued && run.Status != report.StatusClaimed && run.Status != report.StatusRunning {
		return report.Run{}, report.ErrRunNotCancellable
	}
	cancelled, err := scanReportRun(tx.QueryRow(ctx, `
		update report_runs
		set status = 'CANCELLED', claimed_by = null, claimed_at = null, lease_expires_at = null,
		    finished_at = $2, updated_at = $2
		where id = $1
		returning `+reportRunColumns, runID, now), now)
	if err != nil {
		return report.Run{}, fmt.Errorf("cancel report run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return report.Run{}, fmt.Errorf("commit report cancellation: %w", err)
	}
	return cancelled, nil
}

func (store *ReportStore) Claim(ctx context.Context, workerID string, lease time.Duration, now time.Time) (report.Run, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return report.Run{}, fmt.Errorf("begin report claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		update report_runs
		set status = 'QUEUED', claimed_by = null, claimed_at = null,
		    lease_expires_at = null, updated_at = $1
		where status in ('CLAIMED', 'RUNNING') and lease_expires_at < $1`, now); err != nil {
		return report.Run{}, fmt.Errorf("requeue expired report leases: %w", err)
	}
	var runID uuid.UUID
	err = tx.QueryRow(ctx, `
		select r.id
		from report_runs r
		where r.status = 'QUEUED' and r.queued_at <= $1
		  and not exists (
		    select 1 from report_runs active
		    where active.tenant_id = r.tenant_id and active.id <> r.id
		      and active.status in ('CLAIMED', 'RUNNING')
		  )
		order by r.queued_at, r.id
		for update skip locked
		limit 1`, now).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.Run{}, report.ErrNoQueuedRun
	}
	if err != nil {
		return report.Run{}, fmt.Errorf("select report claim: %w", err)
	}
	row := tx.QueryRow(ctx, `
		update report_runs
		set status = 'CLAIMED', claimed_by = $2, claimed_at = $3,
		    lease_expires_at = $4, attempt = attempt + 1, updated_at = $3
		where id = $1
		returning `+reportRunColumns, runID, workerID, now, now.Add(lease))
	claimed, err := scanReportRun(row, now)
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == "report_runs_one_active_per_tenant_idx" {
		return report.Run{}, report.ErrNoQueuedRun
	}
	if err != nil {
		return report.Run{}, fmt.Errorf("claim report run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return report.Run{}, fmt.Errorf("commit report claim: %w", err)
	}
	return claimed, nil
}

func (store *ReportStore) ExtendLease(ctx context.Context, runID uuid.UUID, workerID string, lease time.Duration, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_runs
		set lease_expires_at = $4, updated_at = $3
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING') and lease_expires_at >= $3`,
		runID, workerID, now, now.Add(lease))
	if err != nil {
		return fmt.Errorf("extend report lease: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}

func (store *ReportStore) Retry(ctx context.Context, runID uuid.UUID, workerID, safeCode string, availableAt, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_runs
		set status = 'QUEUED', queued_at = $4, claimed_by = null, claimed_at = null,
		    lease_expires_at = null, safe_error_code = $3, safe_error_message = null,
		    updated_at = $5
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING')`,
		runID, workerID, safeCode, availableAt, now)
	if err != nil {
		return fmt.Errorf("retry report run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}

func (store *ReportStore) MarkRunning(ctx context.Context, runID uuid.UUID, workerID string, lease time.Duration, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_runs
		set status = 'RUNNING', started_at = coalesce(started_at, $3),
		    lease_expires_at = $4, updated_at = $3
		where id = $1 and claimed_by = $2 and status = 'CLAIMED' and lease_expires_at >= $3`, runID, workerID, now, now.Add(lease))
	if err != nil {
		return fmt.Errorf("mark report running: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}

func (store *ReportStore) Complete(ctx context.Context, runID uuid.UUID, workerID string, summary report.SummaryResult, persistRows bool, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var source report.Source
	var tenantID uuid.UUID
	if err := tx.QueryRow(ctx, `
		select source, tenant_id from report_runs
		where id = $1 and claimed_by = $2 and status = 'RUNNING' and lease_expires_at >= $3
		for update`, runID, workerID, now).Scan(&source, &tenantID); errors.Is(err, pgx.ErrNoRows) {
		return report.ErrRunLeaseLost
	} else if err != nil {
		return fmt.Errorf("lock report completion: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from report_run_rows where run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear prior report rows: %w", err)
	}
	expiresAt := now.Add(24 * time.Hour)
	if persistRows && source == report.SourceDashboard && len(summary.Rows) > 0 {
		copyRows := make([][]any, 0, len(summary.Rows))
		for index, row := range summary.Rows {
			rowJSON, err := json.Marshal(row)
			if err != nil {
				return fmt.Errorf("encode report row %d: %w", index+1, err)
			}
			copyRows = append(copyRows, []any{runID, tenantID, index + 1, rowJSON, now, expiresAt})
		}
		copied, err := tx.CopyFrom(
			ctx,
			pgx.Identifier{"report_run_rows"},
			[]string{"run_id", "tenant_id", "ordinal", "row_json", "created_at", "expires_at"},
			pgx.CopyFromRows(copyRows),
		)
		if err != nil {
			return fmt.Errorf("copy report rows: %w", err)
		}
		if copied != int64(len(copyRows)) {
			return fmt.Errorf("copy report rows inserted %d of %d rows", copied, len(copyRows))
		}
	}
	summaryJSON, _ := json.Marshal(summary.Metrics)
	reconciliationJSON, _ := json.Marshal(summary.Reconciliation)
	result, err := tx.Exec(ctx, `
		update report_runs
		set status = 'SUCCEEDED', row_count = $3, summary_json = $4,
		    reconciliation_json = $5, finished_at = $6, expires_at = $7,
		    lease_expires_at = null, updated_at = $6
		where id = $1 and claimed_by = $2 and status = 'RUNNING'`,
		runID, workerID, summary.RowCount, summaryJSON, reconciliationJSON, now, expiresAt)
	if err != nil {
		return fmt.Errorf("complete report run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit report completion: %w", err)
	}
	return nil
}

func (store *ReportStore) Fail(ctx context.Context, runID uuid.UUID, workerID, safeCode, safeMessage string, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_runs
		set status = 'FAILED', safe_error_code = $3, safe_error_message = $4,
		    finished_at = $5, lease_expires_at = null, updated_at = $5
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING')`, runID, workerID, safeCode, safeMessage, now)
	if err != nil {
		return fmt.Errorf("fail report run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}

func (store *ReportStore) Get(ctx context.Context, runID uuid.UUID, now time.Time) (report.Run, error) {
	run, err := scanReportRun(store.pool.QueryRow(ctx, `select `+reportRunColumns+` from report_runs where id = $1`, runID), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.Run{}, report.ErrRunNotFound
	}
	if err != nil {
		return report.Run{}, fmt.Errorf("get report run: %w", err)
	}
	return run, nil
}

func (store *ReportStore) ListRows(ctx context.Context, runID uuid.UUID, afterOrdinal, pageSize int, now time.Time) (report.RowsPage, error) {
	if pageSize < 1 || pageSize > 100 || afterOrdinal < 0 {
		return report.RowsPage{}, errors.New("invalid report row cursor")
	}
	var status report.RunStatus
	var expiresAt time.Time
	if err := store.pool.QueryRow(ctx, `select status, expires_at from report_runs where id = $1`, runID).Scan(&status, &expiresAt); errors.Is(err, pgx.ErrNoRows) {
		return report.RowsPage{}, report.ErrRunNotFound
	} else if err != nil {
		return report.RowsPage{}, fmt.Errorf("read report row status: %w", err)
	}
	if status == report.StatusExpired || !expiresAt.After(now) {
		return report.RowsPage{}, report.ErrRunRowsExpired
	}
	if status != report.StatusSucceeded {
		return report.RowsPage{}, report.ErrRunNotFound
	}
	rows, err := store.pool.Query(ctx, `
		select ordinal, row_json
		from report_run_rows
		where run_id = $1 and ordinal > $2
		order by ordinal
		limit $3`, runID, afterOrdinal, pageSize+1)
	if err != nil {
		return report.RowsPage{}, fmt.Errorf("list report rows: %w", err)
	}
	defer rows.Close()
	type ordinalRow struct {
		ordinal int
		row     map[string]string
	}
	items := make([]ordinalRow, 0, pageSize+1)
	for rows.Next() {
		var ordinal int
		var rowJSON []byte
		if err := rows.Scan(&ordinal, &rowJSON); err != nil {
			return report.RowsPage{}, fmt.Errorf("scan report row: %w", err)
		}
		var row map[string]string
		if err := json.Unmarshal(rowJSON, &row); err != nil {
			return report.RowsPage{}, fmt.Errorf("decode report row: %w", err)
		}
		items = append(items, ordinalRow{ordinal: ordinal, row: row})
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	page := report.RowsPage{Rows: make([]map[string]string, 0, len(items)), HasMore: hasMore}
	for _, item := range items {
		page.Rows = append(page.Rows, item.row)
		page.NextOrdinal = item.ordinal
	}
	return page, nil
}

func findRunByIdempotency(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, source report.Source, idempotencyKey string, now time.Time) (report.Run, error) {
	run, err := scanReportRun(tx.QueryRow(ctx, `
		select `+reportRunColumns+` from report_runs
		where tenant_id = $1 and source = $2 and idempotency_key = $3
		for update`, tenantID, source, idempotencyKey), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.Run{}, report.ErrRunNotFound
	}
	if err != nil {
		return report.Run{}, fmt.Errorf("find idempotent report run: %w", err)
	}
	return run, nil
}

const reportRunColumns = `
id, tenant_id, report_key, source, idempotency_key, status, period_preset,
period_from::text, period_to::text, requested_by_recipient_id,
coalesce(claimed_by, ''), lease_expires_at, attempt, row_count, is_truncated,
summary_json, reconciliation_json, coalesce(safe_error_code, ''),
coalesce(safe_error_message, ''), queued_at, started_at, finished_at,
expires_at, created_at, updated_at`

func scanReportRun(row rowScanner, now time.Time) (report.Run, error) {
	var run report.Run
	var summaryJSON, reconciliationJSON []byte
	err := row.Scan(
		&run.ID, &run.TenantID, &run.ReportKey, &run.Source, &run.IdempotencyKey,
		&run.Status, &run.Period.Preset, &run.Period.DateFrom, &run.Period.DateTo,
		&run.RequestedByRecipient, &run.ClaimedBy, &run.LeaseExpiresAt, &run.Attempt,
		&run.RowCount, &run.IsTruncated, &summaryJSON, &reconciliationJSON,
		&run.SafeErrorCode, &run.SafeErrorMessage, &run.QueuedAt, &run.StartedAt,
		&run.FinishedAt, &run.ExpiresAt, &run.CreatedAt, &run.UpdatedAt,
	)
	if err != nil {
		return report.Run{}, err
	}
	if len(summaryJSON) > 0 {
		_ = json.Unmarshal(summaryJSON, &run.Summary)
	}
	if len(reconciliationJSON) > 0 {
		_ = json.Unmarshal(reconciliationJSON, &run.Reconciliation)
	}
	if run.Status == report.StatusSucceeded && !run.ExpiresAt.After(now) {
		run.Status = report.StatusExpired
	}
	return run, nil
}
