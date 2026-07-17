package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type OperationsStore struct {
	pool *pgxpool.Pool
}

func (store *OperationsStore) GetReportRunDetail(ctx context.Context, runID uuid.UUID, now time.Time) (operations.ReportRunDetail, error) {
	var detail operations.ReportRunDetail
	var reportsTotal, reportsSucceeded, reportsFailed, reportsCancelled int
	err := func() error {
		row := store.pool.QueryRow(ctx, `
			select r.*, tenant.name,
			       coalesce(impact.reports_total, 1),
			       coalesce(impact.reports_succeeded, case when r.status = 'SUCCEEDED' then 1 else 0 end),
			       coalesce(impact.reports_failed, case when r.status = 'FAILED' then 1 else 0 end),
			       coalesce(impact.reports_cancelled, case when r.status = 'CANCELLED' then 1 else 0 end),
			       coalesce(notification.trigger_kind, case when r.source = 'SCHEDULE' then 'UNKNOWN' else r.source end),
			       case
			         when linked.notification_run_id is null then 'NOT_APPLICABLE'
			         when exists (select 1 from line_deliveries delivery where delivery.notification_run_id = linked.notification_run_id) then 'CREATED'
			         when notification.safe_error_code in ('REPORT_SET_INCOMPLETE', 'ALL_REPORTS_FAILED') then 'NOT_CREATED_INCOMPLETE_REPORT_SET'
			         else 'UNKNOWN'
			       end,
			       case when r.failure_evidence_version is null or r.data_source_version is null or r.data_source_version <= 0 then false
			            when connection.version is null then true
			            else connection.version <> r.data_source_version end
			from (
			  select `+reportRunColumns+` from report_runs where id = $1
			) r
			join tenants tenant on tenant.id = r.tenant_id
			left join lateral (
			  select materialized.notification_run_id
			  from notification_run_reports materialized
			  where materialized.report_run_id = r.id
			  order by materialized.notification_run_id
			  limit 1
			) linked on true
			left join notification_runs notification on notification.id = linked.notification_run_id
			left join lateral (
			  select count(*)::integer as reports_total,
			         count(*) filter (where sibling.status = 'SUCCEEDED')::integer as reports_succeeded,
			         count(*) filter (where sibling.status = 'FAILED')::integer as reports_failed,
			         count(*) filter (where sibling.status = 'CANCELLED')::integer as reports_cancelled
			  from notification_run_reports occurrence_report
			  join report_runs sibling on sibling.id = occurrence_report.report_run_id
			  where occurrence_report.notification_run_id = linked.notification_run_id
			) impact on linked.notification_run_id is not null
			left join tenant_sml_connections connection on connection.tenant_id = r.tenant_id
			where r.id = $1`, runID)
		run, err := scanReportRunWithExtras(row, now, &detail.TenantName,
			&reportsTotal, &reportsSucceeded, &reportsFailed, &reportsCancelled,
			&detail.TriggerKind, &detail.Impact.Notification, &detail.ConnectionChangedSinceFailure)
		if err != nil {
			return err
		}
		detail.Run = run
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return operations.ReportRunDetail{}, report.ErrRunNotFound
	}
	if err != nil {
		return operations.ReportRunDetail{}, fmt.Errorf("get admin report run detail: %w", err)
	}
	detail.Impact.ReportsTotal = reportsTotal
	detail.Impact.ReportsSucceeded = reportsSucceeded
	detail.Impact.ReportsFailed = reportsFailed
	detail.Impact.ReportsCancelled = reportsCancelled
	return detail, nil
}

func NewOperationsStore(pool *pgxpool.Pool) *OperationsStore {
	return &OperationsStore{pool: pool}
}

func (store *OperationsStore) GetLineQuota(ctx context.Context, now time.Time) (operations.LineQuotaStatus, error) {
	return NewQuotaStore(store.pool).Get(ctx, now)
}

func (store *OperationsStore) ListReportRuns(ctx context.Context, filter operations.ReportRunFilter) (operations.ReportRunPage, error) {
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return operations.ReportRunPage{}, err
	}
	var status *string
	if filter.Status != nil {
		value := string(*filter.Status)
		status = &value
	}
	rows, err := store.pool.Query(ctx, `
		select `+reportRunColumns+`,
		       (select name from tenants where id = r.tenant_id),
		       case when r.status in ('CLAIMED', 'RUNNING') and r.lease_expires_at < $6 then 'STALLED' else 'ACTIVE' end,
		       (select max(retry_at) from (
		          select circuit.open_until as retry_at
		          from tenant_sml_circuits circuit
		          where circuit.tenant_id = r.tenant_id and circuit.open_until > $6
		          union all
		          select host_circuit.open_until
		          from tenant_sml_connections connection
		          join sml_host_circuits host_circuit on host_circuit.host_key = connection.endpoint_host_key
		          where connection.tenant_id = r.tenant_id and host_circuit.open_until > $6
		       ) retry_times),
		       case when r.status <> 'QUEUED' then null
		         when exists (select 1 from tenant_sml_circuits circuit where circuit.tenant_id = r.tenant_id and circuit.open_until > $6) then 'TENANT_COOLDOWN'
		         when exists (
		           select 1 from tenant_sml_connections connection
		           join sml_host_circuits host_circuit on host_circuit.host_key = connection.endpoint_host_key
		           where connection.tenant_id = r.tenant_id and host_circuit.open_until > $6
		         ) then 'HOST_COOLDOWN'
		         when exists (select 1 from report_runs active where active.tenant_id = r.tenant_id and active.status in ('CLAIMED', 'RUNNING')) then 'TENANT_BUSY'
		         when (select count(*) from report_runs active
		               join tenant_sml_connections active_connection on active_connection.tenant_id = active.tenant_id
		               join tenant_sml_connections candidate_connection on candidate_connection.tenant_id = r.tenant_id
		               where active.status in ('CLAIMED', 'RUNNING')
		                 and active_connection.endpoint_host_key = candidate_connection.endpoint_host_key) >= $7 then 'HOST_BUSY'
		         when r.source <> 'SCHEDULE' and (
		           (select count(*) from report_runs active where active.status in ('CLAIMED', 'RUNNING') and active.source <> 'SCHEDULE') >= $8
		           or (r.report_key in ('stock_balance', 'ar_customer_movement') and exists (
		             select 1 from notification_schedules schedule
		             join tenant_sml_connections scheduled_connection on scheduled_connection.tenant_id = schedule.tenant_id
		             join tenant_sml_connections candidate_connection on candidate_connection.tenant_id = r.tenant_id
		               and candidate_connection.endpoint_host_key = scheduled_connection.endpoint_host_key
		             where schedule.status = 'ACTIVE' and schedule.next_run_at between $6 and $6 + interval '15 minutes'
		           ))
		         ) then 'SCHEDULE_RESERVED'
		         else null end
		from report_runs r
		where ($1::uuid is null or r.tenant_id = $1)
		  and ($2::text is null or r.status = $2)
		  and ($3::timestamptz is null or (r.created_at, r.id) < ($3, $4))
		order by r.created_at desc, r.id desc
		limit $5`, filter.TenantID, status, cursorTime, cursorID, filter.PageSize+1,
		filter.Now, boundedEnvInt("REPORT_HOST_QUERY_CONCURRENCY", 2, 1, 16), max(1, boundedEnvInt("REPORT_GLOBAL_QUERY_CONCURRENCY", 4, 1, 32)-1))
	if err != nil {
		return operations.ReportRunPage{}, fmt.Errorf("list admin report runs: %w", err)
	}
	defer rows.Close()
	items := make([]operations.ReportRun, 0, filter.PageSize+1)
	for rows.Next() {
		var item operations.ReportRun
		run, err := scanReportRunWithExtras(rows, filter.Now, &item.TenantName, &item.RuntimeStatus, &item.RetryAvailableAt, &item.WaitReason)
		if err != nil {
			return operations.ReportRunPage{}, fmt.Errorf("scan admin report run: %w", err)
		}
		item.Run = run
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return operations.ReportRunPage{}, fmt.Errorf("iterate admin report runs: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1].Run
		nextCursor = encodeTenantCursor(last.CreatedAt, last.ID)
	}
	return operations.ReportRunPage{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func (store *OperationsStore) ListDeliveries(ctx context.Context, filter operations.DeliveryFilter) (operations.DeliveryPage, error) {
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return operations.DeliveryPage{}, err
	}
	rows, err := store.pool.Query(ctx, `
		select delivery.id, delivery.tenant_id, tenant.name,
		       recipient.id, recipient.line_user_id_hash,
		       recipient.display_name_ciphertext, recipient.display_name_nonce, recipient.encryption_key_id,
		       coalesce(actual_reports.report_keys, '{}'::text[]),
		       delivery.status, delivery.attempt, delivery.safe_error_code, delivery.provider_request_id,
		       delivery.accepted_at, delivery.created_at, delivery.expires_at
		from line_deliveries delivery
		join tenants tenant on tenant.id = delivery.tenant_id
		join line_recipients recipient on recipient.id = delivery.recipient_id
		left join lateral (
		  select array_agg(linked.report_key order by linked.position nulls last, linked.report_key) as report_keys
		  from notification_run_reports linked
		  where linked.notification_run_id = delivery.notification_run_id
		) actual_reports on true
		where ($1::uuid is null or delivery.tenant_id = $1)
		  and ($2::timestamptz is null or (delivery.created_at, delivery.id) < ($2, $3))
		order by delivery.created_at desc, delivery.id desc
		limit $4`, filter.TenantID, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return operations.DeliveryPage{}, fmt.Errorf("list LINE deliveries: %w", err)
	}
	defer rows.Close()
	items := make([]operations.Delivery, 0, filter.PageSize+1)
	for rows.Next() {
		var item operations.Delivery
		var reportKeys []string
		if err := rows.Scan(
			&item.ID, &item.TenantID, &item.TenantName,
			&item.StoredRecipient.ID, &item.StoredRecipient.LineUserIDHash,
			&item.StoredRecipient.DisplayName.Ciphertext, &item.StoredRecipient.DisplayName.Nonce, &item.StoredRecipient.DisplayName.KeyID,
			&reportKeys,
			&item.Status, &item.Attempt, &item.SafeErrorCode, &item.ProviderRequestID,
			&item.AcceptedAt, &item.CreatedAt, &item.ExpiresAt,
		); err != nil {
			return operations.DeliveryPage{}, fmt.Errorf("scan LINE delivery: %w", err)
		}
		item.ReportKeys = make([]report.Key, len(reportKeys))
		for index, reportKey := range reportKeys {
			item.ReportKeys[index] = report.Key(reportKey)
		}
		item.ReportCount = len(item.ReportKeys)
		item.StoredRecipient.TenantID = item.TenantID
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return operations.DeliveryPage{}, fmt.Errorf("iterate LINE deliveries: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1]
		nextCursor = encodeTenantCursor(last.CreatedAt, last.ID)
	}
	return operations.DeliveryPage{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func (store *OperationsStore) ListAudit(ctx context.Context, filter operations.AuditFilter) (operations.AuditPage, error) {
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return operations.AuditPage{}, err
	}
	rows, err := store.pool.Query(ctx, `
		select audit.id, audit.tenant_id, tenant.name, audit.actor_type, audit.action,
		       audit.resource_type, audit.resource_id, audit.result, audit.safe_error_code, audit.created_at
		from audit_logs audit
		left join tenants tenant on tenant.id = audit.tenant_id
		where ($1::uuid is null or audit.tenant_id = $1)
		  and ($2::timestamptz is null or (audit.created_at, audit.id) < ($2, $3))
		order by audit.created_at desc, audit.id desc
		limit $4`, filter.TenantID, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return operations.AuditPage{}, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	items := make([]operations.AuditEvent, 0, filter.PageSize+1)
	for rows.Next() {
		var item operations.AuditEvent
		if err := rows.Scan(&item.ID, &item.TenantID, &item.TenantName, &item.ActorType, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.SafeErrorCode, &item.CreatedAt); err != nil {
			return operations.AuditPage{}, fmt.Errorf("scan audit event: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return operations.AuditPage{}, fmt.Errorf("iterate audit events: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1]
		nextCursor = encodeTenantCursor(last.CreatedAt, last.ID)
	}
	return operations.AuditPage{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func operationsCursor(cursor string) (*time.Time, *uuid.UUID, error) {
	if cursor == "" {
		return nil, nil, nil
	}
	valueTime, valueID, err := decodeTenantCursor(cursor)
	if err != nil {
		return nil, nil, operations.ErrInvalidCursor
	}
	return &valueTime, &valueID, nil
}
