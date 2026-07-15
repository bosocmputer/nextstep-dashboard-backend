package database

import (
	"context"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/retention"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RetentionStore struct {
	pool *pgxpool.Pool
}

func NewRetentionStore(pool *pgxpool.Pool) *RetentionStore {
	return &RetentionStore{pool: pool}
}

func (store *RetentionStore) Run(ctx context.Context, policy retention.Policy, now time.Time) (retention.Counts, error) {
	if policy.SnapshotRetention < 24*time.Hour || policy.HistoryRetention < policy.SnapshotRetention || policy.ExpiredSessionGrace < 0 || policy.BatchSize < 1 || policy.BatchSize > 50_000 {
		return retention.Counts{}, fmt.Errorf("retention policy is invalid")
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return retention.Counts{}, fmt.Errorf("begin retention: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `set local lock_timeout = '5s'`); err != nil {
		return retention.Counts{}, fmt.Errorf("bound retention transaction: %w", err)
	}
	if _, err := tx.Exec(ctx, `set local statement_timeout = '2min'`); err != nil {
		return retention.Counts{}, fmt.Errorf("bound retention transaction: %w", err)
	}
	var counts retention.Counts
	if counts.ReportRows, err = execRetention(ctx, tx, `
		delete from report_run_rows where ctid in (
		  select ctid from report_run_rows where expires_at <= $1 order by expires_at limit $2
		)`, now, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	snapshotCutoff := now.Add(-policy.SnapshotRetention)
	if counts.DashboardRefreshes, err = execRetention(ctx, tx, `
		delete from dashboard_refreshes where id in (
		  select id from dashboard_refreshes where created_at <= $1 order by created_at limit $2
		)`, snapshotCutoff, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	// Clear composite references before deleting expired generations. Keeping
	// tenant_id in each foreign key makes cross-tenant pointers impossible; it
	// also means PostgreSQL cannot use a broad ON DELETE SET NULL on both columns.
	for _, statement := range []string{
		`update dashboard_generation_heads set published_generation_id = null, updated_at = $3
		 where published_generation_id in (
		   select id from dashboard_generations
		   where coalesce(published_at, finished_at, created_at) <= $1
		   order by coalesce(published_at, finished_at, created_at) limit $2
		 )`,
		`update dashboard_refreshes set generation_id = null, updated_at = $3
		 where generation_id in (
		   select id from dashboard_generations
		   where coalesce(published_at, finished_at, created_at) <= $1
		   order by coalesce(published_at, finished_at, created_at) limit $2
		 )`,
	} {
		if _, err := tx.Exec(ctx, statement, snapshotCutoff, policy.BatchSize, now); err != nil {
			return retention.Counts{}, fmt.Errorf("clear expired dashboard generation reference: %w", err)
		}
	}
	if counts.DashboardGenerations, err = execRetention(ctx, tx, `
		delete from dashboard_generations where id in (
		  select id from dashboard_generations
		  where coalesce(published_at, finished_at, created_at) <= $1
		  order by coalesce(published_at, finished_at, created_at) limit $2
		)`, snapshotCutoff, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if counts.ScrubbedReportRuns, err = execRetention(ctx, tx, `
		update report_runs
		set summary_json = '{}'::jsonb, reconciliation_json = '{}'::jsonb,
		    dashboard_json = '{}'::jsonb, dashboard_version = null,
		    safe_error_message = null, updated_at = $1
		where id in (
		  select id from report_runs
		  where source = 'SCHEDULE' and created_at <= $2
		    and (summary_json <> '{}'::jsonb or reconciliation_json <> '{}'::jsonb
		      or dashboard_json <> '{}'::jsonb or dashboard_version is not null or safe_error_message is not null)
		  order by created_at limit $3
		)`, now, snapshotCutoff, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if counts.ScrubbedOutboxPayloads, err = execRetention(ctx, tx, `
		update line_delivery_outbox set payload_json = '{}'::jsonb
		where id in (
		  select id from line_delivery_outbox
		  where completed_at is not null and created_at <= $1 and payload_json <> '{}'::jsonb
		  order by created_at limit $2
		)`, snapshotCutoff, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if counts.ReportRuns, err = execRetention(ctx, tx, `
		delete from report_runs where id in (
		  select id from report_runs
		  where source in ('DASHBOARD', 'BACKGROUND') and created_at <= $1
		  order by created_at limit $2
		)`, snapshotCutoff, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	historyCutoff := now.Add(-policy.HistoryRetention)
	if counts.AuditLogs, err = execRetention(ctx, tx, `
		delete from audit_logs where id in (
		  select id from audit_logs where expires_at <= $1 order by expires_at limit $2
		)`, now, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if counts.AccessLinks, err = execRetention(ctx, tx, `
		delete from delivery_access_links where reference_hash in (
		  select reference_hash from delivery_access_links where expires_at <= $1 order by expires_at limit $2
		)`, now, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if counts.Deliveries, err = execRetention(ctx, tx, `
		delete from line_deliveries where id in (
		  select id from line_deliveries
		  where expires_at <= $1 and status in ('ACCEPTED', 'FAILED_PERMANENT')
		  order by expires_at limit $2
		)`, now, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if counts.NotificationRuns, err = execRetention(ctx, tx, `
		delete from notification_runs where id in (
		  select id from notification_runs
		  where created_at <= $1 and status in ('COMPLETED', 'PARTIAL_FAILED', 'FAILED', 'BLOCKED_QUOTA', 'CANCELLED')
		    and not exists (select 1 from line_deliveries delivery where delivery.notification_run_id = notification_runs.id)
		  order by created_at limit $2
		)`, historyCutoff, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if removed, err := execRetention(ctx, tx, `
		delete from report_runs where id in (
		  select report_run.id from report_runs report_run
		  where report_run.source = 'SCHEDULE' and report_run.created_at <= $1
		    and not exists (select 1 from notification_run_reports linked where linked.report_run_id = report_run.id)
		  order by report_run.created_at limit $2
		)`, historyCutoff, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	} else {
		counts.ReportRuns += removed
	}
	sessionCutoff := now.Add(-policy.ExpiredSessionGrace)
	for _, query := range []string{
		`delete from viewer_sessions where ctid in (select ctid from viewer_sessions where expires_at <= $1 or revoked_at <= $1 order by expires_at limit $2)`,
		`delete from admin_sessions where ctid in (select ctid from admin_sessions where expires_at <= $1 or revoked_at <= $1 order by expires_at limit $2)`,
	} {
		removed, err := execRetention(ctx, tx, query, sessionCutoff, policy.BatchSize)
		if err != nil {
			return retention.Counts{}, err
		}
		counts.Sessions += removed
	}
	if counts.IdempotencyRequests, err = execRetention(ctx, tx, `
		delete from idempotency_requests where ctid in (
		  select ctid from idempotency_requests where expires_at <= $1 order by expires_at limit $2
		)`, now, policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if _, err := execRetention(ctx, tx, `delete from worker_heartbeats where ctid in (select ctid from worker_heartbeats where heartbeat_at <= $1 order by heartbeat_at limit $2)`, now.Add(-7*24*time.Hour), policy.BatchSize); err != nil {
		return retention.Counts{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return retention.Counts{}, fmt.Errorf("commit retention: %w", err)
	}
	return counts, nil
}

func execRetention(ctx context.Context, tx pgx.Tx, query string, arguments ...any) (int64, error) {
	result, err := tx.Exec(ctx, query, arguments...)
	if err != nil {
		return 0, fmt.Errorf("execute retention step: %w", err)
	}
	return result.RowsAffected(), nil
}
