package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSentinelStoreClassifiesScheduledEventsAndGroupsLargeBatches(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `truncate operational_alert_outbox, operational_incident_events, operational_incidents, operational_monitor_cursors cascade`); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	store := NewSentinelStore(pool)
	observations, err := store.ScanObservations(ctx, now, 500, 5*time.Minute)
	if err != nil {
		t.Fatalf("baseline observations=%d err=%v", len(observations), err)
	}
	if terminalObservationCount(observations) != 0 {
		t.Fatalf("baseline unexpectedly included terminal events: %+v", observations)
	}
	quietBoundary := now.Add(time.Second)
	if err := store.AdvanceObservationCursors(ctx, quietBoundary); err != nil {
		t.Fatal(err)
	}
	var cursorCount int
	var oldestCursor time.Time
	if err := pool.QueryRow(ctx, `select count(*), min(cursor_updated_at) from operational_monitor_cursors`).Scan(&cursorCount, &oldestCursor); err != nil {
		t.Fatal(err)
	}
	if cursorCount != 3 || !oldestCursor.Equal(quietBoundary) {
		t.Fatalf("quiet cursors count=%d oldest=%s", cursorCount, oldestCursor)
	}

	tenantID, scheduleID := uuid.New(), uuid.New()
	defer func() { _, _ = pool.Exec(context.Background(), `delete from tenants where id=$1`, tenantID) }()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1,$2,'Sentinel test','Asia/Bangkok','ACTIVE',$3)`,
		tenantID, "sentinel-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into notification_schedules (id, tenant_id, name, status, local_time, timezone, period_preset)
		values ($1,$2,'Sentinel schedule','DRAFT','08:00','Asia/Bangkok','YESTERDAY')`, scheduleID, tenantID); err != nil {
		t.Fatal(err)
	}
	for index, trigger := range []string{"UNKNOWN", "TEST", "SCHEDULED"} {
		observedAt := now.Add(time.Duration(index+1) * time.Second)
		if _, err := pool.Exec(ctx, `
			insert into notification_runs (id, tenant_id, schedule_id, scheduled_for, status, trigger_kind, safe_error_code, updated_at)
			values ($1,$2,$3,$4,'FAILED',$5,'SML_UNREACHABLE',$4)`, uuid.New(), tenantID, scheduleID, observedAt, trigger); err != nil {
			t.Fatal(err)
		}
	}
	observations, err = store.ScanObservations(ctx, now.Add(10*time.Second), 500, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if terminalObservationCount(observations) != 1 {
		t.Fatalf("terminal observations=%+v, want only scheduled failure", observations)
	}

	batch := make([]sentinel.Observation, 100)
	for index := range batch {
		batch[index] = sentinel.Observation{
			IncidentType: "NEXTSTEP_CONTAINER_UNHEALTHY", RootCause: sentinel.RootPlatform, Severity: sentinel.SeverityP1,
			SourceKind: sentinel.SourceHost, SourceID: uuid.New(), SafeErrorCode: "CONTAINER_WORKER_UNHEALTHY", ObservedAt: now.Add(time.Minute),
		}
	}
	if err := store.RecordObservations(ctx, batch, now.Add(time.Minute), 30*time.Second, true); err != nil {
		t.Fatal(err)
	}
	var incidentCount, eventCount, outboxCount, affectedCount int
	if err := pool.QueryRow(ctx, `select count(*) from operational_incidents where incident_type='NEXTSTEP_CONTAINER_UNHEALTHY'`).Scan(&incidentCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from operational_incident_events event join operational_incidents incident on incident.id=event.incident_id where incident.incident_type='NEXTSTEP_CONTAINER_UNHEALTHY' and event.event_kind='OBSERVED'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from operational_alert_outbox outbox join operational_incidents incident on incident.id=outbox.incident_id where incident.incident_type='NEXTSTEP_CONTAINER_UNHEALTHY'`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select affected_count from operational_incidents where incident_type='NEXTSTEP_CONTAINER_UNHEALTHY'`).Scan(&affectedCount); err != nil {
		t.Fatal(err)
	}
	if incidentCount != 1 || eventCount != 100 || outboxCount != 1 || affectedCount != 100 {
		t.Fatalf("incidents=%d events=%d outbox=%d affected=%d", incidentCount, eventCount, outboxCount, affectedCount)
	}

	firstFailureAt := now.Add(2 * time.Minute)
	firstFailure := sentinel.Observation{
		IncidentType: "SCHEDULED_NOTIFICATION_FAILED", RootCause: sentinel.RootSMLConnectivity, Severity: sentinel.SeverityP1,
		SourceKind: sentinel.SourceNotification, SourceID: uuid.New(), TenantID: &tenantID,
		SafeErrorCode: "SML_UNREACHABLE", ObservedAt: firstFailureAt,
	}
	secondFailure := firstFailure
	secondFailure.SourceID = uuid.New()
	secondFailure.SafeErrorCode = "SML_TIMEOUT"
	secondFailure.ObservedAt = firstFailureAt.Add(10 * time.Second)
	if err := store.RecordObservations(ctx, []sentinel.Observation{firstFailure}, firstFailureAt, 30*time.Second, true); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordObservations(ctx, []sentinel.Observation{secondFailure}, secondFailure.ObservedAt, 30*time.Second, true); err != nil {
		t.Fatal(err)
	}
	var smlIncidentID uuid.UUID
	var smlIncidentType, smlSafeCode, smlStatus string
	var smlVersion, smlOccurrences int
	if err := pool.QueryRow(ctx, `select id, incident_type, safe_error_code, status, version, occurrence_count from operational_incidents where root_cause='SML_CONNECTIVITY'`).Scan(
		&smlIncidentID, &smlIncidentType, &smlSafeCode, &smlStatus, &smlVersion, &smlOccurrences); err != nil {
		t.Fatal(err)
	}
	if smlIncidentType != "SCHEDULED_NOTIFICATION_FAILED" || smlSafeCode != "MULTIPLE_SAFE_ERRORS" || smlOccurrences != 2 || smlStatus != "OPEN" {
		t.Fatalf("SML aggregate type=%s code=%s occurrences=%d status=%s", smlIncidentType, smlSafeCode, smlOccurrences, smlStatus)
	}
	page, err := store.ListIncidents(ctx, sentinel.IncidentFilter{PageSize: 25})
	if err != nil {
		t.Fatal(err)
	}
	foundTenantExample := false
	for _, incident := range page.Data {
		if incident.ID == smlIncidentID {
			foundTenantExample = len(incident.TenantExamples) == 1 && incident.TenantExamples[0] == "Sentinel test"
		}
	}
	if !foundTenantExample {
		t.Fatalf("incident list did not include bounded tenant example: %+v", page.Data)
	}
	if _, err := store.AcknowledgeIncident(ctx, smlIncidentID, smlVersion, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceLifecycle(ctx, nil, now.Add(4*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select status from operational_incidents where id=$1`, smlIncidentID).Scan(&smlStatus); err != nil || smlStatus != "ACKNOWLEDGED" {
		t.Fatalf("terminal incident resolved without evidence: status=%s err=%v", smlStatus, err)
	}
	completedAt := now.Add(5 * time.Minute)
	if _, err := pool.Exec(ctx, `
		insert into notification_runs (id, tenant_id, schedule_id, scheduled_for, status, trigger_kind, updated_at)
		values ($1,$2,$3,$4,'COMPLETED','SCHEDULED',$4)`, uuid.New(), tenantID, scheduleID, completedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceLifecycle(ctx, nil, now.Add(6*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	var recoveryEvidence int
	if err := pool.QueryRow(ctx, `select status from operational_incidents where id=$1`, smlIncidentID).Scan(&smlStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from operational_incident_events where incident_id=$1 and event_kind='EVIDENCE_RESOLVED'`, smlIncidentID).Scan(&recoveryEvidence); err != nil {
		t.Fatal(err)
	}
	if smlStatus != "RESOLVED" || recoveryEvidence != 1 {
		t.Fatalf("evidence recovery status=%s evidence=%d", smlStatus, recoveryEvidence)
	}

	if _, err := pool.Exec(ctx, `truncate operational_alert_outbox, operational_incident_events, operational_incidents cascade`); err != nil {
		t.Fatal(err)
	}
	leaseObservation := sentinel.Observation{
		IncidentType: "WORKER_HEARTBEAT_MISSING", RootCause: sentinel.RootPlatform, Severity: sentinel.SeverityP1,
		SourceKind: sentinel.SourceWorker, SourceID: uuid.New(), SafeErrorCode: "WORKER_HEARTBEAT_STALE", ObservedAt: now.Add(7 * time.Minute),
	}
	if err := store.RecordObservations(ctx, []sentinel.Observation{leaseObservation}, leaseObservation.ObservedAt, 0, true); err != nil {
		t.Fatal(err)
	}
	firstClaim, err := store.ClaimAlert(ctx, "sentinel-before-restart", time.Minute, leaseObservation.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	secondClaim, err := store.ClaimAlert(ctx, "sentinel-after-restart", time.Minute, leaseObservation.ObservedAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if firstClaim.ID != secondClaim.ID {
		t.Fatalf("expired lease claimed different alert: first=%s second=%s", firstClaim.ID, secondClaim.ID)
	}

	if _, err := pool.Exec(ctx, `truncate operational_alert_outbox, operational_incident_events, operational_incidents, operational_monitor_cursors cascade`); err != nil {
		t.Fatal(err)
	}
	backlogStart := now.Add(8 * time.Minute)
	if _, err := store.ScanObservations(ctx, backlogStart, 500, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into notification_runs (
		  id, tenant_id, schedule_id, scheduled_for, status, trigger_kind, safe_error_code, updated_at
		)
		select gen_random_uuid(), $1, $2, $3::timestamptz + make_interval(secs => sequence::double precision / 1000),
		       'FAILED', 'SCHEDULED', 'SML_UNREACHABLE', $3::timestamptz + make_interval(secs => sequence::double precision / 1000)
		from generate_series(1, 501) sequence`, tenantID, scheduleID, backlogStart); err != nil {
		t.Fatal(err)
	}
	backlogEnd := backlogStart.Add(time.Second)
	firstBatch, err := store.ScanObservations(ctx, backlogEnd, 500, 5*time.Minute)
	if err != nil || terminalObservationCount(firstBatch) != 500 {
		t.Fatalf("first backlog batch=%d err=%v", terminalObservationCount(firstBatch), err)
	}
	if err := store.RecordObservations(ctx, firstBatch, backlogEnd, 30*time.Second, false); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceObservationCursors(ctx, backlogEnd); err != nil {
		t.Fatal(err)
	}
	var notificationCursor time.Time
	if err := pool.QueryRow(ctx, `select cursor_updated_at from operational_monitor_cursors where monitor_key='notification_terminal'`).Scan(&notificationCursor); err != nil {
		t.Fatal(err)
	}
	if !notificationCursor.Before(backlogEnd) {
		t.Fatalf("truncated source cursor advanced past its backlog: %s >= %s", notificationCursor, backlogEnd)
	}
	secondBatch, err := store.ScanObservations(ctx, backlogEnd, 500, 5*time.Minute)
	if err != nil || terminalObservationCount(secondBatch) != 1 {
		t.Fatalf("remaining backlog batch=%d err=%v", terminalObservationCount(secondBatch), err)
	}
}

func TestSentinelStoreLinksIncompleteNotificationAsDownstreamWithoutSecondAlert(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `truncate operational_alert_outbox, operational_incident_events, operational_incidents, operational_monitor_cursors cascade`); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 17, 1, 0, 0, 0, time.UTC)
	tenantID, scheduleID := uuid.New(), uuid.New()
	defer func() { _, _ = pool.Exec(context.Background(), `delete from tenants where id=$1`, tenantID) }()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1,$2,'Sentinel correlation','Asia/Bangkok','ACTIVE',$3)`,
		tenantID, "sentinel-correlation-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into notification_schedules (id, tenant_id, name, status, local_time, timezone, period_preset)
		values ($1,$2,'Sentinel correlation','DRAFT','08:00','Asia/Bangkok','YESTERDAY')`, scheduleID, tenantID); err != nil {
		t.Fatal(err)
	}
	store := NewSentinelStore(pool)
	if _, err := store.ScanObservations(ctx, now, 500, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceObservationCursors(ctx, now); err != nil {
		t.Fatal(err)
	}

	reportRunID, notificationRunID := uuid.New(), uuid.New()
	failedAt := now.Add(time.Second)
	if _, err := pool.Exec(ctx, `
		insert into report_runs (
		  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
		  period_from, period_to, safe_error_code, queued_at, started_at, finished_at,
		  expires_at, result_kind, priority, updated_at, failure_evidence_version,
		  failure_category, failure_stage, failure_transport_phase, failure_occurred_at,
		  failure_retryable, failure_remote_state_unknown
		) values ($1,$2,'stock_balance','SCHEDULE',$3,'FAILED','YESTERDAY','2026-07-16','2026-07-16',
		          'SML_UNREACHABLE',$4,$4,$4,$5,'SUMMARY',100,$4,1,'JAVA_WS_CONNECTIVITY',
		          'CONNECT_JAVA_WS','BEFORE_REQUEST_SENT',$4,true,false)`,
		reportRunID, tenantID, "sentinel-correlation-"+reportRunID.String(), failedAt, failedAt.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into notification_runs (
		  id, tenant_id, schedule_id, scheduled_for, status, trigger_kind, safe_error_code,
		  materialization_version, finished_at, updated_at
		) values ($1,$2,$3,$4,'FAILED','SCHEDULED','REPORT_SET_INCOMPLETE',2,$4,$4)`,
		notificationRunID, tenantID, scheduleID, failedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into notification_run_reports (notification_run_id, report_key, report_run_id, position)
		values ($1,'stock_balance',$2,1)`, notificationRunID, reportRunID); err != nil {
		t.Fatal(err)
	}

	firstBatch, err := store.ScanObservations(ctx, failedAt.Add(5*time.Second), 500, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var roots, downstream int
	var rootFingerprint string
	for _, observation := range firstBatch {
		if observation.SourceKind == sentinel.SourceReport && observation.SourceID == reportRunID {
			roots++
			rootFingerprint = observation.Fingerprint()
		}
		if observation.SourceKind == sentinel.SourceNotification && observation.SourceID == notificationRunID {
			downstream++
		}
	}
	if roots != 1 || downstream != 0 {
		t.Fatalf("first scan roots=%d downstream=%d observations=%+v", roots, downstream, firstBatch)
	}
	if err := store.RecordObservations(ctx, firstBatch, failedAt.Add(5*time.Second), 30*time.Second, true); err != nil {
		t.Fatal(err)
	}

	secondBatch, err := store.ScanObservations(ctx, failedAt.Add(6*time.Second), 500, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, observation := range secondBatch {
		if observation.SourceKind == sentinel.SourceNotification && observation.SourceID == notificationRunID && observation.Downstream {
			downstream++
			if observation.Fingerprint() != rootFingerprint {
				t.Fatalf("downstream fingerprint=%s root=%s", observation.Fingerprint(), rootFingerprint)
			}
		}
	}
	if downstream != 1 {
		t.Fatalf("second scan downstream=%d observations=%+v", downstream, secondBatch)
	}
	if err := store.RecordObservations(ctx, secondBatch, failedAt.Add(6*time.Second), 30*time.Second, true); err != nil {
		t.Fatal(err)
	}
	var incidents, alerts, rootEvents, downstreamEvents int
	if err := pool.QueryRow(ctx, `select count(*) from operational_incidents where root_cause='SML_CONNECTIVITY'`).Scan(&incidents); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from operational_alert_outbox outbox
		join operational_incidents incident on incident.id = outbox.incident_id
		where incident.root_cause = 'SML_CONNECTIVITY'`).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select
		count(*) filter (where source_kind='REPORT' and source_id=$1 and event_kind='OBSERVED'),
		count(*) filter (where source_kind='NOTIFICATION' and source_id=$2 and event_kind='DOWNSTREAM_IMPACT')
		from operational_incident_events`, reportRunID, notificationRunID).Scan(&rootEvents, &downstreamEvents); err != nil {
		t.Fatal(err)
	}
	if incidents != 1 || alerts != 1 || rootEvents != 1 || downstreamEvents != 1 {
		t.Fatalf("incidents=%d alerts=%d rootEvents=%d downstreamEvents=%d", incidents, alerts, rootEvents, downstreamEvents)
	}
}

func terminalObservationCount(observations []sentinel.Observation) int {
	count := 0
	for _, observation := range observations {
		if observation.CursorKey != "" && (observation.SourceKind == sentinel.SourceNotification || observation.SourceKind == sentinel.SourceDelivery || observation.SourceKind == sentinel.SourceReport) {
			count++
		}
	}
	return count
}
