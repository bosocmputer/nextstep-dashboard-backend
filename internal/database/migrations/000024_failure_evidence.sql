alter table report_runs
  add column failure_evidence_version smallint,
  add column failure_category text,
  add column failure_stage text,
  add column failure_transport_phase text,
  add column failure_occurred_at timestamptz,
  add column failure_duration_ms bigint,
  add column failure_attempt integer,
  add column failure_retryable boolean,
  add column failure_remote_state_unknown boolean;

alter table report_runs
  add constraint report_runs_failure_evidence_version_check
    check (failure_evidence_version is null or failure_evidence_version = 1) not valid,
  add constraint report_runs_failure_category_check
    check (failure_category is null or failure_category in (
      'SML_CONFIGURATION', 'JAVA_WS_CONNECTIVITY', 'JAVA_WS_RESPONSE', 'REPORT_PROCESSING',
      'QUEUE_WORKER', 'NOTIFICATION', 'LINE_DELIVERY', 'PLATFORM', 'CAPACITY'
    )) not valid,
  add constraint report_runs_failure_stage_check
    check (failure_stage is null or failure_stage in (
      'LOAD_CONNECTION', 'RESOLVE_ENDPOINT', 'CONNECT_JAVA_WS', 'SEND_REQUEST', 'WAIT_RESPONSE',
      'READ_RESPONSE', 'VALIDATE_RESPONSE', 'DECODE_PAYLOAD', 'BUILD_REPORT', 'SAVE_REPORT',
      'PREPARE_NOTIFICATION', 'SEND_LINE', 'QUEUE_EXECUTION', 'PLATFORM_CHECK'
    )) not valid,
  add constraint report_runs_failure_transport_phase_check
    check (failure_transport_phase is null or failure_transport_phase in (
      'BEFORE_REQUEST_SENT', 'REQUEST_SENT_RESULT_UNKNOWN', 'RESPONSE_STARTED'
    )) not valid,
  add constraint report_runs_failure_duration_check
    check (failure_duration_ms is null or failure_duration_ms >= 0) not valid,
  add constraint report_runs_failure_attempt_check
    check (failure_attempt is null or failure_attempt >= 0) not valid,
  add constraint report_runs_failure_evidence_shape_check
    check (
      (failure_evidence_version is null and failure_category is null and failure_stage is null)
      or (failure_evidence_version = 1 and failure_category is not null and failure_stage is not null
          and failure_occurred_at is not null and failure_retryable is not null
          and failure_remote_state_unknown is not null)
    ) not valid;

alter table report_runs validate constraint report_runs_failure_evidence_version_check;
alter table report_runs validate constraint report_runs_failure_category_check;
alter table report_runs validate constraint report_runs_failure_stage_check;
alter table report_runs validate constraint report_runs_failure_transport_phase_check;
alter table report_runs validate constraint report_runs_failure_duration_check;
alter table report_runs validate constraint report_runs_failure_attempt_check;
alter table report_runs validate constraint report_runs_failure_evidence_shape_check;

alter table operational_incident_events
  drop constraint operational_incident_events_event_kind_check;

alter table operational_incident_events
  add constraint operational_incident_events_event_kind_check
    check (event_kind in (
      'OBSERVED', 'DOWNSTREAM_IMPACT', 'ACKNOWLEDGED', 'EVIDENCE_RESOLVED',
      'RISK_ACCEPTED', 'ALERT_SENT', 'ALERT_FAILED'
    )) not valid,
  add column correlation_key text,
  add column downstream boolean not null default false,
  add column caused_by_incident_id uuid references operational_incidents(id) on delete set null,
  add column failure_evidence_version smallint,
  add column failure_level text,
  add column failure_category text,
  add column failure_stage text,
  add column failure_transport_phase text,
  add column failure_occurred_at timestamptz,
  add column failure_duration_ms bigint,
  add column failure_attempt integer,
  add column failure_retryable boolean,
  add column failure_remote_state_unknown boolean,
  add column connection_version integer,
  add column report_key text references report_definitions(report_key) on delete set null,
  add column trigger_kind text,
  add column reports_total smallint,
  add column reports_succeeded smallint,
  add column reports_failed smallint,
  add column reports_cancelled smallint,
  add column notification_outcome text;

alter table operational_incident_events
  add constraint operational_incident_events_correlation_key_check
    check (correlation_key is null or correlation_key ~ '^[0-9a-f]{64}$') not valid,
  add constraint operational_incident_events_failure_version_check
    check (failure_evidence_version is null or failure_evidence_version = 1) not valid,
  add constraint operational_incident_events_failure_level_check
    check (failure_level is null or failure_level in ('CONFIRMED', 'LEGACY_PARTIAL')) not valid,
  add constraint operational_incident_events_failure_category_check
    check (failure_category is null or failure_category in (
      'SML_CONFIGURATION', 'JAVA_WS_CONNECTIVITY', 'JAVA_WS_RESPONSE', 'REPORT_PROCESSING',
      'QUEUE_WORKER', 'NOTIFICATION', 'LINE_DELIVERY', 'PLATFORM', 'CAPACITY'
    )) not valid,
  add constraint operational_incident_events_failure_stage_check
    check (failure_stage is null or failure_stage in (
      'LOAD_CONNECTION', 'RESOLVE_ENDPOINT', 'CONNECT_JAVA_WS', 'SEND_REQUEST', 'WAIT_RESPONSE',
      'READ_RESPONSE', 'VALIDATE_RESPONSE', 'DECODE_PAYLOAD', 'BUILD_REPORT', 'SAVE_REPORT',
      'PREPARE_NOTIFICATION', 'SEND_LINE', 'QUEUE_EXECUTION', 'PLATFORM_CHECK'
    )) not valid,
  add constraint operational_incident_events_failure_transport_phase_check
    check (failure_transport_phase is null or failure_transport_phase in (
      'BEFORE_REQUEST_SENT', 'REQUEST_SENT_RESULT_UNKNOWN', 'RESPONSE_STARTED'
    )) not valid,
  add constraint operational_incident_events_failure_duration_check
    check (failure_duration_ms is null or failure_duration_ms >= 0) not valid,
  add constraint operational_incident_events_connection_version_check
    check (connection_version is null or connection_version >= 0) not valid,
  add constraint operational_incident_events_trigger_kind_check
    check (trigger_kind is null or trigger_kind in ('UNKNOWN', 'SCHEDULED', 'TEST', 'VIEWER', 'BACKGROUND')) not valid,
  add constraint operational_incident_events_impact_check
    check (
      (reports_total is null and reports_succeeded is null and reports_failed is null and reports_cancelled is null)
      or (reports_total >= 0 and reports_succeeded >= 0 and reports_failed >= 0 and reports_cancelled >= 0
          and reports_succeeded + reports_failed + reports_cancelled <= reports_total)
    ) not valid,
  add constraint operational_incident_events_notification_outcome_check
    check (notification_outcome is null or notification_outcome in (
      'NOT_APPLICABLE', 'NOT_CREATED_INCOMPLETE_REPORT_SET', 'CREATED', 'UNKNOWN'
    )) not valid;

alter table operational_incident_events validate constraint operational_incident_events_event_kind_check;
alter table operational_incident_events validate constraint operational_incident_events_correlation_key_check;
alter table operational_incident_events validate constraint operational_incident_events_failure_version_check;
alter table operational_incident_events validate constraint operational_incident_events_failure_level_check;
alter table operational_incident_events validate constraint operational_incident_events_failure_category_check;
alter table operational_incident_events validate constraint operational_incident_events_failure_stage_check;
alter table operational_incident_events validate constraint operational_incident_events_failure_transport_phase_check;
alter table operational_incident_events validate constraint operational_incident_events_failure_duration_check;
alter table operational_incident_events validate constraint operational_incident_events_connection_version_check;
alter table operational_incident_events validate constraint operational_incident_events_trigger_kind_check;
alter table operational_incident_events validate constraint operational_incident_events_impact_check;
alter table operational_incident_events validate constraint operational_incident_events_notification_outcome_check;
