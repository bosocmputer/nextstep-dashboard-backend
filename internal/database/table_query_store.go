package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tablequery"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TableQueryStore struct{ pool *pgxpool.Pool }

func NewTableQueryStore(pool *pgxpool.Pool) *TableQueryStore { return &TableQueryStore{pool: pool} }

func (store *TableQueryStore) readSnapshot(ctx context.Context, operation func(pgx.Tx) error) error {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `set local statement_timeout = '2s'`); err != nil {
		return err
	}
	if err := operation(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func tableQueryRange(from, to string) (*time.Time, *time.Time) {
	location, _ := time.LoadLocation("Asia/Bangkok")
	var fromValue, toValue *time.Time
	if from != "" {
		parsed, _ := time.ParseInLocation("2006-01-02", from, location)
		value := parsed.UTC()
		fromValue = &value
	}
	if to != "" {
		parsed, _ := time.ParseInLocation("2006-01-02", to, location)
		value := parsed.AddDate(0, 0, 1).UTC()
		toValue = &value
	}
	return fromValue, toValue
}

func stringValues[T ~string](values []T) []string {
	result := make([]string, len(values))
	for index := range values {
		result[index] = string(values[index])
	}
	return result
}

func (store *TableQueryStore) QueryTenants(ctx context.Context, input tablequery.TenantsInput, now time.Time) (items []tenant.Tenant, total int, err error) {
	statuses, readiness := stringValues(input.Filters.Statuses), input.Filters.SMLReadiness
	err = store.readSnapshot(ctx, func(tx pgx.Tx) error {
		where := `t.archived_at is null
		  and (cardinality($2::text[]) = 0 or (case when t.access_ends_at <= $1 then 'EXPIRED' else t.status end) = any($2))
		  and (cardinality($3::text[]) = 0 or coalesce(s.readiness_status, 'UNCONFIGURED') = any($3))
		  and ($4 = '' or t.name ilike '%' || $4 || '%' or t.slug ilike '%' || $4 || '%')`
		if err := tx.QueryRow(ctx, `select count(*) from tenants t left join tenant_sml_connections s on s.tenant_id=t.id where `+where, now, statuses, readiness, input.GlobalSearch).Scan(&total); err != nil {
			return fmt.Errorf("count tenants query: %w", err)
		}
		rows, err := tx.Query(ctx, `select t.id,t.slug,t.name,t.timezone,case when t.access_ends_at <= $1 then 'EXPIRED' else t.status end,t.access_ends_at,t.version,coalesce(s.readiness_status,'UNCONFIGURED'),(select min(ns.next_run_at) from notification_schedules ns where ns.tenant_id=t.id and ns.status='ACTIVE'),t.created_at,t.updated_at from tenants t left join tenant_sml_connections s on s.tenant_id=t.id where `+where+` order by t.updated_at desc,t.id desc offset $5 limit $6`, now, statuses, readiness, input.GlobalSearch, input.Page*input.PageSize, input.PageSize)
		if err != nil {
			return fmt.Errorf("query tenants page: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			item, scanErr := scanTenant(rows)
			if scanErr != nil {
				return scanErr
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func (store *TableQueryStore) QuerySchedules(ctx context.Context, tenantID uuid.UUID, input tablequery.SchedulesInput, now time.Time) (items []schedule.Schedule, total int, err error) {
	statuses := stringValues(input.Filters.Statuses)
	err = store.readSnapshot(ctx, func(tx pgx.Tx) error {
		where := `s.tenant_id=$1 and ($2 or s.status <> 'ARCHIVED') and (cardinality($3::text[])=0 or s.status=any($3)) and ($4='' or strpos(lower(s.name),lower($4))>0)`
		if err := tx.QueryRow(ctx, `select count(*) from notification_schedules s where `+where, tenantID, input.Filters.IncludeArchived, statuses, input.GlobalSearch).Scan(&total); err != nil {
			return fmt.Errorf("count schedules query: %w", err)
		}
		rows, err := tx.Query(ctx, `select `+scheduleColumns+`,
		  (tenant.status <> 'ACTIVE' or tenant.access_ends_at <= $5) as tenant_inactive,
		  not exists(select 1 from tenant_sml_connections connection where connection.tenant_id=s.tenant_id and connection.readiness_status='READY' and connection.last_tested_at is not null) as sml_not_ready,
		  exists(select 1 from notification_schedule_recipients selected left join tenant_memberships membership on membership.tenant_id=selected.tenant_id and membership.recipient_id=selected.recipient_id left join line_recipients recipient on recipient.id=selected.recipient_id where selected.schedule_id=s.id and (membership.status is distinct from 'ACTIVE' or recipient.status is distinct from 'ACTIVE')) as recipient_not_active,
		  exists(select 1 from notification_schedule_recipients selected join notification_schedule_reports scheduled on scheduled.schedule_id=selected.schedule_id left join recipient_report_permissions permission on permission.tenant_id=selected.tenant_id and permission.report_key=scheduled.report_key and permission.recipient_id=selected.recipient_id where selected.schedule_id=s.id and permission.report_key is null) as permission_mismatch
		  from notification_schedules s join tenants tenant on tenant.id=s.tenant_id where `+where+` order by s.updated_at desc,s.id desc offset $6 limit $7`, tenantID, input.Filters.IncludeArchived, statuses, input.GlobalSearch, now, input.Page*input.PageSize, input.PageSize)
		if err != nil {
			return fmt.Errorf("query schedules page: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var tenantInactive, smlNotReady, recipientNotActive, permissionMismatch bool
			item, scanErr := scanSchedule(rows, &tenantInactive, &smlNotReady, &recipientNotActive, &permissionMismatch)
			if scanErr != nil {
				return scanErr
			}
			if tenantInactive {
				item.ReadinessBlockers = append(item.ReadinessBlockers, schedule.BlockerTenantInactive)
			}
			if smlNotReady {
				item.ReadinessBlockers = append(item.ReadinessBlockers, schedule.BlockerSMLNotReady)
			}
			if recipientNotActive {
				item.ReadinessBlockers = append(item.ReadinessBlockers, schedule.BlockerRecipientNotActive)
			}
			if permissionMismatch {
				item.ReadinessBlockers = append(item.ReadinessBlockers, schedule.BlockerRecipientPermissionMismatch)
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func (store *TableQueryStore) QueryReportRuns(ctx context.Context, input tablequery.ReportRunsInput, now time.Time) (items []operations.ReportRun, total int, err error) {
	from, to := tableQueryRange(input.Filters.DateFrom, input.Filters.DateTo)
	statuses, keys, sources := stringValues(input.Filters.Statuses), stringValues(input.Filters.ReportKeys), stringValues(input.Filters.Sources)
	err = store.readSnapshot(ctx, func(tx pgx.Tx) error {
		where := `($1::uuid is null or r.tenant_id=$1) and (cardinality($2::text[])=0 or r.status=any($2)) and (cardinality($3::text[])=0 or r.report_key=any($3)) and (cardinality($4::text[])=0 or r.source=any($4)) and ($5::timestamptz is null or r.created_at >= $5) and ($6::timestamptz is null or r.created_at < $6) and ($7='' or tenant.name ilike '%'||$7||'%' or r.report_key ilike '%'||$7||'%' or coalesce(r.safe_error_code,'') ilike '%'||$7||'%')`
		args := []any{input.Filters.TenantID, statuses, keys, sources, from, to, input.GlobalSearch}
		if err := tx.QueryRow(ctx, `select count(*) from report_runs r join tenants tenant on tenant.id=r.tenant_id where `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count report runs query: %w", err)
		}
		rows, err := tx.Query(ctx, `select r.*,tenant.name,
		  case when r.status in ('CLAIMED','RUNNING') and r.lease_expires_at<$10 then 'STALLED' else 'ACTIVE' end,
		  (select max(retry_at) from(
		    select circuit.open_until retry_at from tenant_sml_circuits circuit where circuit.tenant_id=r.tenant_id and circuit.open_until>$10
		    union all
		    select host_circuit.open_until from tenant_sml_connections connection join sml_host_circuits host_circuit on host_circuit.host_key=connection.endpoint_host_key where connection.tenant_id=r.tenant_id and host_circuit.open_until>$10
		  ) retry_times),
		  case when r.status<>'QUEUED' then null
		    when exists(select 1 from tenant_sml_circuits circuit where circuit.tenant_id=r.tenant_id and circuit.open_until>$10) then 'TENANT_COOLDOWN'
		    when exists(select 1 from tenant_sml_connections connection join sml_host_circuits host_circuit on host_circuit.host_key=connection.endpoint_host_key where connection.tenant_id=r.tenant_id and host_circuit.open_until>$10) then 'HOST_COOLDOWN'
		    when exists(select 1 from report_runs active where active.tenant_id=r.tenant_id and active.status in ('CLAIMED','RUNNING')) then 'TENANT_BUSY'
		    when (select count(*) from report_runs active join tenant_sml_connections active_connection on active_connection.tenant_id=active.tenant_id join tenant_sml_connections candidate_connection on candidate_connection.tenant_id=r.tenant_id where active.status in ('CLAIMED','RUNNING') and active_connection.endpoint_host_key=candidate_connection.endpoint_host_key)>=$11 then 'HOST_BUSY'
		    when r.source<>'SCHEDULE' and ((select count(*) from report_runs active where active.status in ('CLAIMED','RUNNING') and active.source<>'SCHEDULE')>=$12 or (r.report_key in ('stock_balance','ar_customer_movement') and exists(select 1 from notification_schedules scheduled join tenant_sml_connections scheduled_connection on scheduled_connection.tenant_id=scheduled.tenant_id join tenant_sml_connections candidate_connection on candidate_connection.tenant_id=r.tenant_id and candidate_connection.endpoint_host_key=scheduled_connection.endpoint_host_key where scheduled.status='ACTIVE' and scheduled.next_run_at between $10 and $10+interval '15 minutes'))) then 'SCHEDULE_RESERVED'
		    else null end
		  from (select `+reportRunColumns+` from report_runs) r
		  join tenants tenant on tenant.id=r.tenant_id
		  where `+where+` order by r.created_at desc,r.id desc offset $8 limit $9`, append(args, input.Page*input.PageSize, input.PageSize, now, boundedEnvInt("REPORT_HOST_QUERY_CONCURRENCY", 2, 1, 16), max(1, boundedEnvInt("REPORT_GLOBAL_QUERY_CONCURRENCY", 4, 1, 32)-1))...)
		if err != nil {
			return fmt.Errorf("query report runs page: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var item operations.ReportRun
			run, scanErr := scanReportRunWithExtras(rows, now, &item.TenantName, &item.RuntimeStatus, &item.RetryAvailableAt, &item.WaitReason)
			if scanErr != nil {
				return scanErr
			}
			item.Run = run
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func (store *TableQueryStore) QueryDeliveries(ctx context.Context, input tablequery.DeliveriesInput) (items []operations.Delivery, total int, err error) {
	from, to := tableQueryRange(input.Filters.DateFrom, input.Filters.DateTo)
	statuses, reportKeys := stringValues(input.Filters.Statuses), stringValues(input.Filters.ReportKeys)
	err = store.readSnapshot(ctx, func(tx pgx.Tx) error {
		where := `($1::uuid is null or delivery.tenant_id=$1) and ($2::uuid is null or delivery.recipient_id=$2) and (cardinality($3::text[])=0 or delivery.status=any($3)) and ($4::timestamptz is null or delivery.created_at >= $4) and ($5::timestamptz is null or delivery.created_at < $5) and (cardinality($6::text[])=0 or exists(select 1 from notification_run_reports nr where nr.notification_run_id=delivery.notification_run_id and nr.report_key=any($6))) and ($7='' or tenant.name ilike '%'||$7||'%' or exists(select 1 from notification_run_reports nr where nr.notification_run_id=delivery.notification_run_id and nr.report_key ilike '%'||$7||'%'))`
		args := []any{input.Filters.TenantID, input.Filters.RecipientID, statuses, from, to, reportKeys, input.GlobalSearch}
		if err := tx.QueryRow(ctx, `select count(*) from line_deliveries delivery join tenants tenant on tenant.id=delivery.tenant_id where `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count deliveries query: %w", err)
		}
		rows, err := tx.Query(ctx, `select delivery.id,delivery.tenant_id,tenant.name,recipient.id,recipient.line_user_id_hash,recipient.display_name_ciphertext,recipient.display_name_nonce,recipient.encryption_key_id,coalesce(actual.report_keys,'{}'::text[]),delivery.status,delivery.attempt,delivery.safe_error_code,delivery.provider_request_id,delivery.accepted_at,delivery.created_at,delivery.expires_at from line_deliveries delivery join tenants tenant on tenant.id=delivery.tenant_id join line_recipients recipient on recipient.id=delivery.recipient_id left join lateral(select array_agg(nr.report_key order by nr.position nulls last,nr.report_key) report_keys from notification_run_reports nr where nr.notification_run_id=delivery.notification_run_id) actual on true where `+where+` order by delivery.created_at desc,delivery.id desc offset $8 limit $9`, append(args, input.Page*input.PageSize, input.PageSize)...)
		if err != nil {
			return fmt.Errorf("query deliveries page: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			item, scanErr := scanTableDelivery(rows)
			if scanErr != nil {
				return scanErr
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func scanTableDelivery(row interface{ Scan(...any) error }) (operations.Delivery, error) {
	var item operations.Delivery
	var keys []string
	err := row.Scan(&item.ID, &item.TenantID, &item.TenantName, &item.StoredRecipient.ID, &item.StoredRecipient.LineUserIDHash, &item.StoredRecipient.DisplayName.Ciphertext, &item.StoredRecipient.DisplayName.Nonce, &item.StoredRecipient.DisplayName.KeyID, &keys, &item.Status, &item.Attempt, &item.SafeErrorCode, &item.ProviderRequestID, &item.AcceptedAt, &item.CreatedAt, &item.ExpiresAt)
	if err != nil {
		return item, err
	}
	item.StoredRecipient.TenantID = item.TenantID
	for _, key := range keys {
		item.ReportKeys = append(item.ReportKeys, report.Key(key))
	}
	item.ReportCount = len(item.ReportKeys)
	return item, nil
}

func (store *TableQueryStore) QueryAudit(ctx context.Context, input tablequery.AuditInput) (items []operations.AuditEvent, total int, err error) {
	from, to := tableQueryRange(input.Filters.DateFrom, input.Filters.DateTo)
	err = store.readSnapshot(ctx, func(tx pgx.Tx) error {
		where := `($1::uuid is null or audit.tenant_id=$1) and (cardinality($2::text[])=0 or audit.actor_type=any($2)) and (cardinality($3::text[])=0 or audit.action=any($3)) and (cardinality($4::text[])=0 or audit.result=any($4)) and ($5::timestamptz is null or audit.created_at >= $5) and ($6::timestamptz is null or audit.created_at < $6) and ($7='' or coalesce(tenant.name,'') ilike '%'||$7||'%' or audit.action ilike '%'||$7||'%' or audit.resource_type ilike '%'||$7||'%')`
		args := []any{input.Filters.TenantID, input.Filters.ActorTypes, input.Filters.Actions, input.Filters.Results, from, to, input.GlobalSearch}
		if err := tx.QueryRow(ctx, `select count(*) from audit_logs audit left join tenants tenant on tenant.id=audit.tenant_id where `+where, args...).Scan(&total); err != nil {
			return fmt.Errorf("count audit query: %w", err)
		}
		rows, queryErr := tx.Query(ctx, `select audit.id,audit.tenant_id,tenant.name,audit.actor_type,audit.action,audit.resource_type,audit.resource_id,audit.result,audit.safe_error_code,audit.created_at from audit_logs audit left join tenants tenant on tenant.id=audit.tenant_id where `+where+` order by audit.created_at desc,audit.id desc offset $8 limit $9`, append(args, input.Page*input.PageSize, input.PageSize)...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var item operations.AuditEvent
			if scanErr := rows.Scan(&item.ID, &item.TenantID, &item.TenantName, &item.ActorType, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.SafeErrorCode, &item.CreatedAt); scanErr != nil {
				return scanErr
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func (store *TableQueryStore) QueryIncidents(ctx context.Context, input tablequery.IncidentsInput) (items []sentinel.Incident, total int, err error) {
	statuses, severities, roots := stringValues(input.Filters.Statuses), stringValues(input.Filters.Severities), stringValues(input.Filters.RootCauses)
	err = store.readSnapshot(ctx, func(tx pgx.Tx) error {
		where := `(cardinality($1::text[])=0 or incident.status=any($1)) and (cardinality($2::text[])=0 or incident.severity=any($2)) and (cardinality($3::text[])=0 or incident.root_cause=any($3)) and (not $4::boolean or incident.status in ('OPEN','ACKNOWLEDGED')) and ($5='' or incident.alert_ref ilike '%'||$5||'%' or incident.incident_type ilike '%'||$5||'%' or incident.safe_error_code ilike '%'||$5||'%' or exists(select 1 from operational_incident_events event join tenants tenant on tenant.id=event.tenant_id where event.incident_id=incident.id and tenant.name ilike '%'||$5||'%'))`
		args := []any{statuses, severities, roots, input.Filters.ActiveOnly, input.GlobalSearch}
		if err := tx.QueryRow(ctx, `select count(*) from operational_incidents incident where `+where, args...).Scan(&total); err != nil {
			return err
		}
		rows, queryErr := tx.Query(ctx, `select incident.id,incident.alert_ref,incident.incident_type,incident.root_cause,incident.severity,incident.status,coalesce(incident.safe_error_code,''),incident.occurrence_count,incident.affected_count,incident.first_seen_at,incident.last_seen_at,incident.acknowledged_at,incident.resolved_at,incident.accepted_at,coalesce(incident.accepted_reason,''),incident.version,incident.observation_mode,incident.subject_type,incident.active_affected_count,incident.measurement_kind,incident.measurement_value,incident.measurement_threshold,incident.measurement_unit,coalesce((select array_agg(sample.name order by sample.name) from(select distinct tenant.name from operational_incident_events event join tenants tenant on tenant.id=event.tenant_id where event.incident_id=incident.id order by tenant.name limit 2)sample),'{}'::text[]) from operational_incidents incident where `+where+` order by incident.last_seen_at desc,incident.id desc offset $6 limit $7`, append(args, input.Page*input.PageSize, input.PageSize)...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var examples []string
			item, scanErr := scanIncidentV2(rows, &examples)
			if scanErr != nil {
				return scanErr
			}
			item.TenantExamples = examples
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func (store *TableQueryStore) QueryOccurrences(ctx context.Context, incidentID uuid.UUID, input tablequery.OccurrencesInput) (items []sentinel.IncidentOccurrence, total int, err error) {
	from, to := tableQueryRange(input.Filters.DateFrom, input.Filters.DateTo)
	reportKeys, sources := stringValues(input.Filters.ReportKeys), stringValues(input.Filters.SourceKinds)
	err = store.readSnapshot(ctx, func(tx pgx.Tx) error {
		where := `event.incident_id=$1 and event.event_kind in ('OBSERVED','CONDITION_UPDATED') and not event.downstream and ($2::uuid is null or event.tenant_id=$2) and (cardinality($3::text[])=0 or event.report_key=any($3)) and (cardinality($4::text[])=0 or event.source_kind=any($4)) and (cardinality($5::text[])=0 or event.safe_error_code=any($5)) and ($6::timestamptz is null or event.observed_at >= $6) and ($7::timestamptz is null or event.observed_at < $7) and ($8='' or coalesce(tenant.name,'') ilike '%'||$8||'%' or coalesce(event.report_key,'') ilike '%'||$8||'%' or coalesce(event.safe_error_code,'') ilike '%'||$8||'%')`
		args := []any{incidentID, input.Filters.TenantID, reportKeys, sources, input.Filters.SafeErrorCodes, from, to, input.GlobalSearch}
		var exists bool
		if err := tx.QueryRow(ctx, `select (select exists(select 1 from operational_incidents where id=$1)),count(*) from operational_incident_events event left join tenants tenant on tenant.id=event.tenant_id where `+where, args...).Scan(&exists, &total); err != nil {
			return err
		}
		if !exists {
			return sentinel.ErrNotFound
		}
		rows, queryErr := tx.Query(ctx, `select event.id,coalesce(event.tenant_id,'00000000-0000-0000-0000-000000000000'::uuid),coalesce(tenant.name,''),coalesce(event.report_key,''),coalesce(event.source_kind,''),coalesce(event.safe_error_code,''),event.observed_at,event.failure_evidence_version,event.failure_level,event.failure_category,event.failure_stage,event.failure_transport_phase,event.failure_occurred_at,event.failure_duration_ms,event.failure_attempt,event.failure_retryable,event.failure_remote_state_unknown,event.connection_version,event.reports_total,event.reports_succeeded,event.reports_failed,event.reports_cancelled,event.notification_outcome,coalesce(history.after_json->>'endpointUrl',''),coalesce(current.endpoint_url,''),current.version,test.cooldown_until from operational_incident_events event left join tenants tenant on tenant.id=event.tenant_id left join tenant_sml_connections current on current.tenant_id=event.tenant_id left join lateral(select audit.after_json from audit_logs audit where audit.tenant_id=event.tenant_id and audit.action='SML_CONNECTION_REPLACED' and event.connection_version is not null and audit.after_json->>'version'=event.connection_version::text order by audit.created_at desc limit 1)history on true left join sml_connection_tests test on test.tenant_id=event.tenant_id where `+where+` order by event.observed_at desc,event.id desc offset $9 limit $10`, append(args, input.Page*input.PageSize, input.PageSize)...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			item, scanErr := scanTableOccurrence(rows)
			if scanErr != nil {
				return scanErr
			}
			items = append(items, item)
		}
		return rows.Err()
	})
	return
}

func scanTableOccurrence(row interface{ Scan(...any) error }) (sentinel.IncidentOccurrence, error) {
	var item sentinel.IncidentOccurrence
	var evidenceVersion, attempt, connectionVersion, currentVersion *int
	var level, category, stage, transport *string
	var occurredAt *time.Time
	var duration *int64
	var retryable, remoteUnknown *bool
	var total, succeeded, failed, cancelled *int
	var outcome *string
	var historicalURL, currentURL string
	var cooldown *time.Time
	err := row.Scan(&item.ID, &item.TenantID, &item.TenantName, &item.ReportKey, &item.SourceKind, &item.SafeErrorCode, &item.ObservedAt, &evidenceVersion, &level, &category, &stage, &transport, &occurredAt, &duration, &attempt, &retryable, &remoteUnknown, &connectionVersion, &total, &succeeded, &failed, &cancelled, &outcome, &historicalURL, &currentURL, &currentVersion, &cooldown)
	if err != nil {
		return item, err
	}
	if level != nil && category != nil && stage != nil && occurredAt != nil && retryable != nil && remoteUnknown != nil {
		e := failure.Evidence{Level: failure.EvidenceLevel(*level), Category: failure.Category(*category), Stage: failure.Stage(*stage), OccurredAt: *occurredAt, DurationMS: duration, Attempt: attempt, Retryable: *retryable, RemoteStateUnknown: *remoteUnknown, ConnectionVersion: connectionVersion, SafeErrorCode: item.SafeErrorCode}
		if evidenceVersion != nil {
			e.Version = *evidenceVersion
		}
		if transport != nil {
			e.TransportPhase = failure.TransportPhase(*transport)
		}
		item.FailureEvidence = &e
	}
	if total != nil && succeeded != nil && failed != nil && cancelled != nil {
		impact := failure.Impact{ReportsTotal: *total, ReportsSucceeded: *succeeded, ReportsFailed: *failed, ReportsCancelled: *cancelled, Notification: failure.NotificationOutcomeUnknown}
		if outcome != nil {
			impact.Notification = failure.NotificationOutcome(*outcome)
		}
		item.Impact = &impact
	}
	if strings.HasPrefix(strings.ToUpper(item.SafeErrorCode), "SML_") || (category != nil && strings.HasPrefix(*category, "JAVA_WS")) {
		reference := sentinel.SMLConnectionReference{EndpointURLAtFailure: historicalURL, CurrentEndpointURL: currentURL, VersionAtFailure: connectionVersion, CurrentVersion: currentVersion, Status: sentinel.ConnectionUnavailable, TestAvailableAt: cooldown}
		switch {
		case historicalURL != "" && connectionVersion != nil && currentVersion != nil && *connectionVersion == *currentVersion:
			reference.Status = sentinel.ConnectionExactVersion
		case historicalURL != "":
			reference.Status = sentinel.ConnectionChanged
		case currentURL != "":
			reference.Status = sentinel.ConnectionCurrentOnly
		}
		item.ConnectionReference = &reference
	}
	return item, nil
}
