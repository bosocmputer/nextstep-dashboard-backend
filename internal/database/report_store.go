package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ReportStore struct {
	pool                   *pgxpool.Pool
	globalQueryConcurrency int
	hostQueryConcurrency   int
	generationCacheEnabled bool
}

func (store *ReportStore) ConfigureGenerationCache(enabled bool) *ReportStore {
	store.generationCacheEnabled = enabled
	return store
}

func NewReportStore(pool *pgxpool.Pool) *ReportStore {
	return &ReportStore{
		pool:                   pool,
		globalQueryConcurrency: boundedEnvInt("REPORT_GLOBAL_QUERY_CONCURRENCY", 4, 1, 32),
		hostQueryConcurrency:   boundedEnvInt("REPORT_HOST_QUERY_CONCURRENCY", 2, 1, 16),
	}
}

func boundedEnvInt(key string, fallback, minimum, maximum int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value < minimum || value > maximum {
		return fallback
	}
	return value
}

func (store *ReportStore) Enqueue(ctx context.Context, input report.EnqueueInput, now time.Time) (report.Run, error) {
	if _, ok := report.DefinitionFor(input.ReportKey); !ok || len(input.IdempotencyKey) < 8 || len(input.IdempotencyKey) > 200 {
		return report.Run{}, report.ErrRunIdempotencyConflict
	}
	if input.Source == report.SourceBackground && (input.RequestedByRecipient != nil || input.ExecutionKey == "" || input.ResultKind != report.ResultSummary) {
		return report.Run{}, report.ErrRunForbidden
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return report.Run{}, fmt.Errorf("begin enqueue report: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if input.ResultKind == "" {
		if input.Source == report.SourceSchedule || input.Source == report.SourceBackground {
			input.ResultKind = report.ResultSummary
		} else {
			input.ResultKind = report.ResultDetail
		}
	}
	if input.Priority == 0 {
		switch input.Source {
		case report.SourceSchedule:
			input.Priority = 100
		case report.SourceDashboard:
			input.Priority = 90
		default:
			definition, _ := report.DefinitionFor(input.ReportKey)
			switch definition.RefreshClass {
			case report.RefreshFast:
				input.Priority = 30
			case report.RefreshStandard:
				input.Priority = 25
			default:
				input.Priority = 20
			}
		}
	}
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
	reserveHalfOpenProbe := false
	if input.Source == report.SourceDashboard {
		var openUntil *time.Time
		var halfOpenRunID *uuid.UUID
		circuitErr := tx.QueryRow(ctx, `
			select open_until, half_open_run_id
			from tenant_sml_circuits where tenant_id = $1
			for update`, input.TenantID).Scan(&openUntil, &halfOpenRunID)
		if circuitErr != nil && !errors.Is(circuitErr, pgx.ErrNoRows) {
			return report.Run{}, fmt.Errorf("lock tenant SML circuit: %w", circuitErr)
		}
		if openUntil != nil && openUntil.After(now) {
			return report.Run{}, report.ErrRunCircuitOpen
		}
		if openUntil != nil && !openUntil.After(now) {
			if halfOpenRunID != nil {
				return report.Run{}, report.ErrRunCircuitOpen
			}
			reserveHalfOpenProbe = true
		}
	}
	if input.Source == report.SourceBackground {
		active, activeErr := scanReportRun(tx.QueryRow(ctx, `
			select `+reportRunColumns+` from report_runs
			where tenant_id = $1 and source = 'BACKGROUND' and execution_key = $2
			  and status in ('QUEUED', 'CLAIMED', 'RUNNING')
			order by queued_at, id limit 1`, input.TenantID, input.ExecutionKey), now)
		if activeErr == nil {
			if err := tx.Commit(ctx); err != nil {
				return report.Run{}, fmt.Errorf("commit background report join: %w", err)
			}
			return active, nil
		}
		if !errors.Is(activeErr, pgx.ErrNoRows) {
			return report.Run{}, fmt.Errorf("find active background report: %w", activeErr)
		}
		var queuedBackground int
		if err := tx.QueryRow(ctx, `select count(*) from report_runs where tenant_id = $1 and source = 'BACKGROUND' and status in ('QUEUED', 'CLAIMED', 'RUNNING')`, input.TenantID).Scan(&queuedBackground); err != nil {
			return report.Run{}, fmt.Errorf("count background report runs: %w", err)
		}
		if queuedBackground >= 10 {
			return report.Run{}, report.ErrRunConcurrencyLimit
		}
	}

	if input.Source == report.SourceDashboard {
		if input.RequestedByRecipient != nil {
			recent, recentErr := scanReportRun(tx.QueryRow(ctx, `
				select `+reportRunColumns+` from report_runs
				where tenant_id = $1 and source = 'DASHBOARD' and requested_by_recipient_id = $2
				  and report_key = $3 and period_from = $4::date and period_to = $5::date
				  and status in ('QUEUED', 'CLAIMED', 'RUNNING') and created_at >= $6::timestamptz - interval '60 seconds'
				order by created_at desc, id desc limit 1`, input.TenantID, input.RequestedByRecipient,
				input.ReportKey, input.Period.DateFrom, input.Period.DateTo, now), now)
			if recentErr == nil {
				if err := tx.Commit(ctx); err != nil {
					return report.Run{}, fmt.Errorf("commit recent dashboard report join: %w", err)
				}
				return recent, nil
			}
			if !errors.Is(recentErr, pgx.ErrNoRows) {
				return report.Run{}, fmt.Errorf("find recent dashboard report: %w", recentErr)
			}
		}
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

	var reportDefinitionVersion string
	var dataSourceVersion int
	if err := tx.QueryRow(ctx, `
		select d.version, coalesce(c.version, 0)
		from report_definitions d
		left join tenant_sml_connections c on c.tenant_id = $2
		where d.report_key = $1`, input.ReportKey, input.TenantID).Scan(&reportDefinitionVersion, &dataSourceVersion); err != nil {
		return report.Run{}, fmt.Errorf("load report execution versions: %w", err)
	}
	queryPlanFingerprint := report.QueryPlanFingerprint(input.ReportKey, input.ResultKind)
	if queryPlanFingerprint == "" {
		return report.Run{}, report.ErrRunNotFound
	}
	expectedP50MS, expectedP90MS, expectedSampleCount, err := loadDurationEstimate(ctx, tx, input.TenantID, input.ReportKey, input.Period)
	if err != nil {
		return report.Run{}, err
	}
	paramsJSON, _ := json.Marshal(map[string]string{"dateFrom": input.Period.DateFrom, "dateTo": input.Period.DateTo})
	row := tx.QueryRow(ctx, `
		insert into report_runs (
		  tenant_id, report_key, source, idempotency_key, status, period_preset,
		  period_from, period_to, params_json, requested_by_recipient_id,
		  queued_at, expires_at, created_at, updated_at, result_kind, priority,
		  execution_key, report_definition_version, data_source_version,
		  progress_phase, progress_updated_at, expected_p50_ms, expected_p90_ms,
		  expected_sample_count, query_plan_fingerprint
		) values ($1, $2, $3, $4, 'QUEUED', $5, nullif($6, '')::date, nullif($7, '')::date, $8, $9, $10, $11, $10, $10,
		          $12, $13, nullif($14, ''), $15, $16, 'QUEUED', $10, nullif($17, 0), nullif($18, 0), $19, $20)
		on conflict do nothing
		returning `+reportRunColumns,
		input.TenantID, input.ReportKey, input.Source, input.IdempotencyKey, input.Period.Preset,
		input.Period.DateFrom, input.Period.DateTo, paramsJSON, input.RequestedByRecipient, now, now.Add(24*time.Hour),
		input.ResultKind, input.Priority, input.ExecutionKey, reportDefinitionVersion, dataSourceVersion,
		expectedP50MS, expectedP90MS, expectedSampleCount, queryPlanFingerprint,
	)
	created, err := scanReportRun(row, now)
	if errors.Is(err, pgx.ErrNoRows) && input.Source == report.SourceBackground {
		created, err = scanReportRun(tx.QueryRow(ctx, `
			select `+reportRunColumns+` from report_runs
			where tenant_id = $1 and source = 'BACKGROUND' and execution_key = $2
			  and status in ('QUEUED', 'CLAIMED', 'RUNNING')
			order by queued_at, id limit 1`, input.TenantID, input.ExecutionKey), now)
	}
	if err != nil {
		return report.Run{}, fmt.Errorf("insert report run: %w", err)
	}
	if reserveHalfOpenProbe {
		result, reserveErr := tx.Exec(ctx, `
			update tenant_sml_circuits
			set half_open_run_id = $2, updated_at = $3
			where tenant_id = $1 and open_until > $3 and half_open_run_id is null`, input.TenantID, created.ID, now)
		if reserveErr != nil {
			return report.Run{}, fmt.Errorf("reserve tenant SML half-open probe: %w", reserveErr)
		}
		if result.RowsAffected() != 1 {
			return report.Run{}, report.ErrRunCircuitOpen
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return report.Run{}, fmt.Errorf("commit enqueue report: %w", err)
	}
	return created, nil
}

func loadDurationEstimate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, reportKey report.Key, period report.Period) (int64, int64, int, error) {
	from, fromErr := time.Parse("2006-01-02", period.DateFrom)
	to, toErr := time.Parse("2006-01-02", period.DateTo)
	if fromErr != nil || toErr != nil {
		return 0, 0, 0, nil
	}
	days := int(to.Sub(from).Hours()/24) + 1
	minimum, maximum := 1, 1
	switch {
	case days <= 1:
	case days <= 7:
		minimum, maximum = 2, 7
	case days <= 31:
		minimum, maximum = 8, 31
	case days <= 90:
		minimum, maximum = 32, 90
	default:
		minimum, maximum = 91, 366
	}
	var count int
	var p50, p90 float64
	err := tx.QueryRow(ctx, `
		with recent as (
		  select extract(epoch from (finished_at - started_at)) * 1000 as duration_ms
		  from report_runs
		  where tenant_id = $1 and report_key = $2 and status = 'SUCCEEDED'
		    and started_at is not null and finished_at is not null
		    and period_from is not null and period_to is not null
		    and (period_to - period_from + 1) between $3 and $4
		  order by finished_at desc
		  limit 30
		)
		select count(*), coalesce(percentile_cont(0.5) within group (order by duration_ms), 0),
		       coalesce(percentile_cont(0.9) within group (order by duration_ms), 0)
		from recent`, tenantID, reportKey, minimum, maximum).Scan(&count, &p50, &p90)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("load report duration estimate: %w", err)
	}
	if count < 5 {
		return 0, 0, count, nil
	}
	return int64(p50), int64(p90), count, nil
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
	if run.Status != report.StatusQueued {
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
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, int64(7214501625)); err != nil {
		return report.Run{}, fmt.Errorf("lock SML query admission: %w", err)
	}
	var runID uuid.UUID
	err = tx.QueryRow(ctx, `
		select r.id
		from report_runs r
		where r.status = 'QUEUED' and r.queued_at <= $1
		  and not exists (
		    select 1 from sml_connection_tests test
		    where test.tenant_id = r.tenant_id and test.status = 'RUNNING' and test.lease_expires_at > $1
		  )
		  and not exists (
		    select 1 from tenant_sml_circuits circuit
		    where circuit.tenant_id = r.tenant_id and circuit.open_until > $1
		  )
		  and ((select count(*) from report_runs system_active where system_active.status in ('CLAIMED', 'RUNNING'))
		       + (select count(*) from sml_connection_tests test where test.status = 'RUNNING' and test.lease_expires_at > $1)) < $2
		  and (
		    r.source = 'SCHEDULE'
		    or (select count(*) from report_runs interactive_active
		        where interactive_active.status in ('CLAIMED', 'RUNNING')
		          and interactive_active.source <> 'SCHEDULE') < greatest(1, $2 - 1)
		  )
		  and not exists (
		    select 1 from report_runs active
		    where active.tenant_id = r.tenant_id and active.id <> r.id
		      and active.status in ('CLAIMED', 'RUNNING')
		  )
		  and (
		    select count(*) from (
		      select host_active.id::text active_id
		      from report_runs host_active
		      join tenant_sml_connections active_connection on active_connection.tenant_id = host_active.tenant_id
		      join tenant_sml_connections candidate_connection on candidate_connection.tenant_id = r.tenant_id
		      where host_active.status in ('CLAIMED', 'RUNNING')
		        and active_connection.endpoint_host_key = candidate_connection.endpoint_host_key
		      union all
		      select test.lease_id::text
		      from sml_connection_tests test
		      join tenant_sml_connections active_connection on active_connection.tenant_id = test.tenant_id
		      join tenant_sml_connections candidate_connection on candidate_connection.tenant_id = r.tenant_id
		      where test.status = 'RUNNING' and test.lease_expires_at > $1
		        and active_connection.endpoint_host_key = candidate_connection.endpoint_host_key
		    ) host_work
		  ) < $3
		  and not exists (
		    select 1
		    from tenant_sml_connections candidate_connection
		    join sml_host_circuits host_circuit
		      on host_circuit.host_key = candidate_connection.endpoint_host_key
		     and host_circuit.open_until > $1
		    where candidate_connection.tenant_id = r.tenant_id
		  )
		  and not (
		    r.source in ('DASHBOARD', 'BACKGROUND')
		    and r.report_key in ('stock_balance', 'ar_customer_movement')
		    and exists (
		      select 1
		      from notification_schedules schedule
		      join tenant_sml_connections scheduled_connection on scheduled_connection.tenant_id = schedule.tenant_id
		      join tenant_sml_connections candidate_connection
		        on candidate_connection.tenant_id = r.tenant_id
		       and candidate_connection.endpoint_host_key = scheduled_connection.endpoint_host_key
		      where schedule.status = 'ACTIVE'
		        and schedule.next_run_at between $1::timestamptz and $1::timestamptz + make_interval(secs => greatest(300, least(900, coalesce(r.expected_p90_ms, 0) / 1000 + 60))::double precision)
		    )
		  )
		order by greatest(r.priority, least(99, r.priority + floor(extract(epoch from ($1 - r.queued_at)) / 600)::integer)) desc,
		         coalesce((select runtime.last_claimed_at from tenant_query_runtime runtime where runtime.tenant_id = r.tenant_id), '-infinity'::timestamptz),
		         r.queued_at, r.id
		for update skip locked
		limit 1`, now, store.globalQueryConcurrency, store.hostQueryConcurrency).Scan(&runID)
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
	if _, err := tx.Exec(ctx, `
		insert into tenant_query_runtime (tenant_id, last_claimed_at, updated_at)
		values ($1, $2, $2)
		on conflict (tenant_id) do update
		set last_claimed_at = excluded.last_claimed_at, updated_at = excluded.updated_at`, claimed.TenantID, now); err != nil {
		return report.Run{}, fmt.Errorf("record tenant report fairness: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return report.Run{}, fmt.Errorf("commit report claim: %w", err)
	}
	return claimed, nil
}

// RecoverExpiredLeases is deliberately separate from Claim. Recovery must
// commit even when every queued run is blocked by the newly-opened cooldown;
// coupling both operations caused expired RUNNING rows to be rolled back every
// time Claim returned ErrNoQueuedRun.
func (store *ReportStore) RecoverExpiredLeases(ctx context.Context, now time.Time) (report.LeaseRecovery, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return report.LeaseRecovery{}, fmt.Errorf("begin report lease recovery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	type expiredLease struct {
		ID       uuid.UUID
		TenantID uuid.UUID
		Status   report.RunStatus
		Source   report.Source
	}
	rows, err := tx.Query(ctx, `
		select id, tenant_id, status, source
		from report_runs
		where status in ('CLAIMED', 'RUNNING') and lease_expires_at < $1
		order by lease_expires_at, id
		for update skip locked
		limit 100`, now)
	if err != nil {
		return report.LeaseRecovery{}, fmt.Errorf("select expired report leases: %w", err)
	}
	expired := make([]expiredLease, 0)
	for rows.Next() {
		var item expiredLease
		if err := rows.Scan(&item.ID, &item.TenantID, &item.Status, &item.Source); err != nil {
			rows.Close()
			return report.LeaseRecovery{}, fmt.Errorf("scan expired report lease: %w", err)
		}
		expired = append(expired, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report.LeaseRecovery{}, fmt.Errorf("iterate expired report leases: %w", err)
	}
	rows.Close()

	result := report.LeaseRecovery{}
	for _, item := range expired {
		if item.Status == report.StatusClaimed {
			command, err := tx.Exec(ctx, `
				update report_runs
				set status = 'QUEUED', claimed_by = null, claimed_at = null,
				    lease_expires_at = null, progress_phase = 'WAITING_RETRY',
				    queued_at = least(queued_at, $2), progress_sequence = progress_sequence + 1,
				    progress_updated_at = $2, updated_at = $2
				where id = $1 and status = 'CLAIMED' and lease_expires_at < $2`, item.ID, now)
			if err != nil {
				return report.LeaseRecovery{}, fmt.Errorf("requeue expired claimed report: %w", err)
			}
			result.RequeuedClaimed += int(command.RowsAffected())
			continue
		}
		command, err := tx.Exec(ctx, `
			update report_runs
			set status = 'FAILED', safe_error_code = 'REPORT_LEASE_EXPIRED',
			    safe_error_message = 'Report worker lease expired after query dispatch.',
			    finished_at = $2, source_finished_at = $2, lease_expires_at = null,
			    progress_updated_at = $2, updated_at = $2
			where id = $1 and status = 'RUNNING' and lease_expires_at < $2`, item.ID, now)
		if err != nil {
			return report.LeaseRecovery{}, fmt.Errorf("fail expired running report: %w", err)
		}
		if command.RowsAffected() == 0 {
			continue
		}
		result.FailedRunning++
		if _, err := tx.Exec(ctx, `
			insert into tenant_sml_circuits (
			  tenant_id, consecutive_failures, window_started_at, open_until, half_open_run_id, updated_at
			) values ($1, 1, $2::timestamptz, $2::timestamptz + interval '10 minutes', null, $2::timestamptz)
			on conflict (tenant_id) do update
			set consecutive_failures = greatest(tenant_sml_circuits.consecutive_failures, 1),
			    window_started_at = coalesce(tenant_sml_circuits.window_started_at, $2::timestamptz),
			    open_until = greatest(coalesce(tenant_sml_circuits.open_until, $2::timestamptz), $2::timestamptz + interval '10 minutes'),
			    half_open_run_id = null, updated_at = $2::timestamptz`, item.TenantID, now); err != nil {
			return report.LeaseRecovery{}, fmt.Errorf("open circuit for expired running report: %w", err)
		}
		if item.Source == report.SourceSchedule {
			cancelled, err := failScheduledOccurrenceTx(ctx, tx, item.ID, now)
			if err != nil {
				return report.LeaseRecovery{}, err
			}
			result.CancelledSiblings += cancelled
		}
		if err := updateDashboardGenerationsForRun(ctx, tx, item.ID, now); err != nil {
			return report.LeaseRecovery{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return report.LeaseRecovery{}, fmt.Errorf("commit report lease recovery: %w", err)
	}
	return result, nil
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

func (store *ReportStore) UpdateProgress(ctx context.Context, runID uuid.UUID, workerID string, phase report.ProgressPhase, completedSteps, totalSteps int, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_runs
		set progress_phase = $3, progress_sequence = progress_sequence + 1,
		    progress_completed_steps = $4, progress_total_steps = $5,
		    progress_updated_at = $6, updated_at = $6
		where id = $1 and claimed_by = $2 and status = 'RUNNING'
		  and lease_expires_at >= $6
		  and case progress_phase
		    when 'QUEUED' then 0 when 'WAITING_RETRY' then 0
		    when 'CONNECTING' then 10 when 'QUERYING_CURRENT' then 20
		    when 'QUERYING_COMPARISON' then 30 when 'BUILDING_DASHBOARD' then 40
		    when 'SAVING_RESULT' then 50 when 'COMPLETED' then 60 else 0 end
		  < case $3
		    when 'CONNECTING' then 10 when 'QUERYING_CURRENT' then 20
		    when 'QUERYING_COMPARISON' then 30 when 'BUILDING_DASHBOARD' then 40
		    when 'SAVING_RESULT' then 50 when 'COMPLETED' then 60 else 0 end`,
		runID, workerID, phase, completedSteps, totalSteps, now)
	if err != nil {
		return fmt.Errorf("update report progress: %w", err)
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
		    progress_phase = 'WAITING_RETRY', progress_sequence = progress_sequence + 1,
		    progress_updated_at = $5, updated_at = $5
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING')
		  and lease_expires_at >= $5`,
		runID, workerID, safeCode, availableAt, now)
	if err != nil {
		return fmt.Errorf("retry report run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	return nil
}

func (store *ReportStore) RetryPreRequestFailure(ctx context.Context, runID uuid.UUID, workerID, safeCode string, availableAt, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin retry pre-request report failure: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenantID uuid.UUID
	err = tx.QueryRow(ctx, `
		update report_runs
		set status = 'QUEUED', queued_at = $4, claimed_by = null, claimed_at = null,
		    lease_expires_at = null, safe_error_code = $3, safe_error_message = null,
		    progress_phase = 'WAITING_RETRY', progress_sequence = progress_sequence + 1,
		    progress_updated_at = $5, updated_at = $5
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING')
		  and lease_expires_at >= $5
		returning tenant_id`, runID, workerID, safeCode, availableAt, now).Scan(&tenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.ErrRunLeaseLost
	}
	if err != nil {
		return fmt.Errorf("retry pre-request report failure: %w", err)
	}
	if err := recordHostPreRequestFailureTx(ctx, tx, tenantID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit retry pre-request report failure: %w", err)
	}
	return nil
}

func (store *ReportStore) FailPreRequestFailure(ctx context.Context, runID uuid.UUID, workerID string, evidence failure.Evidence, now time.Time) error {
	evidence = normalizeFailureEvidence(evidence, now)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail pre-request report failure: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenantID uuid.UUID
	var source report.Source
	err = tx.QueryRow(ctx, `
		update report_runs
		set status = 'FAILED', safe_error_code = $3, safe_error_message = $4,
		    finished_at = $5, source_finished_at = $5, lease_expires_at = null,
		    progress_updated_at = $5, updated_at = $5,
		    failure_evidence_version = $6, failure_category = $7, failure_stage = $8,
		    failure_transport_phase = nullif($9, ''), failure_occurred_at = $10,
		    failure_duration_ms = $11, failure_attempt = $12, failure_retryable = $13,
		    failure_remote_state_unknown = $14
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING')
		  and lease_expires_at >= $5
		returning tenant_id, source`, runID, workerID, evidence.SafeErrorCode, failure.SafeMessage(evidence.SafeErrorCode), now,
		evidence.Version, evidence.Category, evidence.Stage, evidence.TransportPhase, evidence.OccurredAt,
		evidence.DurationMS, evidence.Attempt, evidence.Retryable, evidence.RemoteStateUnknown).Scan(&tenantID, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.ErrRunLeaseLost
	}
	if err != nil {
		return fmt.Errorf("fail pre-request report failure: %w", err)
	}
	if err := recordHostPreRequestFailureTx(ctx, tx, tenantID, now); err != nil {
		return err
	}
	if source == report.SourceSchedule {
		if _, err := failScheduledOccurrenceTx(ctx, tx, runID, now); err != nil {
			return err
		}
	}
	if err := updateDashboardGenerationsForRun(ctx, tx, runID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fail pre-request report failure: %w", err)
	}
	return nil
}

func normalizeFailureEvidence(evidence failure.Evidence, now time.Time) failure.Evidence {
	if evidence.OccurredAt.IsZero() {
		evidence.OccurredAt = now
	}
	if evidence.FinishedAt == nil {
		finishedAt := now
		evidence.FinishedAt = &finishedAt
	}
	return failure.Complete(evidence)
}

func recordHostPreRequestFailureTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, now time.Time) error {
	result, err := tx.Exec(ctx, `
		insert into sml_host_circuits (
		  host_key, consecutive_failures, window_started_at, open_until, half_open_run_id, updated_at
		)
		select connection.endpoint_host_key, 1, $2, null, null, $2
		from tenant_sml_connections connection
		where connection.tenant_id = $1
		on conflict (host_key) do update
		set consecutive_failures = case
		      when sml_host_circuits.window_started_at is null
		        or sml_host_circuits.window_started_at < $2 - interval '2 minutes' then 1
		      else sml_host_circuits.consecutive_failures + 1 end,
		    window_started_at = case
		      when sml_host_circuits.window_started_at is null
		        or sml_host_circuits.window_started_at < $2 - interval '2 minutes' then $2
		      else sml_host_circuits.window_started_at end,
		    open_until = case
		      when (case
		        when sml_host_circuits.window_started_at is null
		          or sml_host_circuits.window_started_at < $2 - interval '2 minutes' then 1
		        else sml_host_circuits.consecutive_failures + 1 end) >= 2
		      then greatest(coalesce(sml_host_circuits.open_until, $2), $2 + interval '5 minutes')
		      else sml_host_circuits.open_until end,
		    half_open_run_id = null,
		    updated_at = $2`, tenantID, now)
	if err != nil {
		return fmt.Errorf("record SML host pre-request failure: %w", err)
	}
	if result.RowsAffected() != 1 {
		return fmt.Errorf("record SML host pre-request failure: connection missing")
	}
	return nil
}

func (store *ReportStore) MarkRunning(ctx context.Context, runID uuid.UUID, workerID string, lease time.Duration, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update report_runs
		set status = 'RUNNING', started_at = coalesce(started_at, $3), source_started_at = coalesce(source_started_at, $3),
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
		    finished_at = $8, source_finished_at = $8, expires_at = $9, lease_expires_at = null,
		    progress_phase = 'COMPLETED', progress_sequence = progress_sequence + 1,
		    progress_completed_steps = progress_total_steps, progress_updated_at = $8, updated_at = $8
		where id = $1 and claimed_by = $2 and status = 'RUNNING'`,
		runID, workerID, summary.RowCount, summaryJSON, reconciliationJSON, dashboardVersion, dashboardJSON, now, expiresAt)
	if err != nil {
		return fmt.Errorf("complete report run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return report.ErrRunLeaseLost
	}
	if _, err := tx.Exec(ctx, `delete from report_run_chunks where run_id = $1`, runID); err != nil {
		return fmt.Errorf("clear completed report chunks: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into tenant_sml_circuits (tenant_id, consecutive_failures, window_started_at, open_until, half_open_run_id, updated_at)
		values ($1, 0, null, null, null, $2)
		on conflict (tenant_id) do update
		set consecutive_failures = 0, window_started_at = null, open_until = null,
		    half_open_run_id = null, updated_at = excluded.updated_at`, tenantID, now); err != nil {
		return fmt.Errorf("reset tenant SML circuit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update sml_host_circuits host_circuit
		set consecutive_failures = 0, window_started_at = null, open_until = null,
		    half_open_run_id = null, updated_at = $2
		from tenant_sml_connections connection
		where connection.tenant_id = $1
		  and host_circuit.host_key = connection.endpoint_host_key`, tenantID, now); err != nil {
		return fmt.Errorf("reset SML host circuit: %w", err)
	}
	if err := updateDashboardGenerationsForRun(ctx, tx, runID, now); err != nil {
		return err
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
	fingerprints := make([]string, len(reportKeys))
	for index, key := range reportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return nil, report.ErrRunForbidden
		}
		keys[index] = string(key)
		fingerprints[index] = report.QueryPlanFingerprint(key, report.ResultSummary)
	}
	policy, err := NewRefreshPolicyStore(store.pool).GetRefreshPolicy(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		with requested as (
		  select report_key, query_plan_fingerprint, ordinal
		  from unnest($2::text[], $3::text[]) with ordinality
		       as input(report_key, query_plan_fingerprint, ordinal)
		), latest as (
		  select distinct on (requested.ordinal)
		         requested.ordinal, r.id, r.report_key, r.dashboard_json,
		         r.period_from::text, r.period_to::text,
		         coalesce(r.source_started_at, r.started_at) as source_started_at,
		         coalesce(r.source_finished_at, r.finished_at) as source_finished_at,
		         r.report_definition_version, r.data_source_version, r.query_plan_fingerprint,
		         r.source_consistency, r.result_kind, r.expires_at
		  from requested
		  join report_runs r on r.tenant_id = $1 and r.report_key = requested.report_key
		    and r.query_plan_fingerprint = requested.query_plan_fingerprint
		  join report_definitions d on d.report_key = r.report_key and d.version = r.report_definition_version
		  join tenant_sml_connections c on c.tenant_id = r.tenant_id and c.version = r.data_source_version
		  where r.status = 'SUCCEEDED' and r.dashboard_json <> '{}'::jsonb
		  order by requested.ordinal, r.finished_at desc nulls last, r.id desc
		)
		select id, report_key, dashboard_json, period_from, period_to,
		       source_started_at, source_finished_at, report_definition_version,
		       data_source_version, query_plan_fingerprint, source_consistency, result_kind, expires_at
		from latest order by ordinal`, tenantID, keys, fingerprints)
	if err != nil {
		return nil, fmt.Errorf("list latest report dashboards: %w", err)
	}
	defer rows.Close()
	items := make([]viewer.DashboardSnapshot, 0, len(reportKeys))
	for rows.Next() {
		var item viewer.DashboardSnapshot
		var reportKey report.Key
		var dashboardJSON []byte
		var resultKind report.ResultKind
		var expiresAt time.Time
		if err := rows.Scan(&item.RunID, &reportKey, &dashboardJSON, &item.PeriodFrom, &item.PeriodTo,
			&item.SourceStartedAt, &item.SourceFinishedAt, &item.ReportDefinitionVersion,
			&item.DataSourceVersion, &item.QueryPlanFingerprint, &item.SourceConsistency,
			&resultKind, &expiresAt); err != nil {
			return nil, fmt.Errorf("scan latest report dashboard: %w", err)
		}
		if err := json.Unmarshal(dashboardJSON, &item.Dashboard); err != nil {
			return nil, fmt.Errorf("decode latest report dashboard: %w", err)
		}
		if item.Dashboard.ReportKey != reportKey {
			return nil, fmt.Errorf("stored dashboard report key does not match run")
		}
		definition, _ := report.DefinitionFor(reportKey)
		period := report.Period{Preset: item.Dashboard.Period.Preset, DateFrom: item.PeriodFrom, DateTo: item.PeriodTo}
		if err := finalizeSnapshot(&item, resultKind, expiresAt, definition, period, policy, time.Now().UTC()); err != nil {
			continue
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
		matches, matchErr := dashboardRefreshInputsMatch(ctx, tx, existingID, inputs)
		if matchErr != nil {
			return viewer.DashboardRefresh{}, matchErr
		}
		if !matches {
			return viewer.DashboardRefresh{}, report.ErrRunIdempotencyConflict
		}
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
	var dataSourceVersion int
	if err := tx.QueryRow(ctx, `select coalesce((select version from tenant_sml_connections where tenant_id = $1), 0)`, tenantID).Scan(&dataSourceVersion); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("load dashboard refresh data source version: %w", err)
	}
	if !store.generationCacheEnabled {
		refreshID, createErr := createLegacyDashboardRefreshTx(ctx, tx, recipientID, tenantID, idempotencyKey, inputs, dataSourceVersion, now)
		if createErr != nil {
			return viewer.DashboardRefresh{}, createErr
		}
		if err := tx.Commit(ctx); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("commit legacy dashboard refresh: %w", err)
		}
		return store.GetDashboardRefresh(ctx, recipientID, tenantID, refreshID, now)
	}

	descriptor, err := describeDashboardGeneration(tenantID, inputs, dataSourceVersion)
	if err != nil {
		return viewer.DashboardRefresh{}, err
	}
	var generationID uuid.UUID
	joinedGeneration := false
	err = tx.QueryRow(ctx, `
		select id from dashboard_generations
		where tenant_id = $1 and generation_key = $2 and status = 'BUILDING'
		for update`, tenantID, descriptor.GenerationKey).Scan(&generationID)
	if err == nil {
		joinedGeneration = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return viewer.DashboardRefresh{}, fmt.Errorf("find active dashboard generation: %w", err)
	} else {
		var activeGenerations int
		if err := tx.QueryRow(ctx, `
			select count(*) from dashboard_generations
			where tenant_id = $1 and status = 'BUILDING'`, tenantID).Scan(&activeGenerations); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("count active dashboard generations: %w", err)
		}
		// One generation may execute and one may wait behind the tenant-level
		// query lock. Further distinct periods are rejected instead of growing
		// an unbounded SML queue.
		if activeGenerations >= 2 {
			return viewer.DashboardRefresh{}, report.ErrRunConcurrencyLimit
		}
		generationID = uuid.New()
		if _, err := tx.Exec(ctx, `
			insert into dashboard_generations (
			  id, tenant_id, generation_key, period_preset, period_from, period_to,
			  request_json, report_set_hash, query_plan_set_fingerprint,
			  data_source_version, projection, source_consistency, total, created_at, updated_at
			) values ($1, $2, $3, $4, $5::date, $6::date, $7, $8, $9, $10, 'SUMMARY', 'SERIAL_WINDOW', $11, $12, $12)`,
			generationID, tenantID, descriptor.GenerationKey, descriptor.PeriodPreset,
			descriptor.PeriodFrom, descriptor.PeriodTo, descriptor.RequestJSON,
			descriptor.ReportSetHash, descriptor.QueryPlanSetFingerprint,
			dataSourceVersion, len(inputs), now); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("insert dashboard generation: %w", err)
		}
	}

	refreshID := uuid.New()
	if _, err := tx.Exec(ctx, `
		insert into dashboard_refreshes (id, tenant_id, requested_by_recipient_id, idempotency_key, total, generation_id, request_hash, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $8)`, refreshID, tenantID, recipientID, idempotencyKey, len(inputs), generationID, descriptor.GenerationKey, now); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("insert dashboard refresh: %w", err)
	}
	if joinedGeneration {
		if _, err := tx.Exec(ctx, `
			insert into dashboard_refresh_runs (refresh_id, report_key, report_run_id)
			select $1, report_key, report_run_id
			from dashboard_generation_reports where generation_id = $2`, refreshID, generationID); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("join dashboard refresh to generation: %w", err)
		}
	}
	for position, input := range inputs {
		if joinedGeneration {
			continue
		}
		runID := uuid.New()
		queryPlanFingerprint := report.QueryPlanFingerprint(input.ReportKey, report.ResultSummary)
		definition, _ := report.DefinitionFor(input.ReportKey)
		priority := 84
		if definition.RefreshClass == report.RefreshFast {
			priority = 85
		} else if definition.RefreshClass == report.RefreshHeavy {
			priority = 83
		}
		executionKey := snapshotExecutionKey(tenantID, input.ReportKey, input.Period, definition.ParameterKind, definition.Version, dataSourceVersion, queryPlanFingerprint, report.ResultSummary)
		activeRunErr := tx.QueryRow(ctx, `
			select id from report_runs
			where tenant_id = $1 and execution_key = $2 and result_kind = 'SUMMARY'
			  and source in ('DASHBOARD', 'BACKGROUND')
			  and status in ('QUEUED', 'CLAIMED', 'RUNNING')
			order by queued_at, id limit 1`, tenantID, executionKey).Scan(&runID)
		if activeRunErr != nil && !errors.Is(activeRunErr, pgx.ErrNoRows) {
			return viewer.DashboardRefresh{}, fmt.Errorf("find coalesced summary run: %w", activeRunErr)
		}
		paramsJSON, _ := json.Marshal(map[string]string{"dateFrom": input.Period.DateFrom, "dateTo": input.Period.DateTo})
		runIdempotency := "overview:" + refreshID.String() + ":" + string(input.ReportKey)
		if errors.Is(activeRunErr, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `
			insert into report_runs (
			  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
			  period_from, period_to, params_json, requested_by_recipient_id,
			  queued_at, expires_at, created_at, updated_at, result_kind, priority,
			  report_definition_version, data_source_version, progress_phase, progress_updated_at,
			  query_plan_fingerprint, execution_key
			)
			select $1, $2, $3, 'DASHBOARD', $4, 'QUEUED', $5, $6::date, $7::date, $8, $9,
			       $10, $11, $10, $10, 'SUMMARY', $12, d.version, coalesce(c.version, 0), 'QUEUED', $10,
			       $13, $14
			from report_definitions d
			left join tenant_sml_connections c on c.tenant_id = $2
			where d.report_key = $3`,
				runID, tenantID, input.ReportKey, runIdempotency, input.Period.Preset, input.Period.DateFrom, input.Period.DateTo,
				paramsJSON, recipientID, now, now.Add(24*time.Hour), priority, queryPlanFingerprint, executionKey); err != nil {
				return viewer.DashboardRefresh{}, fmt.Errorf("insert dashboard refresh report run: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			insert into dashboard_refresh_runs (refresh_id, report_key, report_run_id)
			values ($1, $2, $3)`, refreshID, input.ReportKey, runID); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("link dashboard refresh report run: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into dashboard_generation_reports (generation_id, tenant_id, report_key, report_run_id, position)
			values ($1, $2, $3, $4, $5)`, generationID, tenantID, input.ReportKey, runID, position+1); err != nil {
			return viewer.DashboardRefresh{}, fmt.Errorf("link dashboard generation report run: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("commit dashboard refresh: %w", err)
	}
	return store.GetDashboardRefresh(ctx, recipientID, tenantID, refreshID, now)
}

func createLegacyDashboardRefreshTx(ctx context.Context, tx pgx.Tx, recipientID, tenantID uuid.UUID, idempotencyKey string, inputs []report.EnqueueInput, dataSourceVersion int, now time.Time) (uuid.UUID, error) {
	var active bool
	if err := tx.QueryRow(ctx, `
		select exists (
		  select 1 from dashboard_refreshes refresh
		  join dashboard_refresh_runs linked on linked.refresh_id = refresh.id
		  join report_runs run on run.id = linked.report_run_id
		  where refresh.tenant_id = $1 and run.status in ('QUEUED', 'CLAIMED', 'RUNNING')
		)`, tenantID).Scan(&active); err != nil {
		return uuid.Nil, fmt.Errorf("check active dashboard refresh: %w", err)
	}
	if active {
		return uuid.Nil, report.ErrRunConcurrencyLimit
	}
	refreshID := uuid.New()
	if _, err := tx.Exec(ctx, `
		insert into dashboard_refreshes (id, tenant_id, requested_by_recipient_id, idempotency_key, total, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $6)`, refreshID, tenantID, recipientID, idempotencyKey, len(inputs), now); err != nil {
		return uuid.Nil, fmt.Errorf("insert legacy dashboard refresh: %w", err)
	}
	for _, input := range inputs {
		runID := uuid.New()
		queryPlanFingerprint := report.QueryPlanFingerprint(input.ReportKey, report.ResultSummary)
		definition, _ := report.DefinitionFor(input.ReportKey)
		priority := 84
		if definition.RefreshClass == report.RefreshFast {
			priority = 85
		} else if definition.RefreshClass == report.RefreshHeavy {
			priority = 83
		}
		executionKey := snapshotExecutionKey(tenantID, input.ReportKey, input.Period, definition.ParameterKind, definition.Version, dataSourceVersion, queryPlanFingerprint, report.ResultSummary)
		paramsJSON, _ := json.Marshal(map[string]string{"dateFrom": input.Period.DateFrom, "dateTo": input.Period.DateTo})
		if _, err := tx.Exec(ctx, `
			insert into report_runs (
			  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
			  period_from, period_to, params_json, requested_by_recipient_id,
			  queued_at, expires_at, created_at, updated_at, result_kind, priority,
			  report_definition_version, data_source_version, progress_phase, progress_updated_at,
			  query_plan_fingerprint, execution_key
			)
			select $1, $2, $3, 'DASHBOARD', $4, 'QUEUED', $5, $6::date, $7::date, $8, $9,
			       $10, $11, $10, $10, 'SUMMARY', $12, d.version, coalesce(c.version, 0), 'QUEUED', $10,
			       $13, $14
			from report_definitions d
			left join tenant_sml_connections c on c.tenant_id = $2
			where d.report_key = $3`,
			runID, tenantID, input.ReportKey, "overview:"+refreshID.String()+":"+string(input.ReportKey),
			input.Period.Preset, input.Period.DateFrom, input.Period.DateTo, paramsJSON, recipientID,
			now, now.Add(24*time.Hour), priority, queryPlanFingerprint, executionKey); err != nil {
			return uuid.Nil, fmt.Errorf("insert legacy dashboard report run: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into dashboard_refresh_runs (refresh_id, report_key, report_run_id)
			values ($1, $2, $3)`, refreshID, input.ReportKey, runID); err != nil {
			return uuid.Nil, fmt.Errorf("link legacy dashboard report run: %w", err)
		}
	}
	return refreshID, nil
}

func (store *ReportStore) GetDashboardRefreshResult(ctx context.Context, recipientID, tenantID, refreshID uuid.UUID) (viewer.DashboardRefreshResult, error) {
	result := viewer.DashboardRefreshResult{Items: []viewer.DashboardSnapshot{}, Failures: []viewer.DashboardRefreshFailure{}}
	policy, err := NewRefreshPolicyStore(store.pool).GetRefreshPolicy(ctx, tenantID)
	if err != nil {
		return viewer.DashboardRefreshResult{}, err
	}
	var generationStatus *string
	var generationKey *string
	var sourceConsistency *report.SourceConsistency
	err = store.pool.QueryRow(ctx, `
		select refresh.id, refresh.tenant_id, refresh.generation_id,
		       generation.generation_key, generation.status, generation.source_consistency,
		       generation.source_started_at, generation.source_finished_at, generation.published_at
		from dashboard_refreshes refresh
		left join dashboard_generations generation on generation.id = refresh.generation_id
		where refresh.id = $1 and refresh.tenant_id = $2 and refresh.requested_by_recipient_id = $3`,
		refreshID, tenantID, recipientID).Scan(&result.RefreshID, &result.TenantID, &result.GenerationID,
		&generationKey, &generationStatus, &sourceConsistency,
		&result.SourceStartedAt, &result.SourceFinishedAt, &result.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return viewer.DashboardRefreshResult{}, report.ErrRunNotFound
	}
	if err != nil {
		return viewer.DashboardRefreshResult{}, fmt.Errorf("get dashboard refresh generation: %w", err)
	}
	if generationKey != nil {
		result.GenerationKey = *generationKey
	}
	if sourceConsistency != nil {
		result.SourceConsistency = *sourceConsistency
	}
	if generationStatus != nil && *generationStatus == "BUILDING" {
		return viewer.DashboardRefreshResult{}, viewer.ErrDashboardRefreshNotReady
	}
	rows, err := store.pool.Query(ctx, `
		select refresh.id, refresh.tenant_id, linked.report_key, linked.report_run_id,
		       run.status, coalesce(run.safe_error_code, ''), run.dashboard_json,
		       run.period_from::text, run.period_to::text,
		       coalesce(run.source_started_at, run.started_at, run.queued_at),
		       coalesce(run.source_finished_at, run.finished_at),
		       run.report_definition_version, run.data_source_version,
		       run.query_plan_fingerprint, run.source_consistency, run.result_kind, run.expires_at
		from dashboard_refreshes refresh
		join dashboard_refresh_runs linked on linked.refresh_id = refresh.id
		join report_runs run on run.id = linked.report_run_id
		where refresh.id = $1 and refresh.tenant_id = $2 and refresh.requested_by_recipient_id = $3
		order by linked.report_key`, refreshID, tenantID, recipientID)
	if err != nil {
		return viewer.DashboardRefreshResult{}, fmt.Errorf("get dashboard refresh result: %w", err)
	}
	defer rows.Close()
	queued, running := 0, 0
	for rows.Next() {
		var reportKey report.Key
		var runID uuid.UUID
		var status report.RunStatus
		var safeCode string
		var dashboardJSON []byte
		var snapshot viewer.DashboardSnapshot
		var resultKind report.ResultKind
		var expiresAt time.Time
		if err := rows.Scan(&result.RefreshID, &result.TenantID, &reportKey, &runID, &status, &safeCode, &dashboardJSON,
			&snapshot.PeriodFrom, &snapshot.PeriodTo, &snapshot.SourceStartedAt, &snapshot.SourceFinishedAt,
			&snapshot.ReportDefinitionVersion, &snapshot.DataSourceVersion, &snapshot.QueryPlanFingerprint,
			&snapshot.SourceConsistency, &resultKind, &expiresAt); err != nil {
			return viewer.DashboardRefreshResult{}, fmt.Errorf("scan dashboard refresh result: %w", err)
		}
		switch status {
		case report.StatusQueued:
			queued++
		case report.StatusClaimed, report.StatusRunning:
			running++
		case report.StatusSucceeded:
			if err := json.Unmarshal(dashboardJSON, &snapshot.Dashboard); err != nil || snapshot.Dashboard.ReportKey != reportKey || snapshot.Dashboard.Version == "" {
				return viewer.DashboardRefreshResult{}, fmt.Errorf("decode dashboard refresh result for %s", reportKey)
			}
			snapshot.RunID = runID
			definition, _ := report.DefinitionFor(reportKey)
			period := report.Period{Preset: snapshot.Dashboard.Period.Preset, DateFrom: snapshot.PeriodFrom, DateTo: snapshot.PeriodTo}
			if err := finalizeSnapshot(&snapshot, resultKind, expiresAt, definition, period, policy, time.Now().UTC()); err != nil {
				return viewer.DashboardRefreshResult{}, fmt.Errorf("finalize dashboard refresh snapshot for %s: %w", reportKey, err)
			}
			result.Items = append(result.Items, snapshot)
		case report.StatusFailed, report.StatusCancelled, report.StatusExpired:
			result.Failures = append(result.Failures, viewer.DashboardRefreshFailure{ReportKey: reportKey, Status: status, SafeErrorCode: safeCode})
		default:
			return viewer.DashboardRefreshResult{}, fmt.Errorf("dashboard refresh result has unknown run status %q", status)
		}
	}
	if err := rows.Err(); err != nil {
		return viewer.DashboardRefreshResult{}, fmt.Errorf("iterate dashboard refresh result: %w", err)
	}
	if result.RefreshID == uuid.Nil {
		return viewer.DashboardRefreshResult{}, report.ErrRunNotFound
	}
	if (generationStatus == nil || *generationStatus != "FAILED") && (queued > 0 || running > 0) {
		return viewer.DashboardRefreshResult{}, viewer.ErrDashboardRefreshNotReady
	}
	switch {
	case generationStatus != nil && *generationStatus == "FAILED":
		result.Items = []viewer.DashboardSnapshot{}
		result.Status = viewer.DashboardRefreshFailed
	case len(result.Items) > 0 && len(result.Failures) == 0:
		result.Status = viewer.DashboardRefreshSucceeded
	case len(result.Items) > 0:
		result.Status = viewer.DashboardRefreshPartial
	default:
		result.Status = viewer.DashboardRefreshFailed
	}
	return result, nil
}

func dashboardRefreshInputsMatch(ctx context.Context, tx pgx.Tx, refreshID uuid.UUID, inputs []report.EnqueueInput) (bool, error) {
	rows, err := tx.Query(ctx, `
		select linked.report_key, run.period_preset, run.period_from::text, run.period_to::text
		from dashboard_refresh_runs linked
		join report_runs run on run.id = linked.report_run_id
		where linked.refresh_id = $1`, refreshID)
	if err != nil {
		return false, fmt.Errorf("compare dashboard refresh replay: %w", err)
	}
	defer rows.Close()
	want := make(map[report.Key]report.Period, len(inputs))
	for _, input := range inputs {
		want[input.ReportKey] = input.Period
	}
	seen := 0
	for rows.Next() {
		var key report.Key
		var period report.Period
		if err := rows.Scan(&key, &period.Preset, &period.DateFrom, &period.DateTo); err != nil {
			return false, fmt.Errorf("scan dashboard refresh replay: %w", err)
		}
		if expected, ok := want[key]; !ok || expected != period {
			return false, nil
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate dashboard refresh replay: %w", err)
	}
	return seen == len(want), nil
}

func (store *ReportStore) GetDashboardRefresh(ctx context.Context, recipientID, tenantID, refreshID uuid.UUID, now time.Time) (viewer.DashboardRefresh, error) {
	var refresh viewer.DashboardRefresh
	var generationStatus *string
	err := store.pool.QueryRow(ctx, `
		select refresh.id, refresh.tenant_id, refresh.generation_id, refresh.total, refresh.created_at, refresh.finished_at,
		       generation.status
		from dashboard_refreshes refresh
		left join dashboard_generations generation on generation.id = refresh.generation_id
		where refresh.id = $1 and refresh.tenant_id = $2 and refresh.requested_by_recipient_id = $3`, refreshID, tenantID, recipientID).Scan(
		&refresh.ID, &refresh.TenantID, &refresh.GenerationID, &refresh.Total, &refresh.CreatedAt, &refresh.FinishedAt, &generationStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return viewer.DashboardRefresh{}, report.ErrRunNotFound
	}
	if err != nil {
		return viewer.DashboardRefresh{}, fmt.Errorf("get dashboard refresh: %w", err)
	}
	rows, err := store.pool.Query(ctx, `
		select linked.report_key, linked.report_run_id, run.status, run.progress_phase,
		       run.progress_updated_at, coalesce(run.expected_p50_ms, 0),
		       coalesce(run.expected_p90_ms, 0), run.expected_sample_count,
		       run.execution_strategy, run.source_consistency,
		       run.progress_completed_chunks, run.progress_total_chunks
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
		if err := rows.Scan(&item.ReportKey, &item.RunID, &item.Status, &item.ProgressPhase,
			&item.ProgressUpdatedAt, &item.ExpectedP50MS, &item.ExpectedP90MS, &item.ExpectedSampleCount,
			&item.ExecutionStrategy, &item.SourceConsistency, &item.CompletedChunks, &item.TotalChunks); err != nil {
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
	case generationStatus != nil && *generationStatus == "FAILED":
		refresh.Status = viewer.DashboardRefreshFailed
	case generationStatus != nil && *generationStatus == "PUBLISHED":
		refresh.Status = viewer.DashboardRefreshSucceeded
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

func (store *ReportStore) Fail(ctx context.Context, runID uuid.UUID, workerID string, evidence failure.Evidence, now time.Time) error {
	evidence = normalizeFailureEvidence(evidence, now)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail report run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenantID uuid.UUID
	var source report.Source
	err = tx.QueryRow(ctx, `
		update report_runs
		set status = 'FAILED', safe_error_code = $3, safe_error_message = $4,
		    finished_at = $5, lease_expires_at = null, progress_updated_at = $5, updated_at = $5,
		    failure_evidence_version = $6, failure_category = $7, failure_stage = $8,
		    failure_transport_phase = nullif($9, ''), failure_occurred_at = $10,
		    failure_duration_ms = $11, failure_attempt = $12, failure_retryable = $13,
		    failure_remote_state_unknown = $14
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING')
		  and lease_expires_at >= $5
		returning tenant_id, source`, runID, workerID, evidence.SafeErrorCode, failure.SafeMessage(evidence.SafeErrorCode), now,
		evidence.Version, evidence.Category, evidence.Stage, evidence.TransportPhase, evidence.OccurredAt,
		evidence.DurationMS, evidence.Attempt, evidence.Retryable, evidence.RemoteStateUnknown).Scan(&tenantID, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.ErrRunLeaseLost
	}
	if err != nil {
		return fmt.Errorf("fail report run: %w", err)
	}
	if isSMLCircuitFailure(evidence.SafeErrorCode) {
		if _, err := tx.Exec(ctx, `
			insert into tenant_sml_circuits (tenant_id, consecutive_failures, window_started_at, open_until, updated_at)
			values ($1, 1, $2, null, $2)
			on conflict (tenant_id) do update
			set consecutive_failures = case
			      when tenant_sml_circuits.window_started_at is null or tenant_sml_circuits.window_started_at < $2 - interval '10 minutes' then 1
			      else tenant_sml_circuits.consecutive_failures + 1 end,
			    window_started_at = case
			      when tenant_sml_circuits.window_started_at is null or tenant_sml_circuits.window_started_at < $2 - interval '10 minutes' then $2
			      else tenant_sml_circuits.window_started_at end,
			    open_until = case
			      when (case when tenant_sml_circuits.window_started_at is null or tenant_sml_circuits.window_started_at < $2 - interval '10 minutes' then 1 else tenant_sml_circuits.consecutive_failures + 1 end) >= 3
			      then $2 + interval '15 minutes' else tenant_sml_circuits.open_until end,
			    half_open_run_id = case
			      when tenant_sml_circuits.window_started_at is null or tenant_sml_circuits.window_started_at < $2 - interval '10 minutes' then null
			      else tenant_sml_circuits.half_open_run_id end,
			    updated_at = $2`, tenantID, now); err != nil {
			return fmt.Errorf("record tenant SML circuit failure: %w", err)
		}
	}
	if source == report.SourceSchedule {
		if _, err := failScheduledOccurrenceTx(ctx, tx, runID, now); err != nil {
			return err
		}
	}
	if err := updateDashboardGenerationsForRun(ctx, tx, runID, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FailRemoteUnknown atomically fails a dispatched JavaWS request and opens the
// tenant uncertainty circuit. A timeout after bytes were sent is not safe to
// retry because PostgreSQL may still be executing behind JavaWS.
func (store *ReportStore) FailRemoteUnknown(ctx context.Context, runID uuid.UUID, workerID string, evidence failure.Evidence, now, openUntil time.Time) error {
	if !openUntil.After(now) {
		return fmt.Errorf("uncertainty circuit expiry must be in the future")
	}
	evidence = normalizeFailureEvidence(evidence, now)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail uncertain report run: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenantID uuid.UUID
	var source report.Source
	err = tx.QueryRow(ctx, `
		update report_runs
		set status = 'FAILED', safe_error_code = $3, safe_error_message = $4,
		    finished_at = $5, source_finished_at = $5, lease_expires_at = null,
		    progress_updated_at = $5, updated_at = $5,
		    failure_evidence_version = $6, failure_category = $7, failure_stage = $8,
		    failure_transport_phase = nullif($9, ''), failure_occurred_at = $10,
		    failure_duration_ms = $11, failure_attempt = $12, failure_retryable = $13,
		    failure_remote_state_unknown = $14
		where id = $1 and claimed_by = $2 and status in ('CLAIMED', 'RUNNING')
		  and lease_expires_at >= $5
		returning tenant_id, source`, runID, workerID, evidence.SafeErrorCode, failure.SafeMessage(evidence.SafeErrorCode), now,
		evidence.Version, evidence.Category, evidence.Stage, evidence.TransportPhase, evidence.OccurredAt,
		evidence.DurationMS, evidence.Attempt, evidence.Retryable, evidence.RemoteStateUnknown).Scan(&tenantID, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.ErrRunLeaseLost
	}
	if err != nil {
		return fmt.Errorf("fail uncertain report run: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into tenant_sml_circuits (
		  tenant_id, consecutive_failures, window_started_at, open_until, half_open_run_id, updated_at
		) values ($1, 1, $2, $3, null, $2)
		on conflict (tenant_id) do update
		set consecutive_failures = greatest(tenant_sml_circuits.consecutive_failures, 1),
		    window_started_at = coalesce(tenant_sml_circuits.window_started_at, $2),
		    open_until = greatest(coalesce(tenant_sml_circuits.open_until, $2), $3),
		    half_open_run_id = null,
		    updated_at = $2`, tenantID, now, openUntil); err != nil {
		return fmt.Errorf("open tenant uncertainty circuit: %w", err)
	}
	if source == report.SourceSchedule {
		if _, err := failScheduledOccurrenceTx(ctx, tx, runID, now); err != nil {
			return err
		}
	}
	if err := updateDashboardGenerationsForRun(ctx, tx, runID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fail uncertain report run: %w", err)
	}
	return nil
}

// failScheduledOccurrenceTx closes the immutable occurrence immediately and
// removes queued sibling work. It is called in the same transaction that makes
// a report terminal, preventing another worker lane from claiming a sibling in
// the gap between report failure and notification failure.
func failScheduledOccurrenceTx(ctx context.Context, tx pgx.Tx, failedRunID uuid.UUID, now time.Time) (int, error) {
	var notificationRunID uuid.UUID
	err := tx.QueryRow(ctx, `
		update notification_runs notification
		set status = 'FAILED', safe_error_code = 'REPORT_SET_INCOMPLETE',
		    finished_at = $2, lease_expires_at = null, updated_at = $2
		from notification_run_reports linked
		where linked.report_run_id = $1
		  and notification.id = linked.notification_run_id
		  and notification.status in ('QUEUED', 'COLLECTING', 'READY')
		returning notification.id`, failedRunID, now).Scan(&notificationRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("fail incomplete notification occurrence: %w", err)
	}
	command, err := tx.Exec(ctx, `
		update report_runs sibling
		set status = 'CANCELLED', safe_error_code = 'REPORT_SET_INCOMPLETE',
		    safe_error_message = null, finished_at = $3, lease_expires_at = null,
		    progress_updated_at = $3, updated_at = $3
		from notification_run_reports linked
		where linked.notification_run_id = $1
		  and linked.report_run_id = sibling.id
		  and sibling.id <> $2
		  and sibling.status = 'QUEUED'`, notificationRunID, failedRunID, now)
	if err != nil {
		return 0, fmt.Errorf("cancel incomplete notification siblings: %w", err)
	}
	return int(command.RowsAffected()), nil
}

func isSMLCircuitFailure(code string) bool {
	switch code {
	case "SML_TIMEOUT", "SML_UNREACHABLE", "SML_QUERY_FAILED", "SML_CONNECTION_LOAD_FAILED", "SML_RESPONSE_READ_FAILED":
		return true
	default:
		return false
	}
}

func (store *ReportStore) Get(ctx context.Context, runID uuid.UUID, now time.Time) (report.Run, error) {
	run, err := scanReportRun(store.pool.QueryRow(ctx, `select `+reportRunColumns+` from report_runs where id = $1`, runID), now)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.Run{}, report.ErrRunNotFound
	}
	if err != nil {
		return report.Run{}, fmt.Errorf("get report run: %w", err)
	}
	if run.Status == report.StatusQueued {
		if err := store.pool.QueryRow(ctx, `
			with target as (
			  select tenant_id, priority, queued_at, id,
			         greatest(priority, least(99, priority + floor(extract(epoch from ($2 - queued_at)) / 600)::integer)) as effective_priority
			  from report_runs where id = $1
			)
			select count(*) + 1
			from report_runs candidate, target
			where candidate.tenant_id = target.tenant_id and candidate.status = 'QUEUED' and candidate.id <> target.id
			  and (
			    greatest(candidate.priority, least(99, candidate.priority + floor(extract(epoch from ($2 - candidate.queued_at)) / 600)::integer)) > target.effective_priority
			    or (greatest(candidate.priority, least(99, candidate.priority + floor(extract(epoch from ($2 - candidate.queued_at)) / 600)::integer)) = target.effective_priority
			        and (candidate.queued_at, candidate.id) < (target.queued_at, target.id))
			  )`, runID, now).Scan(&run.QueuePosition); err != nil {
			return report.Run{}, fmt.Errorf("get report queue position: %w", err)
		}
	}
	return run, nil
}

// CanAccessScheduledRun binds a scheduled snapshot to the LINE recipient that
// received the notification containing its deep link. Tenant/report permission
// checks remain in the viewer service and are intentionally evaluated as well.
func (store *ReportStore) CanAccessScheduledRun(ctx context.Context, recipientID, runID uuid.UUID) (bool, error) {
	var allowed bool
	if err := store.pool.QueryRow(ctx, `
		select exists (
		  select 1
		  from notification_run_reports linked
		  join line_deliveries delivery
		    on delivery.notification_run_id = linked.notification_run_id
		   and delivery.recipient_id = $2
		  where linked.report_run_id = $1
		)`, runID, recipientID).Scan(&allowed); err != nil {
		return false, fmt.Errorf("check scheduled report recipient: %w", err)
	}
	return allowed, nil
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

func (store *ReportStore) QueryRows(ctx context.Context, runID uuid.UUID, input report.RowsQueryInput, now time.Time) (report.RowsQueryPage, error) {
	if input.Page < 0 || input.Page > 200_000 || input.PageSize < 1 || input.PageSize > 100 || len(input.Filters) > 5 {
		return report.RowsQueryPage{}, errors.New("invalid report row query")
	}
	var status report.RunStatus
	var expiresAt time.Time
	if err := store.pool.QueryRow(ctx, `select status, expires_at from report_runs where id = $1`, runID).Scan(&status, &expiresAt); errors.Is(err, pgx.ErrNoRows) {
		return report.RowsQueryPage{}, report.ErrRunNotFound
	} else if err != nil {
		return report.RowsQueryPage{}, fmt.Errorf("read report row query status: %w", err)
	}
	if status == report.StatusExpired || !expiresAt.After(now) {
		return report.RowsQueryPage{}, report.ErrRunRowsExpired
	}
	if status != report.StatusSucceeded {
		return report.RowsQueryPage{}, report.ErrRunNotFound
	}
	filtersJSON, err := json.Marshal(input.Filters)
	if err != nil {
		return report.RowsQueryPage{}, fmt.Errorf("encode report row filters: %w", err)
	}
	rows, err := store.pool.Query(ctx, `
		with filtered as (
		  select stored.ordinal, stored.row_json
		  from report_run_rows stored
		  where stored.run_id = $1
		    and not exists (
		      select 1
		      from jsonb_array_elements($2::jsonb) selected
		      where not case selected ->> 'operator'
		        when 'CONTAINS' then strpos(lower(coalesce(stored.row_json ->> (selected ->> 'columnKey'), '')), lower(selected ->> 'value')) > 0
		        when 'EQUALS' then case
		          when selected ->> 'valueType' = 'NUMBER' then
		            coalesce(stored.row_json ->> (selected ->> 'columnKey'), '') ~ $numeric$^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$numeric$
		            and (stored.row_json ->> (selected ->> 'columnKey'))::numeric = (selected ->> 'value')::numeric
		          else coalesce(stored.row_json ->> (selected ->> 'columnKey'), '') = selected ->> 'value'
		        end
		        when 'GTE' then case
		          when selected ->> 'valueType' = 'NUMBER' and coalesce(stored.row_json ->> (selected ->> 'columnKey'), '') ~ $numeric$^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$numeric$
		            and (selected ->> 'value') ~ $numeric$^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$numeric$
		          then (stored.row_json ->> (selected ->> 'columnKey'))::numeric >= (selected ->> 'value')::numeric
		          when selected ->> 'valueType' = 'NUMBER' then false
		          else coalesce(stored.row_json ->> (selected ->> 'columnKey'), '') >= selected ->> 'value'
		        end
		        when 'LTE' then case
		          when selected ->> 'valueType' = 'NUMBER' and coalesce(stored.row_json ->> (selected ->> 'columnKey'), '') ~ $numeric$^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$numeric$
		            and (selected ->> 'value') ~ $numeric$^[+-]?([0-9]+([.][0-9]*)?|[.][0-9]+)([eE][+-]?[0-9]+)?$numeric$
		          then (stored.row_json ->> (selected ->> 'columnKey'))::numeric <= (selected ->> 'value')::numeric
		          when selected ->> 'valueType' = 'NUMBER' then false
		          else coalesce(stored.row_json ->> (selected ->> 'columnKey'), '') <= selected ->> 'value'
		        end
		        else false
		      end
		    )
		)
		select row_json, count(*) over()
		from filtered
		order by ordinal
		offset $3 limit $4`, runID, filtersJSON, input.Page*input.PageSize, input.PageSize)
	if err != nil {
		return report.RowsQueryPage{}, fmt.Errorf("query report rows: %w", err)
	}
	defer rows.Close()
	page := report.RowsQueryPage{Rows: make([]map[string]string, 0, input.PageSize), Page: input.Page, PageSize: input.PageSize}
	for rows.Next() {
		var rowJSON []byte
		if err := rows.Scan(&rowJSON, &page.Total); err != nil {
			return report.RowsQueryPage{}, fmt.Errorf("scan queried report row: %w", err)
		}
		var row map[string]string
		if err := json.Unmarshal(rowJSON, &row); err != nil {
			return report.RowsQueryPage{}, fmt.Errorf("decode queried report row: %w", err)
		}
		page.Rows = append(page.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return report.RowsQueryPage{}, fmt.Errorf("iterate queried report rows: %w", err)
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
id, tenant_id, report_key, source, result_kind, priority, coalesce(execution_key, ''), idempotency_key, status, period_preset,
period_from::text, period_to::text, requested_by_recipient_id,
coalesce(claimed_by, ''), lease_expires_at, attempt, row_count, is_truncated,
summary_json, reconciliation_json, coalesce(safe_error_code, ''),
coalesce(safe_error_message, ''), queued_at, started_at, finished_at,
expires_at, created_at, updated_at, coalesce(report_definition_version, ''),
coalesce(data_source_version, 0) as data_source_version, progress_phase, progress_sequence,
progress_completed_steps, progress_total_steps, progress_updated_at,
coalesce(expected_p50_ms, 0), coalesce(expected_p90_ms, 0), expected_sample_count,
coalesce(query_plan_fingerprint, ''), execution_strategy, source_consistency,
source_started_at, source_finished_at, progress_completed_chunks, progress_total_chunks,
failure_evidence_version, failure_category, failure_stage, failure_transport_phase,
failure_occurred_at, failure_duration_ms, failure_attempt, failure_retryable,
failure_remote_state_unknown`

func scanReportRun(row rowScanner, now time.Time) (report.Run, error) {
	return scanReportRunWithExtras(row, now)
}

func scanReportRunWithExtras(row rowScanner, now time.Time, extraDestinations ...any) (report.Run, error) {
	var run report.Run
	var summaryJSON, reconciliationJSON []byte
	var evidenceVersion *int
	var evidenceCategory, evidenceStage, evidenceTransportPhase *string
	var evidenceOccurredAt *time.Time
	var evidenceDurationMS *int64
	var evidenceAttempt *int
	var evidenceRetryable, evidenceRemoteStateUnknown *bool
	destinations := []any{
		&run.ID, &run.TenantID, &run.ReportKey, &run.Source, &run.ResultKind, &run.Priority, &run.ExecutionKey, &run.IdempotencyKey,
		&run.Status, &run.Period.Preset, &run.Period.DateFrom, &run.Period.DateTo,
		&run.RequestedByRecipient, &run.ClaimedBy, &run.LeaseExpiresAt, &run.Attempt,
		&run.RowCount, &run.IsTruncated, &summaryJSON, &reconciliationJSON,
		&run.SafeErrorCode, &run.SafeErrorMessage, &run.QueuedAt, &run.StartedAt,
		&run.FinishedAt, &run.ExpiresAt, &run.CreatedAt, &run.UpdatedAt,
		&run.ReportDefinitionVersion, &run.DataSourceVersion, &run.ProgressPhase, &run.ProgressSequence,
		&run.ProgressCompletedSteps, &run.ProgressTotalSteps, &run.ProgressUpdatedAt,
		&run.ExpectedP50MS, &run.ExpectedP90MS, &run.ExpectedSampleCount,
		&run.QueryPlanFingerprint, &run.ExecutionStrategy, &run.SourceConsistency,
		&run.SourceStartedAt, &run.SourceFinishedAt, &run.ProgressCompletedChunks, &run.ProgressTotalChunks,
		&evidenceVersion, &evidenceCategory, &evidenceStage, &evidenceTransportPhase,
		&evidenceOccurredAt, &evidenceDurationMS, &evidenceAttempt, &evidenceRetryable,
		&evidenceRemoteStateUnknown,
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
	if evidenceVersion != nil && evidenceCategory != nil && evidenceStage != nil && evidenceOccurredAt != nil && evidenceRetryable != nil && evidenceRemoteStateUnknown != nil {
		evidence := failure.Evidence{
			Version: *evidenceVersion, Level: failure.LevelConfirmed,
			Category: failure.Category(*evidenceCategory), Stage: failure.Stage(*evidenceStage),
			OccurredAt: *evidenceOccurredAt, DurationMS: evidenceDurationMS, Attempt: evidenceAttempt,
			Retryable: *evidenceRetryable, RemoteStateUnknown: *evidenceRemoteStateUnknown,
			SafeErrorCode: run.SafeErrorCode,
		}
		if evidenceTransportPhase != nil {
			evidence.TransportPhase = failure.TransportPhase(*evidenceTransportPhase)
		}
		if run.StartedAt != nil {
			evidence.StartedAt = run.StartedAt
		}
		if run.FinishedAt != nil {
			evidence.FinishedAt = run.FinishedAt
		}
		if run.DataSourceVersion > 0 {
			connectionVersion := run.DataSourceVersion
			evidence.ConnectionVersion = &connectionVersion
		}
		evidence = failure.Complete(evidence)
		run.FailureEvidence = &evidence
	}
	if run.Status == report.StatusSucceeded && !run.ExpiresAt.After(now) {
		run.Status = report.StatusExpired
	}
	return run, nil
}
