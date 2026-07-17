package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type SMLTestCoordinator struct {
	pool              *pgxpool.Pool
	globalConcurrency int
	hostConcurrency   int
	lease             time.Duration
	cooldown          time.Duration
	quietWindow       time.Duration
}

func NewSMLTestCoordinator(pool *pgxpool.Pool) *SMLTestCoordinator {
	return &SMLTestCoordinator{
		pool:              pool,
		globalConcurrency: boundedEnvInt("REPORT_GLOBAL_QUERY_CONCURRENCY", 4, 1, 32),
		hostConcurrency:   boundedEnvInt("REPORT_HOST_QUERY_CONCURRENCY", 2, 1, 16),
		lease:             45 * time.Second, cooldown: 60 * time.Second, quietWindow: 5 * time.Minute,
	}
}

func (coordinator *SMLTestCoordinator) Acquire(ctx context.Context, tenantID uuid.UUID, now time.Time) (sml.ConnectionTestPermit, error) {
	tx, err := coordinator.pool.Begin(ctx)
	if err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("begin SML connection test admission: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, int64(7214501625)); err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("lock SML query admission: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into sml_connection_tests (tenant_id, status, updated_at) values ($1, 'IDLE', $2)
		on conflict (tenant_id) do update
		set status = case when sml_connection_tests.status = 'RUNNING' and sml_connection_tests.lease_expires_at <= $2 then 'IDLE' else sml_connection_tests.status end,
		    lease_id = case when sml_connection_tests.status = 'RUNNING' and sml_connection_tests.lease_expires_at <= $2 then null else sml_connection_tests.lease_id end,
		    started_at = case when sml_connection_tests.status = 'RUNNING' and sml_connection_tests.lease_expires_at <= $2 then null else sml_connection_tests.started_at end,
		    lease_expires_at = case when sml_connection_tests.status = 'RUNNING' and sml_connection_tests.lease_expires_at <= $2 then null else sml_connection_tests.lease_expires_at end,
		    updated_at = $2`, tenantID, now); err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("prepare SML connection test state: %w", err)
	}
	var status string
	var leaseExpiresAt, cooldownUntil *time.Time
	if err := tx.QueryRow(ctx, `select status, lease_expires_at, cooldown_until from sml_connection_tests where tenant_id = $1 for update`, tenantID).Scan(&status, &leaseExpiresAt, &cooldownUntil); err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("lock SML connection test state: %w", err)
	}
	if status == "RUNNING" && leaseExpiresAt != nil && leaseExpiresAt.After(now) {
		return sml.ConnectionTestPermit{}, &sml.ConnectionTestBlockError{SafeCode: "SML_TEST_BUSY", RetryAfter: *leaseExpiresAt}
	}
	if cooldownUntil != nil && cooldownUntil.After(now) {
		return sml.ConnectionTestPermit{}, &sml.ConnectionTestBlockError{SafeCode: "SML_TEST_COOLDOWN", RetryAfter: *cooldownUntil}
	}
	var blockedUntil *time.Time
	err = tx.QueryRow(ctx, `
		select max(retry_at) from (
		  select max(run.lease_expires_at) retry_at from report_runs run
		  where run.tenant_id = $1 and run.status in ('CLAIMED', 'RUNNING')
		  union all
		  select max(circuit.open_until) from tenant_sml_circuits circuit
		  where circuit.tenant_id = $1 and circuit.open_until > $2
		  union all
		  select max(schedule.next_run_at + interval '1 minute') from notification_schedules schedule
		  where schedule.tenant_id = $1 and schedule.status = 'ACTIVE'
		    and schedule.next_run_at between $2 and $2 + $3::bigint * interval '1 second'
		) blocked`, tenantID, now, int64(coordinator.quietWindow/time.Second)).Scan(&blockedUntil)
	if err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("inspect SML connection test blockers: %w", err)
	}
	if blockedUntil != nil {
		return sml.ConnectionTestPermit{}, &sml.ConnectionTestBlockError{SafeCode: "SML_TEST_BUSY", RetryAfter: *blockedUntil}
	}
	var globalActive, hostActive int
	if err := tx.QueryRow(ctx, `
		select
		  (select count(*) from report_runs run where run.status in ('CLAIMED', 'RUNNING'))
		    + (select count(*) from sml_connection_tests test where test.status = 'RUNNING' and test.lease_expires_at > $1),
		  (select count(*) from (
		    select run.id::text
		    from report_runs run
		    join tenant_sml_connections active on active.tenant_id = run.tenant_id
		    join tenant_sml_connections candidate on candidate.tenant_id = $2
		    where run.status in ('CLAIMED', 'RUNNING') and active.endpoint_host_key = candidate.endpoint_host_key
		    union all
		    select test.lease_id::text
		    from sml_connection_tests test
		    join tenant_sml_connections active on active.tenant_id = test.tenant_id
		    join tenant_sml_connections candidate on candidate.tenant_id = $2
		    where test.status = 'RUNNING' and test.lease_expires_at > $1 and active.endpoint_host_key = candidate.endpoint_host_key
		  ) host_work)`, now, tenantID).Scan(&globalActive, &hostActive); err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("inspect SML connection test capacity: %w", err)
	}
	if globalActive >= coordinator.globalConcurrency || hostActive >= coordinator.hostConcurrency {
		return sml.ConnectionTestPermit{}, &sml.ConnectionTestBlockError{SafeCode: "SML_TEST_BUSY", RetryAfter: now.Add(time.Minute)}
	}
	permit := sml.ConnectionTestPermit{TenantID: tenantID, LeaseID: uuid.New()}
	if _, err := tx.Exec(ctx, `
		update sml_connection_tests set status = 'RUNNING', lease_id = $2, started_at = $3,
		lease_expires_at = $4, updated_at = $3 where tenant_id = $1`, tenantID, permit.LeaseID, now, now.Add(coordinator.lease)); err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("claim SML connection test: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sml.ConnectionTestPermit{}, fmt.Errorf("commit SML connection test admission: %w", err)
	}
	return permit, nil
}

func (coordinator *SMLTestCoordinator) Complete(ctx context.Context, permit sml.ConnectionTestPermit, now time.Time, remoteStateUnknown bool) error {
	tx, err := coordinator.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin SML connection test completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, int64(7214501625)); err != nil {
		return fmt.Errorf("lock SML query admission: %w", err)
	}
	cooldownUntil := now.Add(coordinator.cooldown)
	if remoteStateUnknown {
		cooldownUntil = now.Add(10 * time.Minute)
	}
	result, err := tx.Exec(ctx, `
		update sml_connection_tests
		set status = 'IDLE', lease_id = null, started_at = null, lease_expires_at = null,
		    cooldown_until = $3, updated_at = $2
		where tenant_id = $1 and lease_id = $4 and status = 'RUNNING'`, permit.TenantID, now, cooldownUntil, permit.LeaseID)
	if err != nil {
		return fmt.Errorf("complete SML connection test: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("SML connection test permit was lost")
	}
	if remoteStateUnknown {
		if _, err := tx.Exec(ctx, `
			insert into tenant_sml_circuits (tenant_id, consecutive_failures, window_started_at, open_until, half_open_run_id, updated_at)
			values ($1, 1, $2, $3, null, $2)
			on conflict (tenant_id) do update set consecutive_failures = greatest(tenant_sml_circuits.consecutive_failures, 1),
			window_started_at = coalesce(tenant_sml_circuits.window_started_at, $2),
			open_until = greatest(coalesce(tenant_sml_circuits.open_until, $2), $3), half_open_run_id = null, updated_at = $2`, permit.TenantID, now, cooldownUntil); err != nil {
			return fmt.Errorf("open uncertainty circuit after SML connection test: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit SML connection test completion: %w", err)
	}
	return nil
}
