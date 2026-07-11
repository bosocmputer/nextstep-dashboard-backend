package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
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
	var reportKey report.Key
	if err := tx.QueryRow(ctx, `
		select source, tenant_id, report_key from report_runs
		where id = $1 and claimed_by = $2 and status = 'RUNNING' and lease_expires_at >= $3
		for update`, runID, workerID, now).Scan(&source, &tenantID, &reportKey); errors.Is(err, pgx.ErrNoRows) {
		return report.ErrRunLeaseLost
	} else if err != nil {
		return fmt.Errorf("lock report completion: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from report_run_rows where run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear prior report rows: %w", err)
	}
	expiresAt := now.Add(24 * time.Hour)
	if persistRows && source == report.SourceDashboard && len(summary.Rows) > 0 {
		copied, err := tx.CopyFrom(
			ctx,
			pgx.Identifier{"report_run_rows"},
			[]string{"run_id", "tenant_id", "ordinal", "row_json", "created_at", "expires_at"},
			pgx.CopyFromSlice(len(summary.Rows), func(index int) ([]any, error) {
				rowJSON, encodeErr := json.Marshal(summary.Rows[index])
				if encodeErr != nil {
					return nil, fmt.Errorf("encode report row %d: %w", index+1, encodeErr)
				}
				return []any{runID, tenantID, index + 1, rowJSON, now, expiresAt}, nil
			}),
		)
		if err != nil {
			return fmt.Errorf("copy report rows: %w", err)
		}
		if copied != int64(len(summary.Rows)) {
			return fmt.Errorf("copy report rows inserted %d of %d rows", copied, len(summary.Rows))
		}
	}
	summaryJSON, _ := json.Marshal(summary.Metrics)
	reconciliationJSON, _ := json.Marshal(summary.Reconciliation)
	dashboardJSON := []byte(`{}`)
	var dashboardVersion *string
	if summary.Dashboard != nil {
		if summary.Dashboard.ReportKey != reportKey || summary.Dashboard.Version == "" {
			return fmt.Errorf("dashboard identity does not match report run")
		}
		summary.Dashboard.GeneratedAt = now
		encoded, encodeErr := json.Marshal(summary.Dashboard)
		if encodeErr != nil {
			return fmt.Errorf("encode report dashboard: %w", encodeErr)
		}
		if len(encoded) > 128*1024 {
			return fmt.Errorf("report dashboard exceeds 128 KiB")
		}
		dashboardJSON = encoded
		dashboardVersion = &summary.Dashboard.Version
	}
	result, err := tx.Exec(ctx, `
		update report_runs
		set status = 'SUCCEEDED', row_count = $3, summary_json = $4,
		    reconciliation_json = $5, dashboard_version = $6, dashboard_json = $7,
		    finished_at = $8, expires_at = $9, lease_expires_at = null, updated_at = $8
		where id = $1 and claimed_by = $2 and status = 'RUNNING'`,
		runID, workerID, summary.RowCount, summaryJSON, reconciliationJSON, dashboardVersion, dashboardJSON, now, expiresAt)
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

func (store *ReportStore) GetDashboard(ctx context.Context, tenantID, runID uuid.UUID) (report.Dashboard, error) {
	var dashboardJSON []byte
	err := store.pool.QueryRow(ctx, `
		select dashboard_json
		from report_runs
		where id = $1 and tenant_id = $2 and status = 'SUCCEEDED'
		  and dashboard_json <> '{}'::jsonb`, runID, tenantID).Scan(&dashboardJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.Dashboard{}, report.ErrRunNotFound
	}
	if err != nil {
		return report.Dashboard{}, fmt.Errorf("get report dashboard: %w", err)
	}
	var dashboard report.Dashboard
	if err := json.Unmarshal(dashboardJSON, &dashboard); err != nil {
		return report.Dashboard{}, fmt.Errorf("decode report dashboard: %w", err)
	}
	return dashboard, nil
}

func (store *ReportStore) ListLatestDashboards(ctx context.Context, tenantID uuid.UUID, reportKeys []report.Key) ([]viewer.DashboardSnapshot, error) {
	if len(reportKeys) == 0 || len(reportKeys) > len(report.Keys()) {
		return []viewer.DashboardSnapshot{}, nil
	}
	keys := make([]string, len(reportKeys))
	for index, key := range reportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return nil, report.ErrRunForbidden
		}
		keys[index] = string(key)
	}
	rows, err := store.pool.Query(ctx, `
		select distinct on (report_key) id, report_key, dashboard_json
		from report_runs
		where tenant_id = $1 and report_key = any($2::text[])
		  and status = 'SUCCEEDED' and dashboard_json <> '{}'::jsonb
		order by report_key, finished_at desc nulls last, id desc`, tenantID, keys)
	if err != nil {
		return nil, fmt.Errorf("list latest report dashboards: %w", err)
	}
	defer rows.Close()
	items := make([]viewer.DashboardSnapshot, 0, len(reportKeys))
	for rows.Next() {
		var item viewer.DashboardSnapshot
		var reportKey report.Key
		var dashboardJSON []byte
		if err := rows.Scan(&item.RunID, &reportKey, &dashboardJSON); err != nil {
			return nil, fmt.Errorf("scan latest report dashboard: %w", err)
		}
		if err := json.Unmarshal(dashboardJSON, &item.Dashboard); err != nil {
			return nil, fmt.Errorf("decode latest report dashboard: %w", err)
		}
		if item.Dashboard.ReportKey != reportKey {
			return nil, fmt.Errorf("stored dashboard report key does not match run")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate latest report dashboards: %w", err)
	}
	return items, nil
}

func (store *ReportStore) CreateDashboardRefresh(ctx context.Context, recipientID, tenantID uuid.UUID, idempotencyKey string, inputs []report.EnqueueInput, now time.Time) (viewer.DashboardRefresh, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 || len(inputs) < 1 || len(inputs) > len(report.Keys()) {
		return viewer.DashboardRefresh{}, viewer.ErrReportInputInvalid
	}
	keys := make([]string, 0, len(inputs))
	seen := make(map[report.Key]struct{}, len(inputs))
	for _, input := range inputs {
		if input.TenantID != tenantID || input.Source != report.SourceDashboard || input.RequestedByRecipient == nil || *input.RequestedByRecipient != recipientID {
			return viewer.DashboardRefresh{}, report.ErrRunForbidden
		}
		if _, ok := report.DefinitionFor(input.ReportKey); !ok || input.Period.DateFrom == "" || input.Period.DateTo == "" {
			return viewer.DashboardRefresh{}, viewer.ErrReportInputInvalid
		}
		if _, duplicate := seen[input.ReportKey]; duplicate {
			return viewer.DashboardRefresh{}, viewer.ErrReportInputInvalid
		}
		seen[input.ReportKey] = struct{}{}
		keys = append(keys, string(input.ReportKey))
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("begin dashboard refresh: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, "dashboard-refresh:"+tenantID.String()); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("lock dashboard refresh: %w", err)
	}

	var existingID uuid.UUID
	err = tx.QueryRow(ctx, `
		select id from dashboard_refreshes
		where tenant_id = $1 and requested_by_recipient_id = $2 and idempotency_key = $3`, tenantID, recipientID, idempotencyKey).Scan(&existingID)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("commit dashboard refresh replay: %w", err)
		}
		return store.GetDashboardRefresh(ctx, recipientID, tenantID, existingID, now)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return viewer.DashboardRefresh{}, fmt.Errorf("find dashboard refresh replay: %w", err)
	}

	var tenantAllowed bool
	if err := tx.QueryRow(ctx, `
		select exists (
		  select 1 from tenants t
		  join tenant_memberships m on m.tenant_id = t.id and m.recipient_id = $2 and m.status = 'ACTIVE'
		  join line_recipients r on r.id = m.recipient_id and r.status = 'ACTIVE'
		  where t.id = $1 and t.status = 'ACTIVE' and t.access_ends_at > $3
		)`, tenantID, recipientID, now).Scan(&tenantAllowed); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("validate dashboard refresh tenant: %w", err)
	}
	if !tenantAllowed {
		return viewer.DashboardRefresh{}, report.ErrRunForbidden
	}
	var permissionCount int
	if err := tx.QueryRow(ctx, `
		select count(*) from recipient_report_permissions
		where tenant_id = $1 and recipient_id = $2 and report_key = any($3::text[])`, tenantID, recipientID, keys).Scan(&permissionCount); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("validate dashboard refresh reports: %w", err)
	}
	if permissionCount != len(keys) {
		return viewer.DashboardRefresh{}, report.ErrRunForbidden
	}
	var active bool
	if err := tx.QueryRow(ctx, `
		select exists (
		  select 1 from dashboard_refreshes refresh
		  join dashboard_refresh_runs linked on linked.refresh_id = refresh.id
		  join report_runs run on run.id = linked.report_run_id
		  where refresh.tenant_id = $1 and run.status in ('QUEUED', 'CLAIMED', 'RUNNING')
		)`, tenantID).Scan(&active); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("check active dashboard refresh: %w", err)
	}
	if active {
		return viewer.DashboardRefresh{}, report.ErrRunConcurrencyLimit
	}

	refreshID := uuid.New()
	if _, err := tx.Exec(ctx, `
		insert into dashboard_refreshes (id, tenant_id, requested_by_recipient_id, idempotency_key, total, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $6)`, refreshID, tenantID, recipientID, idempotencyKey, len(inputs), now); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("insert dashboard refresh: %w", err)
	}
	for _, input := range inputs {
		runID := uuid.New()
		paramsJSON, _ := json.Marshal(map[string]string{"dateFrom": input.Period.DateFrom, "dateTo": input.Period.DateTo})
		runIdempotency := "overview:" + refreshID.String() + ":" + string(input.ReportKey)
		if _, err := tx.Exec(ctx, `
			insert into report_runs (
			  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
			  period_from, period_to, params_json, requested_by_recipient_id,
			  queued_at, expires_at, created_at, updated_at
			) values ($1, $2, $3, 'DASHBOARD', $4, 'QUEUED', $5, $6::date, $7::date, $8, $9, $10, $11, $10, $10)`,
			runID, tenantID, input.ReportKey, runIdempotency, input.Period.Preset, input.Period.DateFrom, input.Period.DateTo,
			paramsJSON, recipientID, now, now.Add(24*time.Hour)); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("insert dashboard refresh report run: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into dashboard_refresh_runs (refresh_id, report_key, report_run_id)
			values ($1, $2, $3)`, refreshID, input.ReportKey, runID); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("link dashboard refresh report run: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("commit dashboard refresh: %w", err)
	}
	return store.GetDashboardRefresh(ctx, recipientID, tenantID, refreshID, now)
}

func (store *ReportStore) GetDashboardRefresh(ctx context.Context, recipientID, tenantID, refreshID uuid.UUID, now time.Time) (viewer.DashboardRefresh, error) {
	var refresh viewer.DashboardRefresh
	err := store.pool.QueryRow(ctx, `
		select id, tenant_id, total, created_at, finished_at
		from dashboard_refreshes
		where id = $1 and tenant_id = $2 and requested_by_recipient_id = $3`, refreshID, tenantID, recipientID).Scan(
		&refresh.ID, &refresh.TenantID, &refresh.Total, &refresh.CreatedAt, &refresh.FinishedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return viewer.DashboardRefresh{}, report.ErrRunNotFound
	}
	if err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("get dashboard refresh: %w", err)
	}
	rows, err := store.pool.Query(ctx, `
		select linked.report_key, linked.report_run_id, run.status
		from dashboard_refresh_runs linked
		join report_runs run on run.id = linked.report_run_id
		where linked.refresh_id = $1
		order by linked.report_key`, refreshID)
	if err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("list dashboard refresh runs: %w", err)
	}
	defer rows.Close()
	queued, running := 0, 0
	refresh.Runs = make([]viewer.DashboardRefreshRun, 0, refresh.Total)
	for rows.Next() {
		var item viewer.DashboardRefreshRun
		if err := rows.Scan(&item.ReportKey, &item.RunID, &item.Status); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("scan dashboard refresh run: %w", err)
		}
		switch item.Status {
		case report.StatusQueued:
			queued++
		case report.StatusClaimed, report.StatusRunning:
			running++
		case report.StatusSucceeded:
			refresh.Completed++
		case report.StatusFailed, report.StatusCancelled, report.StatusExpired:
			refresh.Failed++
		}
		refresh.Runs = append(refresh.Runs, item)
	}
	if err := rows.Err(); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("iterate dashboard refresh runs: %w", err)
	}
	switch {
	case running > 0:
		refresh.Status = viewer.DashboardRefreshRunning
	case queued > 0:
		refresh.Status = viewer.DashboardRefreshQueued
	case refresh.Completed == refresh.Total:
		refresh.Status = viewer.DashboardRefreshSucceeded
	case refresh.Completed > 0 && refresh.Failed > 0:
		refresh.Status = viewer.DashboardRefreshPartial
	default:
		refresh.Status = viewer.DashboardRefreshFailed
	}
	terminal := refresh.Status == viewer.DashboardRefreshSucceeded || refresh.Status == viewer.DashboardRefreshPartial || refresh.Status == viewer.DashboardRefreshFailed
	if terminal && refresh.FinishedAt == nil {
		finished := now
		refresh.FinishedAt = &finished
	}
	if _, err := store.pool.Exec(ctx, `
		update dashboard_refreshes
		set status = $2, completed = $3, failed = $4,
		    finished_at = case when $5 then coalesce(finished_at, $6) else null end,
		    updated_at = $6
		where id = $1`, refresh.ID, refresh.Status, refresh.Completed, refresh.Failed, terminal, now); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("update dashboard refresh status: %w", err)
	}
	return refresh, nil
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
	return scanReportRunWithExtras(row, now)
}

func scanReportRunWithExtras(row rowScanner, now time.Time, extraDestinations ...any) (report.Run, error) {
	var run report.Run
	var summaryJSON, reconciliationJSON []byte
	destinations := []any{
		&run.ID, &run.TenantID, &run.ReportKey, &run.Source, &run.IdempotencyKey,
		&run.Status, &run.Period.Preset, &run.Period.DateFrom, &run.Period.DateTo,
		&run.RequestedByRecipient, &run.ClaimedBy, &run.LeaseExpiresAt, &run.Attempt,
		&run.RowCount, &run.IsTruncated, &summaryJSON, &reconciliationJSON,
		&run.SafeErrorCode, &run.SafeErrorMessage, &run.QueuedAt, &run.StartedAt,
		&run.FinishedAt, &run.ExpiresAt, &run.CreatedAt, &run.UpdatedAt,
	}
	destinations = append(destinations, extraDestinations...)
	err := row.Scan(destinations...)
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
