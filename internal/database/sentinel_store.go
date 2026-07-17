package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const monitorBaselineID = "00000000-0000-0000-0000-000000000000"

type SentinelStore struct {
	pool                      *pgxpool.Pool
	conditionMu               sync.Mutex
	databaseConnectionsHighAt *time.Time
}

func NewSentinelStore(pool *pgxpool.Pool) *SentinelStore { return &SentinelStore{pool: pool} }

func (store *SentinelStore) ScanObservations(ctx context.Context, now time.Time, limit int, overlap time.Duration) ([]sentinel.Observation, error) {
	if limit < 1 || limit > 500 || overlap < 0 || overlap > 10*time.Minute {
		return nil, sentinel.ErrInvalidInput
	}
	observations := make([]sentinel.Observation, 0, limit)
	remaining := limit
	for _, source := range []struct {
		key   string
		query string
		scan  func(pgx.Rows) (*sentinel.Observation, error)
	}{
		{key: "notification_terminal", query: `
			select n.id, n.tenant_id, n.trigger_kind, n.status, coalesce(n.safe_error_code, ''), n.updated_at,
			       coalesce(impact.reports_total, 0), coalesce(impact.reports_succeeded, 0),
			       coalesce(impact.reports_failed, 0), coalesce(impact.reports_cancelled, 0),
			       coalesce(root_failure.safe_error_code, ''), coalesce(root_failure.incident_open, false)
			from notification_runs n
			left join lateral (
			  select count(*)::integer reports_total,
			         count(*) filter (where run.status = 'SUCCEEDED')::integer reports_succeeded,
			         count(*) filter (where run.status = 'FAILED')::integer reports_failed,
			         count(*) filter (where run.status = 'CANCELLED')::integer reports_cancelled
			  from notification_run_reports linked join report_runs run on run.id = linked.report_run_id
			  where linked.notification_run_id = n.id
			) impact on true
			left join lateral (
			  select coalesce(failed_run.safe_error_code, '') safe_error_code,
			         failed_run.updated_at,
			         exists (
			           select 1 from operational_incident_events root_event
			           join operational_incidents root_incident on root_incident.id = root_event.incident_id
			           where root_event.source_kind = 'REPORT' and root_event.source_id = failed_run.id
			             and root_event.observed_at = failed_run.updated_at
			             and root_incident.status in ('OPEN', 'ACKNOWLEDGED')
			         ) incident_open
			  from notification_run_reports linked
			  join report_runs failed_run on failed_run.id = linked.report_run_id
			  where linked.notification_run_id = n.id and failed_run.source = 'SCHEDULE'
			    and failed_run.status = 'FAILED'
			    and n.updated_at between failed_run.updated_at and failed_run.updated_at + interval '30 seconds'
			  order by failed_run.updated_at, failed_run.id limit 1
			) root_failure on n.safe_error_code in ('REPORT_SET_INCOMPLETE', 'ALL_REPORTS_FAILED')
			where n.trigger_kind = 'SCHEDULED'
			  and n.status in ('FAILED', 'PARTIAL_FAILED', 'BLOCKED_QUOTA')
			  and not (
			    n.safe_error_code in ('REPORT_SET_INCOMPLETE', 'ALL_REPORTS_FAILED')
			    and root_failure.safe_error_code is not null
			    and root_failure.updated_at between $1 and $2
			    and not root_failure.incident_open
			  )
			  and n.updated_at between $1 and $2
			  and not exists (
			    select 1 from operational_incident_events event
			    where event.source_kind = 'NOTIFICATION' and event.source_id = n.id and event.observed_at = n.updated_at
			  )
			order by n.updated_at, n.id limit $3`, scan: scanNotificationObservation},
		{key: "delivery_terminal", query: `
			select delivery.id, delivery.tenant_id, delivery.status, coalesce(delivery.safe_error_code, ''), delivery.updated_at
			from line_deliveries delivery
			where delivery.status = 'FAILED_PERMANENT'
			  and delivery.updated_at between $1 and $2
			  and not exists (
			    select 1 from operational_incident_events event
			    where event.source_kind = 'DELIVERY' and event.source_id = delivery.id and event.observed_at = delivery.updated_at
			  )
			order by delivery.updated_at, delivery.id limit $3`, scan: scanDeliveryObservation},
		{key: "report_terminal", query: `
			select run.id, run.tenant_id, run.status, coalesce(run.safe_error_code, ''), run.updated_at,
			       run.report_key, run.failure_evidence_version, run.failure_category, run.failure_stage,
			       run.failure_transport_phase, run.failure_occurred_at, run.failure_duration_ms,
			       run.failure_attempt, run.failure_retryable, run.failure_remote_state_unknown,
			       run.data_source_version, linked.notification_run_id,
			       coalesce(notification.trigger_kind, 'SCHEDULED'),
			       coalesce(impact.reports_total, 1),
			       coalesce(impact.reports_succeeded, case when run.status = 'SUCCEEDED' then 1 else 0 end),
			       coalesce(impact.reports_failed, case when run.status = 'FAILED' then 1 else 0 end),
			       coalesce(impact.reports_cancelled, case when run.status = 'CANCELLED' then 1 else 0 end),
			       case when linked.notification_run_id is null then 'NOT_APPLICABLE'
			            when notification.safe_error_code in ('REPORT_SET_INCOMPLETE', 'ALL_REPORTS_FAILED') then 'NOT_CREATED_INCOMPLETE_REPORT_SET'
			            when exists (select 1 from line_deliveries delivery where delivery.notification_run_id = linked.notification_run_id) then 'CREATED'
			            else 'UNKNOWN' end
			from report_runs run
			left join lateral (
			  select materialized.notification_run_id from notification_run_reports materialized
			  where materialized.report_run_id = run.id order by materialized.notification_run_id limit 1
			) linked on true
			left join notification_runs notification on notification.id = linked.notification_run_id
			left join lateral (
			  select count(*)::integer reports_total,
			         count(*) filter (where sibling.status = 'SUCCEEDED')::integer reports_succeeded,
			         count(*) filter (where sibling.status = 'FAILED')::integer reports_failed,
			         count(*) filter (where sibling.status = 'CANCELLED')::integer reports_cancelled
			  from notification_run_reports occurrence_report join report_runs sibling on sibling.id = occurrence_report.report_run_id
			  where occurrence_report.notification_run_id = linked.notification_run_id
			) impact on linked.notification_run_id is not null
			where run.source = 'SCHEDULE' and run.status = 'FAILED'
			  and run.updated_at between $1 and $2
			  and not exists (
			    select 1 from operational_incident_events event
			    where event.source_kind = 'REPORT' and event.source_id = run.id and event.observed_at = run.updated_at
			  )
			order by run.updated_at, run.id limit $3`, scan: scanReportObservation},
	} {
		cursor, initialized, err := store.monitorCursor(ctx, source.key, now)
		if err != nil {
			return nil, err
		}
		if initialized {
			continue
		}
		if remaining == 0 {
			continue
		}
		rows, err := store.pool.Query(ctx, source.query, cursor.Add(-overlap), now, remaining)
		if err != nil {
			return nil, fmt.Errorf("scan %s observations: %w", source.key, err)
		}
		for rows.Next() {
			observation, err := source.scan(rows)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s observation: %w", source.key, err)
			}
			if observation != nil {
				observation.CursorKey = source.key
				observations = append(observations, *observation)
				remaining--
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate %s observations: %w", source.key, err)
		}
		rows.Close()
	}
	conditionObservations, err := store.conditionObservations(ctx, now)
	if err != nil {
		return nil, err
	}
	for _, observation := range conditionObservations {
		if len(observations) == limit {
			break
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func scanNotificationObservation(rows pgx.Rows) (*sentinel.Observation, error) {
	var sourceID, tenantID uuid.UUID
	var trigger sentinel.TriggerKind
	var status, safeCode string
	var observedAt time.Time
	var total, succeeded, failed, cancelled int
	var rootSafeCode string
	var rootIncidentOpen bool
	if err := rows.Scan(&sourceID, &tenantID, &trigger, &status, &safeCode, &observedAt, &total, &succeeded, &failed, &cancelled, &rootSafeCode, &rootIncidentOpen); err != nil {
		return nil, err
	}
	observation := sentinel.NotificationObservation(sourceID, tenantID, trigger, status, safeCode, observedAt)
	if observation != nil {
		evidence := failure.EvidenceForCode(safeCode)
		evidence.OccurredAt = observedAt
		evidence = failure.Complete(evidence)
		observation.Evidence = &evidence
		observation.TriggerKind = trigger
		observation.CorrelationKey = sentinel.OccurrenceCorrelationKey(sourceID)
		observation.Impact = &failure.Impact{ReportsTotal: total, ReportsSucceeded: succeeded, ReportsFailed: failed, ReportsCancelled: cancelled, Notification: failure.NotificationOutcomeUnknown}
		if rootIncidentOpen {
			observation.Downstream = true
			observation.RootCause = rootCauseForOperationalCode(rootSafeCode)
			observation.IncidentType = "SCHEDULED_NOTIFICATION_DOWNSTREAM"
			observation.Impact.Notification = failure.NotificationNotCreatedIncompleteSet
		}
	}
	return observation, nil
}

func scanDeliveryObservation(rows pgx.Rows) (*sentinel.Observation, error) {
	var sourceID, tenantID uuid.UUID
	var status, safeCode string
	var observedAt time.Time
	if err := rows.Scan(&sourceID, &tenantID, &status, &safeCode, &observedAt); err != nil {
		return nil, err
	}
	observation := sentinel.DeliveryObservation(sourceID, tenantID, status, safeCode, observedAt)
	if observation != nil {
		evidence := failure.EvidenceForCode(safeCode)
		evidence.OccurredAt = observedAt
		evidence = failure.Complete(evidence)
		observation.Evidence = &evidence
	}
	return observation, nil
}

func scanReportObservation(rows pgx.Rows) (*sentinel.Observation, error) {
	var sourceID, tenantID uuid.UUID
	var status, safeCode, reportKey string
	var observedAt time.Time
	var evidenceVersion, attempt, connectionVersion *int
	var durationMS *int64
	var category, stage, transportPhase *string
	var evidenceOccurredAt *time.Time
	var retryable, remoteStateUnknown *bool
	var notificationRunID *uuid.UUID
	var trigger sentinel.TriggerKind
	var total, succeeded, failed, cancelled int
	var notificationOutcome failure.NotificationOutcome
	if err := rows.Scan(&sourceID, &tenantID, &status, &safeCode, &observedAt, &reportKey,
		&evidenceVersion, &category, &stage, &transportPhase, &evidenceOccurredAt, &durationMS,
		&attempt, &retryable, &remoteStateUnknown, &connectionVersion, &notificationRunID, &trigger,
		&total, &succeeded, &failed, &cancelled, &notificationOutcome); err != nil {
		return nil, err
	}
	observation := sentinel.ReportObservation(sourceID, tenantID, status, safeCode, observedAt)
	if observation == nil {
		return nil, nil
	}
	evidence := failure.EvidenceForCode(safeCode)
	evidence.Level = failure.LevelLegacyPartial
	evidence.Version = 0
	evidence.OccurredAt = observedAt
	if evidenceVersion != nil && category != nil && stage != nil && evidenceOccurredAt != nil && retryable != nil && remoteStateUnknown != nil {
		evidence.Version = *evidenceVersion
		evidence.Level = failure.LevelConfirmed
		evidence.Category = failure.Category(*category)
		evidence.Stage = failure.Stage(*stage)
		evidence.OccurredAt = *evidenceOccurredAt
		evidence.Retryable = *retryable
		evidence.RemoteStateUnknown = *remoteStateUnknown
		if transportPhase != nil {
			evidence.TransportPhase = failure.TransportPhase(*transportPhase)
		}
	}
	if durationMS != nil {
		evidence.DurationMS = durationMS
	}
	evidence.Attempt = attempt
	evidence.ConnectionVersion = connectionVersion
	evidence = failure.Complete(evidence)
	observation.Evidence = &evidence
	observation.ReportKey = reportKey
	observation.TriggerKind = trigger
	observation.Impact = &failure.Impact{ReportsTotal: total, ReportsSucceeded: succeeded, ReportsFailed: failed, ReportsCancelled: cancelled, Notification: notificationOutcome}
	if notificationRunID != nil {
		observation.CorrelationKey = sentinel.OccurrenceCorrelationKey(*notificationRunID)
	} else {
		observation.CorrelationKey = sentinel.OccurrenceCorrelationKey(sourceID)
	}
	return observation, nil
}

func (store *SentinelStore) monitorCursor(ctx context.Context, key string, now time.Time) (time.Time, bool, error) {
	result, err := store.pool.Exec(ctx, `
		insert into operational_monitor_cursors (monitor_key, cursor_updated_at, cursor_id, initialized_at, updated_at)
		values ($1, $2, $3, $2, $2) on conflict (monitor_key) do nothing`, key, now, monitorBaselineID)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("initialize operational cursor: %w", err)
	}
	if result.RowsAffected() == 1 {
		return now, true, nil
	}
	var cursor time.Time
	if err := store.pool.QueryRow(ctx, `select cursor_updated_at from operational_monitor_cursors where monitor_key = $1`, key).Scan(&cursor); err != nil {
		return time.Time{}, false, fmt.Errorf("read operational cursor: %w", err)
	}
	return cursor, false, nil
}

func (store *SentinelStore) conditionObservations(ctx context.Context, now time.Time) ([]sentinel.Observation, error) {
	observations := make([]sentinel.Observation, 0, 4)
	var queueRunID, queueTenantID uuid.UUID
	err := store.pool.QueryRow(ctx, `
		select id, tenant_id from report_runs
		where source = 'SCHEDULE' and status = 'QUEUED' and queued_at <= $1
		order by queued_at, id limit 1`, now.Add(-120*time.Second)).Scan(&queueRunID, &queueTenantID)
	if err == nil {
		observations = append(observations, sentinel.Observation{
			IncidentType: "SCHEDULE_QUEUE_AGE_EXCEEDED", RootCause: sentinel.RootPlatform, Severity: sentinel.SeverityP1,
			SourceKind: sentinel.SourceReport, SourceID: queueRunID, TenantID: &queueTenantID,
			SafeErrorCode: "SCHEDULE_QUEUE_AGE_EXCEEDED", ObservedAt: now,
		})
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("inspect schedule queue age: %w", err)
	}
	var latestHeartbeat *time.Time
	if err := store.pool.QueryRow(ctx, `select max(heartbeat_at) from worker_heartbeats`).Scan(&latestHeartbeat); err != nil {
		return nil, fmt.Errorf("inspect worker heartbeat: %w", err)
	}
	missingHeartbeat := latestHeartbeat != nil && latestHeartbeat.Before(now.Add(-90*time.Second))
	if latestHeartbeat == nil {
		var monitorStartedAt *time.Time
		if err := store.pool.QueryRow(ctx, `select min(initialized_at) from operational_monitor_cursors`).Scan(&monitorStartedAt); err != nil {
			return nil, fmt.Errorf("inspect monitor initialization: %w", err)
		}
		missingHeartbeat = monitorStartedAt != nil && monitorStartedAt.Before(now.Add(-90*time.Second))
	}
	if missingHeartbeat {
		observations = append(observations, sentinel.Observation{
			IncidentType: "WORKER_HEARTBEAT_MISSING", RootCause: sentinel.RootPlatform, Severity: sentinel.SeverityP1,
			SourceKind: sentinel.SourceWorker, SourceID: stableOperationalID("worker-heartbeat"),
			SafeErrorCode: "WORKER_HEARTBEAT_STALE", ObservedAt: now,
		})
	}
	rows, err := store.pool.Query(ctx, `
		select circuit.tenant_id
		from tenant_sml_circuits circuit
		where circuit.open_until > $1
		  and exists (
		    select 1 from notification_schedules schedule
		    where schedule.tenant_id = circuit.tenant_id and schedule.status = 'ACTIVE'
		      and schedule.next_run_at between $1 and $1 + interval '15 minutes'
		  )
		order by circuit.tenant_id limit 100`, now)
	if err != nil {
		return nil, fmt.Errorf("inspect SML schedule circuits: %w", err)
	}
	for rows.Next() {
		var tenantID uuid.UUID
		if err := rows.Scan(&tenantID); err != nil {
			return nil, fmt.Errorf("scan SML schedule circuit: %w", err)
		}
		observations = append(observations, sentinel.Observation{
			IncidentType: "SML_CIRCUIT_SCHEDULE_AT_RISK", RootCause: sentinel.RootSMLConnectivity, Severity: sentinel.SeverityP1,
			SourceKind: sentinel.SourceSMLCircuit, SourceID: stableOperationalID("tenant-circuit:" + tenantID.String()), TenantID: &tenantID,
			SafeErrorCode: "SML_CIRCUIT_OPEN", ObservedAt: now,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate SML schedule circuits: %w", err)
	}
	rows.Close()

	rows, err = store.pool.Query(ctx, `
		with active as (
		  select run.id, run.tenant_id, run.report_key, run.started_at,
		         case
		           when run.period_from is null or run.period_to is null then 1
		           else greatest(1, run.period_to - run.period_from + 1)
		         end period_days
		  from report_runs run
		  where run.source = 'SCHEDULE' and run.status in ('CLAIMED', 'RUNNING') and run.started_at is not null
		), measured as (
		  select active.*, history.sample_count, history.p90_seconds
		  from active
		  left join lateral (
		    select count(*) sample_count,
		           percentile_disc(0.9) within group (order by sample.duration_seconds) p90_seconds
		    from (
		      select extract(epoch from completed.finished_at - completed.started_at) duration_seconds
		      from report_runs completed
		      where completed.tenant_id = active.tenant_id and completed.report_key = active.report_key
		        and completed.source = 'SCHEDULE' and completed.status = 'SUCCEEDED'
		        and completed.started_at is not null and completed.finished_at is not null
		        and (case
		          when completed.period_from is null or completed.period_to is null then 1
		          else greatest(1, completed.period_to - completed.period_from + 1)
		        end) between greatest(1, active.period_days - 1) and active.period_days + 1
		      order by completed.finished_at desc limit 30
		    ) sample
		  ) history on true
		)
		select id, tenant_id
		from measured
		where extract(epoch from ($1 - started_at)) > greatest(
		  120,
		  case when sample_count >= 5 then p90_seconds * 2 else 0 end,
		  case when sample_count >= 5 then p90_seconds + 60 else 0 end
		)
		order by started_at, id limit 100`, now)
	if err != nil {
		return nil, fmt.Errorf("inspect slow scheduled reports: %w", err)
	}
	for rows.Next() {
		var runID, tenantID uuid.UUID
		if err := rows.Scan(&runID, &tenantID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan slow scheduled report: %w", err)
		}
		observations = append(observations, sentinel.Observation{
			IncidentType: "SCHEDULED_REPORT_SLOW", RootCause: sentinel.RootPlatform, Severity: sentinel.SeverityP1,
			SourceKind: sentinel.SourceReport, SourceID: runID, TenantID: &tenantID,
			SafeErrorCode: "SCHEDULED_REPORT_SLOW", ObservedAt: now,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate slow scheduled reports: %w", err)
	}
	rows.Close()

	rows, err = store.pool.Query(ctx, `
		select tenant_id, report_key, coalesce(safe_error_code, 'REPORT_FAILED'), min(id::text)::uuid, count(*)
		from report_runs
		where source = 'DASHBOARD' and status = 'FAILED' and updated_at >= $1::timestamptz - interval '10 minutes'
		group by tenant_id, report_key, coalesce(safe_error_code, 'REPORT_FAILED')
		having count(*) >= 3
		order by tenant_id, report_key limit 100`, now)
	if err != nil {
		return nil, fmt.Errorf("inspect repeated viewer report failures: %w", err)
	}
	for rows.Next() {
		var tenantID, firstRunID uuid.UUID
		var reportKey, safeCode string
		var count int
		if err := rows.Scan(&tenantID, &reportKey, &safeCode, &firstRunID, &count); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan repeated viewer report failure: %w", err)
		}
		observations = append(observations, sentinel.Observation{
			IncidentType: "VIEWER_REPORT_FAILURE_REPEATED", RootCause: rootCauseForOperationalCode(safeCode), Severity: sentinel.SeverityP2,
			SourceKind: sentinel.SourceReport, SourceID: stableOperationalID("viewer-failure:" + tenantID.String() + ":" + reportKey + ":" + safeCode), TenantID: &tenantID,
			SafeErrorCode: safeCode, ObservedAt: now,
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate repeated viewer report failures: %w", err)
	}
	rows.Close()

	var connectionUsage float64
	if err := store.pool.QueryRow(ctx, `
		select count(*)::double precision / greatest(1, current_setting('max_connections')::integer)
		from pg_stat_activity`).Scan(&connectionUsage); err != nil {
		return nil, fmt.Errorf("inspect PostgreSQL connection usage: %w", err)
	}
	store.conditionMu.Lock()
	if connectionUsage >= 0.95 {
		if store.databaseConnectionsHighAt == nil {
			startedAt := now
			store.databaseConnectionsHighAt = &startedAt
		}
		if now.Sub(*store.databaseConnectionsHighAt) >= 5*time.Minute {
			observations = append(observations, sentinel.Observation{
				IncidentType: "DATABASE_CONNECTIONS_CRITICAL", RootCause: sentinel.RootCapacity, Severity: sentinel.SeverityP1,
				SourceKind: sentinel.SourceDatabase, SourceID: stableOperationalID("database-connections"),
				SafeErrorCode: "DATABASE_CONNECTIONS_CRITICAL", ObservedAt: now,
			})
		}
	} else {
		store.databaseConnectionsHighAt = nil
	}
	store.conditionMu.Unlock()
	return observations, nil
}

func rootCauseForOperationalCode(safeCode string) sentinel.RootCause {
	upper := strings.ToUpper(safeCode)
	if strings.HasPrefix(upper, "SML_") || strings.Contains(upper, "NETWORK") || strings.Contains(upper, "TIMEOUT") {
		return sentinel.RootSMLConnectivity
	}
	if strings.Contains(upper, "REPORT") || strings.Contains(upper, "OUTPUT") || strings.Contains(upper, "FLEX") {
		return sentinel.RootReportData
	}
	return sentinel.RootPlatform
}

func stableOperationalID(value string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("nextstep-sentinel:"+value))
}

type incidentGroup struct {
	observation sentinel.Observation
	alertRef    string
	count       int
	first       time.Time
	last        time.Time
}

func (store *SentinelStore) RecordObservations(ctx context.Context, observations []sentinel.Observation, now time.Time, aggregationWindow time.Duration, enqueue bool) error {
	if len(observations) == 0 {
		return nil
	}
	groups := make(map[string]*incidentGroup)
	for _, observation := range observations {
		// Downstream observations enrich the root incident timeline but must not
		// create an incident or an alert by themselves. The SQL scanner only marks
		// them downstream after proving that the root incident remains open.
		if observation.Downstream {
			continue
		}
		fingerprint := observation.Fingerprint()
		group := groups[fingerprint]
		if group == nil {
			alertRef, err := sentinel.NewAlertReference()
			if err != nil {
				return err
			}
			groups[fingerprint] = &incidentGroup{observation: observation, alertRef: alertRef, count: 1, first: observation.ObservedAt, last: observation.ObservedAt}
			continue
		}
		group.count++
		if group.observation.IncidentType != observation.IncidentType {
			group.observation.IncidentType = "AGGREGATED_" + string(group.observation.RootCause)
		}
		if group.observation.SafeErrorCode != observation.SafeErrorCode {
			group.observation.SafeErrorCode = "MULTIPLE_SAFE_ERRORS"
		}
		if observation.ObservedAt.Before(group.first) {
			group.first = observation.ObservedAt
		}
		if observation.ObservedAt.After(group.last) {
			group.last = observation.ObservedAt
		}
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operational observation batch: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fingerprints := make([]string, 0, len(groups))
	alertRefs := make([]string, 0, len(groups))
	incidentTypes := make([]string, 0, len(groups))
	rootCauses := make([]string, 0, len(groups))
	severities := make([]string, 0, len(groups))
	safeCodes := make([]string, 0, len(groups))
	counts := make([]int32, 0, len(groups))
	firstSeen := make([]time.Time, 0, len(groups))
	lastSeen := make([]time.Time, 0, len(groups))
	aggregationUntil := make([]time.Time, 0, len(groups))
	reminderDue := make([]time.Time, 0, len(groups))
	for fingerprint, group := range groups {
		fingerprints = append(fingerprints, fingerprint)
		alertRefs = append(alertRefs, group.alertRef)
		incidentTypes = append(incidentTypes, group.observation.IncidentType)
		rootCauses = append(rootCauses, string(group.observation.RootCause))
		severities = append(severities, string(group.observation.Severity))
		safeCodes = append(safeCodes, group.observation.SafeErrorCode)
		counts = append(counts, int32(group.count))
		firstSeen = append(firstSeen, group.first)
		lastSeen = append(lastSeen, group.last)
		aggregationUntil = append(aggregationUntil, group.first.Add(aggregationWindow))
		reminderDue = append(reminderDue, group.first.Add(time.Hour))
	}
	if _, err := tx.Exec(ctx, `
		insert into operational_incidents (
		  alert_ref, fingerprint, incident_type, root_cause, severity, safe_error_code,
		  occurrence_count, affected_count, first_seen_at, last_seen_at, aggregation_until,
		  reminder_due_at, created_at, updated_at
		)
		select input.alert_ref, input.fingerprint, input.incident_type, input.root_cause, input.severity,
		       nullif(input.safe_error_code, ''), input.occurrence_count, 1, input.first_seen_at,
		       input.last_seen_at, input.aggregation_until, input.reminder_due_at, $12, $12
		from unnest(
		  $1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[],
		  $7::integer[], $8::timestamptz[], $9::timestamptz[], $10::timestamptz[], $11::timestamptz[]
		) as input(alert_ref, fingerprint, incident_type, root_cause, severity, safe_error_code,
		           occurrence_count, first_seen_at, last_seen_at, aggregation_until, reminder_due_at)
		on conflict (fingerprint) where status in ('OPEN', 'ACKNOWLEDGED') do update
		set last_seen_at = greatest(operational_incidents.last_seen_at, excluded.last_seen_at),
		    occurrence_count = operational_incidents.occurrence_count + excluded.occurrence_count,
		    incident_type = case
		      when operational_incidents.incident_type = excluded.incident_type then operational_incidents.incident_type
		      else 'AGGREGATED_' || operational_incidents.root_cause
		    end,
		    safe_error_code = case
		      when operational_incidents.safe_error_code is not distinct from excluded.safe_error_code then operational_incidents.safe_error_code
		      else 'MULTIPLE_SAFE_ERRORS'
		    end,
		    updated_at = excluded.updated_at,
		    version = operational_incidents.version + 1`,
		alertRefs, fingerprints, incidentTypes, rootCauses, severities, safeCodes, counts,
		firstSeen, lastSeen, aggregationUntil, reminderDue, now); err != nil {
		return fmt.Errorf("upsert operational incidents batch: %w", err)
	}

	type persistedIncidentEvent struct {
		Fingerprint            string     `json:"fingerprint"`
		EventKind              string     `json:"event_kind"`
		SourceKind             string     `json:"source_kind"`
		SourceID               uuid.UUID  `json:"source_id"`
		TenantID               string     `json:"tenant_id"`
		SafeErrorCode          string     `json:"safe_error_code"`
		ObservedAt             time.Time  `json:"observed_at"`
		CorrelationKey         string     `json:"correlation_key"`
		Downstream             bool       `json:"downstream"`
		EvidenceVersion        *int       `json:"failure_evidence_version"`
		EvidenceLevel          string     `json:"failure_level"`
		EvidenceCategory       string     `json:"failure_category"`
		EvidenceStage          string     `json:"failure_stage"`
		EvidenceTransportPhase string     `json:"failure_transport_phase"`
		EvidenceOccurredAt     *time.Time `json:"failure_occurred_at"`
		EvidenceDurationMS     *int64     `json:"failure_duration_ms"`
		EvidenceAttempt        *int       `json:"failure_attempt"`
		EvidenceRetryable      *bool      `json:"failure_retryable"`
		RemoteStateUnknown     *bool      `json:"failure_remote_state_unknown"`
		ConnectionVersion      *int       `json:"connection_version"`
		ReportKey              string     `json:"report_key"`
		TriggerKind            string     `json:"trigger_kind"`
		ReportsTotal           *int       `json:"reports_total"`
		ReportsSucceeded       *int       `json:"reports_succeeded"`
		ReportsFailed          *int       `json:"reports_failed"`
		ReportsCancelled       *int       `json:"reports_cancelled"`
		NotificationOutcome    string     `json:"notification_outcome"`
	}
	events := make([]persistedIncidentEvent, 0, len(observations))
	for _, observation := range observations {
		tenantID := ""
		if observation.TenantID != nil {
			tenantID = observation.TenantID.String()
		}
		event := persistedIncidentEvent{
			Fingerprint: observation.Fingerprint(), EventKind: "OBSERVED", SourceKind: string(observation.SourceKind),
			SourceID: observation.SourceID, TenantID: tenantID, SafeErrorCode: observation.SafeErrorCode,
			ObservedAt: observation.ObservedAt, CorrelationKey: observation.CorrelationKey,
			Downstream: observation.Downstream, ReportKey: observation.ReportKey, TriggerKind: string(observation.TriggerKind),
		}
		if observation.Downstream {
			event.EventKind = "DOWNSTREAM_IMPACT"
		}
		if evidence := observation.Evidence; evidence != nil {
			if evidence.Version > 0 {
				version := evidence.Version
				event.EvidenceVersion = &version
			}
			event.EvidenceLevel = string(evidence.Level)
			event.EvidenceCategory = string(evidence.Category)
			event.EvidenceStage = string(evidence.Stage)
			event.EvidenceTransportPhase = string(evidence.TransportPhase)
			event.EvidenceOccurredAt = &evidence.OccurredAt
			event.EvidenceDurationMS = evidence.DurationMS
			event.EvidenceAttempt = evidence.Attempt
			retryable, remoteUnknown := evidence.Retryable, evidence.RemoteStateUnknown
			event.EvidenceRetryable = &retryable
			event.RemoteStateUnknown = &remoteUnknown
			event.ConnectionVersion = evidence.ConnectionVersion
		}
		if impact := observation.Impact; impact != nil {
			total, succeeded, failed, cancelled := impact.ReportsTotal, impact.ReportsSucceeded, impact.ReportsFailed, impact.ReportsCancelled
			event.ReportsTotal, event.ReportsSucceeded, event.ReportsFailed, event.ReportsCancelled = &total, &succeeded, &failed, &cancelled
			event.NotificationOutcome = string(impact.Notification)
		}
		events = append(events, event)
	}
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("encode operational incident evidence: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into operational_incident_events (
		  incident_id, event_kind, source_kind, source_id, tenant_id, safe_error_code, observed_at,
		  correlation_key, downstream, failure_evidence_version, failure_level, failure_category,
		  failure_stage, failure_transport_phase, failure_occurred_at, failure_duration_ms,
		  failure_attempt, failure_retryable, failure_remote_state_unknown, connection_version,
		  report_key, trigger_kind, reports_total, reports_succeeded, reports_failed,
		  reports_cancelled, notification_outcome
		)
		select incident.id, input.event_kind, input.source_kind, input.source_id,
		       nullif(input.tenant_id, '')::uuid, nullif(input.safe_error_code, ''), input.observed_at,
		       nullif(input.correlation_key, ''), input.downstream, input.failure_evidence_version,
		       nullif(input.failure_level, ''), nullif(input.failure_category, ''), nullif(input.failure_stage, ''),
		       nullif(input.failure_transport_phase, ''), input.failure_occurred_at, input.failure_duration_ms,
		       input.failure_attempt, input.failure_retryable, input.failure_remote_state_unknown,
		       input.connection_version, nullif(input.report_key, ''), nullif(input.trigger_kind, ''),
		       input.reports_total, input.reports_succeeded, input.reports_failed, input.reports_cancelled,
		       nullif(input.notification_outcome, '')
		from jsonb_to_recordset($1::jsonb) as input(
		  fingerprint text, event_kind text, source_kind text, source_id uuid, tenant_id text,
		  safe_error_code text, observed_at timestamptz, correlation_key text, downstream boolean,
		  failure_evidence_version integer, failure_level text, failure_category text, failure_stage text,
		  failure_transport_phase text, failure_occurred_at timestamptz, failure_duration_ms bigint,
		  failure_attempt integer, failure_retryable boolean, failure_remote_state_unknown boolean,
		  connection_version integer, report_key text, trigger_kind text, reports_total integer,
		  reports_succeeded integer, reports_failed integer, reports_cancelled integer, notification_outcome text
		)
		join operational_incidents incident on incident.fingerprint = input.fingerprint
		  and incident.status in ('OPEN', 'ACKNOWLEDGED')
		on conflict do nothing`, eventsJSON); err != nil {
		return fmt.Errorf("insert operational incident events batch: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update operational_incidents incident
		set occurrence_count = counts.occurrence_count,
		    affected_count = counts.affected_count,
		    first_seen_at = counts.first_seen_at,
		    last_seen_at = counts.last_seen_at
		from (
		  select event.incident_id, count(*)::integer occurrence_count,
		         count(distinct coalesce(event.tenant_id::text, event.source_kind || ':' || coalesce(event.source_id::text, 'platform')))::integer affected_count,
		         min(event.observed_at) first_seen_at, max(event.observed_at) last_seen_at
		  from operational_incident_events event join operational_incidents target on target.id = event.incident_id
		  where event.event_kind = 'OBSERVED' and target.fingerprint = any($1::text[])
		  group by event.incident_id
		) counts
		where incident.id = counts.incident_id and incident.status in ('OPEN', 'ACKNOWLEDGED')`, fingerprints); err != nil {
		return fmt.Errorf("reconcile operational incident counts batch: %w", err)
	}
	if enqueue {
		if _, err := tx.Exec(ctx, `
			insert into operational_alert_outbox (incident_id, alert_kind, available_at, created_at, updated_at)
			select incident.id, 'OPEN', incident.aggregation_until, $1, $1
			from operational_incidents incident
			where incident.fingerprint = any($2::text[]) and incident.status = 'OPEN' and incident.severity = 'P1'
			on conflict (incident_id, alert_kind) do nothing`, now, fingerprints); err != nil {
			return fmt.Errorf("enqueue operational alerts: %w", err)
		}
	}
	type cursorAdvance struct {
		observedAt time.Time
		sourceID   uuid.UUID
	}
	cursorAdvances := make(map[string]cursorAdvance)
	for _, observation := range observations {
		if observation.CursorKey == "" {
			continue
		}
		current, exists := cursorAdvances[observation.CursorKey]
		if !exists || observation.ObservedAt.After(current.observedAt) || (observation.ObservedAt.Equal(current.observedAt) && observation.SourceID.String() > current.sourceID.String()) {
			cursorAdvances[observation.CursorKey] = cursorAdvance{observedAt: observation.ObservedAt, sourceID: observation.SourceID}
		}
	}
	for key, advance := range cursorAdvances {
		if _, err := tx.Exec(ctx, `
			update operational_monitor_cursors
			set cursor_updated_at = greatest(cursor_updated_at, $2), cursor_id = $3, updated_at = $4
			where monitor_key = $1`, key, advance.observedAt, advance.sourceID, now); err != nil {
			return fmt.Errorf("advance operational cursor: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit operational observation batch: %w", err)
	}
	return nil
}

// AdvanceObservationCursors bounds quiet scans without skipping a source whose
// batch was truncated. It is called only after RecordObservations succeeds;
// ScanObservations always applies the same five-minute overlap.
func (store *SentinelStore) AdvanceObservationCursors(ctx context.Context, scannedThrough time.Time) error {
	_, err := store.pool.Exec(ctx, `
		update operational_monitor_cursors cursor
		set cursor_updated_at = greatest(cursor.cursor_updated_at, $1), updated_at = $1
		where
		  (cursor.monitor_key = 'notification_terminal' and not exists (
		    select 1 from notification_runs run
		    left join lateral (
		      select failed_run.updated_at,
		             exists (
		               select 1 from operational_incident_events root_event
		               join operational_incidents root_incident on root_incident.id = root_event.incident_id
		               where root_event.source_kind = 'REPORT' and root_event.source_id = failed_run.id
		                 and root_event.observed_at = failed_run.updated_at
		                 and root_incident.status in ('OPEN', 'ACKNOWLEDGED')
		             ) incident_open
		      from notification_run_reports linked
		      join report_runs failed_run on failed_run.id = linked.report_run_id
		      where linked.notification_run_id = run.id and failed_run.source = 'SCHEDULE'
		        and failed_run.status = 'FAILED'
		        and run.updated_at between failed_run.updated_at and failed_run.updated_at + interval '30 seconds'
		      order by failed_run.updated_at, failed_run.id limit 1
		    ) root_failure on run.safe_error_code in ('REPORT_SET_INCOMPLETE', 'ALL_REPORTS_FAILED')
		    where run.trigger_kind = 'SCHEDULED' and run.status in ('FAILED', 'PARTIAL_FAILED', 'BLOCKED_QUOTA')
		      and run.updated_at between cursor.cursor_updated_at - interval '5 minutes' and $1
		      and not (
		        run.safe_error_code in ('REPORT_SET_INCOMPLETE', 'ALL_REPORTS_FAILED')
		        and root_failure.updated_at is not null
		        and root_failure.updated_at between cursor.cursor_updated_at - interval '5 minutes' and $1
		        and not coalesce(root_failure.incident_open, false)
		      )
		      and not exists (select 1 from operational_incident_events event
		        where event.source_kind = 'NOTIFICATION' and event.source_id = run.id and event.observed_at = run.updated_at)
		  ))
		  or (cursor.monitor_key = 'delivery_terminal' and not exists (
		    select 1 from line_deliveries delivery
		    where delivery.status = 'FAILED_PERMANENT'
		      and delivery.updated_at between cursor.cursor_updated_at - interval '5 minutes' and $1
		      and not exists (select 1 from operational_incident_events event
		        where event.source_kind = 'DELIVERY' and event.source_id = delivery.id and event.observed_at = delivery.updated_at)
		  ))
		  or (cursor.monitor_key = 'report_terminal' and not exists (
		    select 1 from report_runs run
		    where run.source = 'SCHEDULE' and run.status = 'FAILED'
		      and run.updated_at between cursor.cursor_updated_at - interval '5 minutes' and $1
		      and not exists (select 1 from operational_incident_events event
		        where event.source_kind = 'REPORT' and event.source_id = run.id and event.observed_at = run.updated_at)
		  ))`, scannedThrough)
	if err != nil {
		return fmt.Errorf("advance operational scan cursors: %w", err)
	}
	return nil
}

func (store *SentinelStore) MaintenanceActive(ctx context.Context, now time.Time) (bool, error) {
	var active bool
	if err := store.pool.QueryRow(ctx, `
		select exists(select 1 from operational_maintenance_windows
		where status = 'ACTIVE' and starts_at <= $1 and ends_at > $1)`, now).Scan(&active); err != nil {
		return false, fmt.Errorf("check operational maintenance: %w", err)
	}
	return active, nil
}

func (store *SentinelStore) AdvanceLifecycle(ctx context.Context, activeFingerprints []string, now time.Time, enqueue bool) error {
	if activeFingerprints == nil {
		activeFingerprints = []string{}
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operational incident lifecycle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		update operational_incidents incident
		set status = 'RESOLVED', resolved_at = $2, reminder_due_at = null, version = version + 1, updated_at = $2
		where incident.status in ('OPEN', 'ACKNOWLEDGED')
		  and not (incident.fingerprint = any($1::text[]))
		  and (
		    not exists (
		      select 1 from operational_incident_events terminal
		      where terminal.incident_id = incident.id and terminal.event_kind = 'OBSERVED'
		        and terminal.source_kind in ('NOTIFICATION', 'DELIVERY', 'REPORT')
		    )
		    or not exists (
		      select 1 from operational_incident_events affected
		      where affected.incident_id = incident.id and affected.event_kind = 'OBSERVED'
		        and affected.source_kind in ('NOTIFICATION', 'DELIVERY', 'REPORT') and affected.tenant_id is not null
		        and not exists (
		          select 1 from notification_runs recovered
		          where recovered.tenant_id = affected.tenant_id and recovered.trigger_kind = 'SCHEDULED'
		            and recovered.status = 'COMPLETED' and recovered.updated_at > incident.last_seen_at
		        )
		    )
		  )
		returning incident.id, incident.severity`, activeFingerprints, now)
	if err != nil {
		return fmt.Errorf("resolve operational incidents from evidence: %w", err)
	}
	type resolvedIncident struct {
		id       uuid.UUID
		severity string
	}
	resolved := make([]resolvedIncident, 0)
	for rows.Next() {
		var item resolvedIncident
		if err := rows.Scan(&item.id, &item.severity); err != nil {
			rows.Close()
			return err
		}
		resolved = append(resolved, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, item := range resolved {
		if _, err := tx.Exec(ctx, `insert into operational_incident_events (incident_id, event_kind, observed_at) values ($1, 'EVIDENCE_RESOLVED', $2)`, item.id, now); err != nil {
			return fmt.Errorf("record operational recovery evidence: %w", err)
		}
		if enqueue && item.severity == "P1" {
			if _, err := tx.Exec(ctx, `
				insert into operational_alert_outbox (incident_id, alert_kind, available_at, created_at, updated_at)
				select $1, 'RECOVERY', $2, $2, $2
				where exists (select 1 from operational_alert_outbox where incident_id = $1 and alert_kind = 'OPEN' and status = 'SENT')
				on conflict (incident_id, alert_kind) do nothing`, item.id, now); err != nil {
				return fmt.Errorf("enqueue operational recovery alert: %w", err)
			}
		}
	}
	if enqueue {
		if _, err := tx.Exec(ctx, `
			insert into operational_alert_outbox (incident_id, alert_kind, available_at, created_at, updated_at)
			select incident.id, 'REMINDER', $1, $1, $1
			from operational_incidents incident
			where incident.status = 'OPEN' and incident.severity = 'P1' and incident.reminder_due_at <= $1
			  and exists (select 1 from operational_alert_outbox sent where sent.incident_id = incident.id and sent.alert_kind = 'OPEN' and sent.status = 'SENT')
			on conflict (incident_id, alert_kind) do nothing`, now); err != nil {
			return fmt.Errorf("enqueue operational reminder: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit operational incident lifecycle: %w", err)
	}
	return nil
}

func (store *SentinelStore) ListIncidents(ctx context.Context, filter sentinel.IncidentFilter) (sentinel.IncidentPage, error) {
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return sentinel.IncidentPage{}, sentinel.ErrInvalidInput
	}
	var status, severity *string
	if filter.Status != nil {
		value := string(*filter.Status)
		status = &value
	}
	if filter.Severity != nil {
		value := string(*filter.Severity)
		severity = &value
	}
	rows, err := store.pool.Query(ctx, `
		select incident.id, incident.alert_ref, incident.incident_type, incident.root_cause,
		       incident.severity, incident.status, coalesce(incident.safe_error_code, ''),
		       incident.occurrence_count, incident.affected_count, incident.first_seen_at,
		       incident.last_seen_at, incident.acknowledged_at, incident.resolved_at,
		       incident.accepted_at, coalesce(incident.accepted_reason, ''), incident.version,
		       coalesce((
		         select array_agg(sample.name order by sample.name)
		         from (
		           select distinct tenant.name
		           from operational_incident_events event join tenants tenant on tenant.id = event.tenant_id
		           where event.incident_id = incident.id order by tenant.name limit 2
		         ) sample
		       ), '{}'::text[])
		from operational_incidents incident
		where ($1::text is null or incident.status = $1) and ($2::text is null or incident.severity = $2)
		  and ($3::timestamptz is null or (incident.last_seen_at, incident.id) < ($3, $4))
		order by incident.last_seen_at desc, incident.id desc limit $5`, status, severity, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return sentinel.IncidentPage{}, fmt.Errorf("list operational incidents: %w", err)
	}
	defer rows.Close()
	items := make([]sentinel.Incident, 0, filter.PageSize+1)
	for rows.Next() {
		var tenantExamples []string
		item, err := scanIncident(rows, &tenantExamples)
		if err != nil {
			return sentinel.IncidentPage{}, fmt.Errorf("scan operational incident: %w", err)
		}
		item.TenantExamples = tenantExamples
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return sentinel.IncidentPage{}, fmt.Errorf("iterate operational incidents: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	next := ""
	if hasMore {
		next = encodeTenantCursor(items[len(items)-1].LastSeenAt, items[len(items)-1].ID)
	}
	return sentinel.IncidentPage{Data: items, NextCursor: next, HasMore: hasMore}, nil
}

func scanIncident(row interface{ Scan(...any) error }, extraDestinations ...any) (sentinel.Incident, error) {
	var item sentinel.Incident
	destinations := []any{&item.ID, &item.AlertRef, &item.IncidentType, &item.RootCause, &item.Severity, &item.Status, &item.SafeErrorCode,
		&item.OccurrenceCount, &item.AffectedCount, &item.FirstSeenAt, &item.LastSeenAt, &item.AcknowledgedAt, &item.ResolvedAt,
		&item.AcceptedAt, &item.AcceptedReason, &item.Version}
	destinations = append(destinations, extraDestinations...)
	err := row.Scan(destinations...)
	return item, err
}

func (store *SentinelStore) GetIncident(ctx context.Context, id uuid.UUID) (sentinel.IncidentDetail, error) {
	item, err := scanIncident(store.pool.QueryRow(ctx, `
		select id, alert_ref, incident_type, root_cause, severity, status, coalesce(safe_error_code, ''),
		       occurrence_count, affected_count, first_seen_at, last_seen_at, acknowledged_at, resolved_at,
		       accepted_at, coalesce(accepted_reason, ''), version
		from operational_incidents where id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return sentinel.IncidentDetail{}, sentinel.ErrNotFound
	}
	if err != nil {
		return sentinel.IncidentDetail{}, fmt.Errorf("get operational incident: %w", err)
	}
	rows, err := store.pool.Query(ctx, `
		select event.id, event.event_kind, coalesce(event.source_kind, ''), coalesce(event.safe_error_code, ''),
		       coalesce(tenant.name, ''), event.observed_at,
		       event.failure_evidence_version, event.failure_level, event.failure_category, event.failure_stage,
		       event.failure_transport_phase, event.failure_occurred_at, event.failure_duration_ms,
		       event.failure_attempt, event.failure_retryable, event.failure_remote_state_unknown,
		       event.connection_version, coalesce(event.report_key, ''), coalesce(event.trigger_kind, ''),
		       event.reports_total, event.reports_succeeded, event.reports_failed, event.reports_cancelled,
		       event.notification_outcome, event.downstream, coalesce(cause.alert_ref, ''),
		       case when event.connection_version is null then false
		            when connection.version is null then true
		            else event.connection_version <> connection.version end
		from operational_incident_events event
		left join tenants tenant on tenant.id = event.tenant_id
		left join tenant_sml_connections connection on connection.tenant_id = event.tenant_id
		left join operational_incidents cause on cause.id = event.caused_by_incident_id
		where event.incident_id = $1 order by event.observed_at desc, event.id desc limit 200`, id)
	if err != nil {
		return sentinel.IncidentDetail{}, fmt.Errorf("list operational incident events: %w", err)
	}
	defer rows.Close()
	detail := sentinel.IncidentDetail{Incident: item, Events: make([]sentinel.IncidentEvent, 0)}
	for rows.Next() {
		var event sentinel.IncidentEvent
		var evidenceVersion *int
		var evidenceLevel, category, stage, transportPhase *string
		var evidenceOccurredAt *time.Time
		var durationMS *int64
		var attempt, connectionVersion *int
		var retryable, remoteStateUnknown *bool
		var reportsTotal, reportsSucceeded, reportsFailed, reportsCancelled *int
		var notificationOutcome *string
		if err := rows.Scan(&event.ID, &event.EventKind, &event.SourceKind, &event.SafeErrorCode, &event.TenantName, &event.ObservedAt,
			&evidenceVersion, &evidenceLevel, &category, &stage, &transportPhase, &evidenceOccurredAt,
			&durationMS, &attempt, &retryable, &remoteStateUnknown, &connectionVersion,
			&event.ReportKey, &event.TriggerKind, &reportsTotal, &reportsSucceeded, &reportsFailed,
			&reportsCancelled, &notificationOutcome, &event.IsDownstream, &event.CausedByAlertRef,
			&event.ConnectionChangedSinceFailure); err != nil {
			return sentinel.IncidentDetail{}, fmt.Errorf("scan operational incident event: %w", err)
		}
		if evidenceLevel != nil && category != nil && stage != nil && evidenceOccurredAt != nil && retryable != nil && remoteStateUnknown != nil {
			evidence := failure.Evidence{
				Level: failure.EvidenceLevel(*evidenceLevel), Category: failure.Category(*category), Stage: failure.Stage(*stage),
				OccurredAt: *evidenceOccurredAt, DurationMS: durationMS, Attempt: attempt,
				Retryable: *retryable, RemoteStateUnknown: *remoteStateUnknown,
				ConnectionVersion: connectionVersion, SafeErrorCode: event.SafeErrorCode,
			}
			if evidenceVersion != nil {
				evidence.Version = *evidenceVersion
			}
			if transportPhase != nil {
				evidence.TransportPhase = failure.TransportPhase(*transportPhase)
			}
			evidence = failure.Complete(evidence)
			event.FailureEvidence = &evidence
		}
		if reportsTotal != nil && reportsSucceeded != nil && reportsFailed != nil && reportsCancelled != nil {
			impact := failure.Impact{
				ReportsTotal: *reportsTotal, ReportsSucceeded: *reportsSucceeded,
				ReportsFailed: *reportsFailed, ReportsCancelled: *reportsCancelled,
				Notification: failure.NotificationOutcomeUnknown,
			}
			if notificationOutcome != nil {
				impact.Notification = failure.NotificationOutcome(*notificationOutcome)
			}
			event.Impact = &impact
		}
		detail.Events = append(detail.Events, event)
	}
	return detail, rows.Err()
}

func (store *SentinelStore) AcknowledgeIncident(ctx context.Context, id uuid.UUID, version int, now time.Time) (sentinel.Incident, error) {
	return store.mutateIncident(ctx, id, version, now, "ACKNOWLEDGED", "ACKNOWLEDGED", "")
}

func (store *SentinelStore) AcceptIncidentRisk(ctx context.Context, id uuid.UUID, version int, reason string, now time.Time) (sentinel.Incident, error) {
	return store.mutateIncident(ctx, id, version, now, "CLOSED_ACCEPTED", "RISK_ACCEPTED", reason)
}

func (store *SentinelStore) mutateIncident(ctx context.Context, id uuid.UUID, version int, now time.Time, status, eventKind, reason string) (sentinel.Incident, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return sentinel.Incident{}, fmt.Errorf("begin operational incident mutation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var result pgx.Row
	if status == "ACKNOWLEDGED" {
		result = tx.QueryRow(ctx, `
			update operational_incidents set status = 'ACKNOWLEDGED', acknowledged_at = $3, reminder_due_at = null,
			version = version + 1, updated_at = $3
			where id = $1 and version = $2 and status = 'OPEN'
			returning id, alert_ref, incident_type, root_cause, severity, status, coalesce(safe_error_code, ''), occurrence_count,
			affected_count, first_seen_at, last_seen_at, acknowledged_at, resolved_at, accepted_at, coalesce(accepted_reason, ''), version`, id, version, now)
	} else {
		result = tx.QueryRow(ctx, `
			update operational_incidents set status = 'CLOSED_ACCEPTED', accepted_at = $3, accepted_reason = $4, reminder_due_at = null,
			version = version + 1, updated_at = $3
			where id = $1 and version = $2 and status in ('OPEN', 'ACKNOWLEDGED')
			returning id, alert_ref, incident_type, root_cause, severity, status, coalesce(safe_error_code, ''), occurrence_count,
			affected_count, first_seen_at, last_seen_at, acknowledged_at, resolved_at, accepted_at, coalesce(accepted_reason, ''), version`, id, version, now, reason)
	}
	item, err := scanIncident(result)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if lookupErr := tx.QueryRow(ctx, `select exists(select 1 from operational_incidents where id = $1)`, id).Scan(&exists); lookupErr != nil {
			return sentinel.Incident{}, fmt.Errorf("check operational incident conflict: %w", lookupErr)
		}
		if !exists {
			return sentinel.Incident{}, sentinel.ErrNotFound
		}
		return sentinel.Incident{}, sentinel.ErrVersionConflict
	}
	if err != nil {
		return sentinel.Incident{}, fmt.Errorf("mutate operational incident: %w", err)
	}
	if _, err := tx.Exec(ctx, `insert into operational_incident_events (incident_id, event_kind, observed_at) values ($1, $2, $3)`, id, eventKind, now); err != nil {
		return sentinel.Incident{}, fmt.Errorf("record operational incident mutation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sentinel.Incident{}, fmt.Errorf("commit operational incident mutation: %w", err)
	}
	return item, nil
}

func (store *SentinelStore) ClaimAlert(ctx context.Context, workerID string, lease time.Duration, now time.Time) (sentinel.Alert, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return sentinel.Alert{}, fmt.Errorf("begin operational alert claim: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// A process can stop after claiming an alert and before it records the
	// provider result. Reclaim only expired leases so a live sender is never
	// raced, while a restart cannot leave the outbox stuck in SENDING forever.
	if _, err := tx.Exec(ctx, `
		update operational_alert_outbox
		set status = 'PENDING', claimed_by = null, claimed_at = null,
		    lease_expires_at = null, updated_at = $1
		where status = 'SENDING' and lease_expires_at <= $1`, now); err != nil {
		return sentinel.Alert{}, fmt.Errorf("reclaim expired operational alerts: %w", err)
	}
	var alert sentinel.Alert
	err = tx.QueryRow(ctx, `
		select outbox.id, outbox.alert_kind,
		       incident.id, incident.alert_ref, incident.incident_type, incident.root_cause, incident.severity, incident.status,
		       coalesce(incident.safe_error_code, ''), incident.occurrence_count, incident.affected_count, incident.first_seen_at,
		       incident.last_seen_at, incident.acknowledged_at, incident.resolved_at, incident.accepted_at,
		       coalesce(incident.accepted_reason, ''), incident.version
		from operational_alert_outbox outbox join operational_incidents incident on incident.id = outbox.incident_id
		where outbox.status = 'PENDING' and outbox.available_at <= $1
		  and ((outbox.alert_kind in ('OPEN', 'REMINDER') and incident.status = 'OPEN') or (outbox.alert_kind = 'RECOVERY' and incident.status = 'RESOLVED'))
		order by outbox.available_at, outbox.id for update of outbox skip locked limit 1`, now).Scan(
		&alert.ID, &alert.Kind, &alert.Incident.ID, &alert.Incident.AlertRef, &alert.Incident.IncidentType,
		&alert.Incident.RootCause, &alert.Incident.Severity, &alert.Incident.Status, &alert.Incident.SafeErrorCode,
		&alert.Incident.OccurrenceCount, &alert.Incident.AffectedCount, &alert.Incident.FirstSeenAt, &alert.Incident.LastSeenAt,
		&alert.Incident.AcknowledgedAt, &alert.Incident.ResolvedAt, &alert.Incident.AcceptedAt, &alert.Incident.AcceptedReason,
		&alert.Incident.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return sentinel.Alert{}, sentinel.ErrNoAlertReady
	}
	if err != nil {
		return sentinel.Alert{}, fmt.Errorf("select operational alert: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update operational_alert_outbox set status = 'SENDING', claimed_by = $2, claimed_at = $3,
		lease_expires_at = $4, attempt = attempt + 1, updated_at = $3 where id = $1`, alert.ID, workerID, now, now.Add(lease)); err != nil {
		return sentinel.Alert{}, fmt.Errorf("claim operational alert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sentinel.Alert{}, fmt.Errorf("commit operational alert claim: %w", err)
	}
	return alert, nil
}

func (store *SentinelStore) CompleteAlert(ctx context.Context, alertID uuid.UUID, workerID string, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		with completed as (
		  update operational_alert_outbox set status = 'SENT', sent_at = $3, claimed_by = null, claimed_at = null,
		  lease_expires_at = null, updated_at = $3 where id = $1 and claimed_by = $2 and status = 'SENDING'
		  returning incident_id
		)
		insert into operational_incident_events (incident_id, event_kind, observed_at)
		select incident_id, 'ALERT_SENT', $3 from completed`, alertID, workerID, now)
	if err != nil {
		return fmt.Errorf("complete operational alert: %w", err)
	}
	if result.RowsAffected() != 1 {
		return sentinel.ErrAlertLeaseLost
	}
	return nil
}

func (store *SentinelStore) RetryAlert(ctx context.Context, alertID uuid.UUID, workerID, safeCode string, availableAt, now time.Time, permanent bool) error {
	status := "PENDING"
	if permanent {
		status = "FAILED_PERMANENT"
	}
	result, err := store.pool.Exec(ctx, `
		with failed as (
		  update operational_alert_outbox set status = $3, available_at = $4, last_safe_error_code = $5,
		  claimed_by = null, claimed_at = null, lease_expires_at = null, updated_at = $6
		  where id = $1 and claimed_by = $2 and status = 'SENDING' returning incident_id
		)
		insert into operational_incident_events (incident_id, event_kind, safe_error_code, observed_at)
		select incident_id, 'ALERT_FAILED', $5, $6 from failed`, alertID, workerID, status, availableAt, safeCode, now)
	if err != nil {
		return fmt.Errorf("retry operational alert: %w", err)
	}
	if result.RowsAffected() != 1 {
		return sentinel.ErrAlertLeaseLost
	}
	return nil
}

func (store *SentinelStore) ReconcileDatabaseIncident(ctx context.Context, alertRef string, startedAt, recoveredAt time.Time) (sentinel.Incident, error) {
	observation := sentinel.Observation{
		IncidentType: "PLATFORM_DATABASE_UNAVAILABLE", RootCause: sentinel.RootPlatform, Severity: sentinel.SeverityP1,
		SourceKind: sentinel.SourceDatabase, SourceID: stableOperationalID("production-database"),
		SafeErrorCode: "DATABASE_UNAVAILABLE", ObservedAt: startedAt.UTC(),
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return sentinel.Incident{}, fmt.Errorf("begin database incident reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	item, err := scanIncident(tx.QueryRow(ctx, `
		insert into operational_incidents (
		  alert_ref, fingerprint, incident_type, root_cause, severity, status, safe_error_code,
		  occurrence_count, affected_count, first_seen_at, last_seen_at, aggregation_until,
		  resolved_at, created_at, updated_at
		) values ($1, $2, $3, $4, $5, 'RESOLVED', $6, 1, 1, $7, $8, $7, $8, $8, $8)
		on conflict (alert_ref) do update set updated_at = excluded.updated_at
		returning id, alert_ref, incident_type, root_cause, severity, status, coalesce(safe_error_code, ''), occurrence_count,
		affected_count, first_seen_at, last_seen_at, acknowledged_at, resolved_at, accepted_at, coalesce(accepted_reason, ''), version`,
		alertRef, observation.Fingerprint(), observation.IncidentType, observation.RootCause, observation.Severity,
		observation.SafeErrorCode, startedAt.UTC(), recoveredAt.UTC()))
	if err != nil {
		return sentinel.Incident{}, fmt.Errorf("upsert database incident reconciliation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into operational_incident_events (incident_id, event_kind, source_kind, source_id, safe_error_code, observed_at)
		values ($1, 'OBSERVED', 'DATABASE', $2, $3, $4),
		       ($1, 'EVIDENCE_RESOLVED', 'DATABASE', $2, $3, $5)
		on conflict do nothing`, item.ID, observation.SourceID, observation.SafeErrorCode, startedAt.UTC(), recoveredAt.UTC()); err != nil {
		return sentinel.Incident{}, fmt.Errorf("record database incident evidence: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return sentinel.Incident{}, fmt.Errorf("commit database incident reconciliation: %w", err)
	}
	return item, nil
}
