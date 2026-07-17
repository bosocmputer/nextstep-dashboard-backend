alter table operational_incidents
  add column family_fingerprint text,
  add column observation_mode text not null default 'DISCRETE',
  add column subject_type text not null default 'HOST_RESOURCE',
  add column active_affected_count integer not null default 1,
  add column burst_until timestamptz,
  add column update_alert_sent boolean not null default false,
  add column measurement_kind text,
  add column measurement_value double precision,
  add column measurement_threshold double precision,
  add column measurement_unit text;

update operational_incidents incident
set family_fingerprint = incident.fingerprint,
    subject_type = case
      when exists (select 1 from operational_incident_events event where event.incident_id = incident.id and event.tenant_id is not null) then 'TENANT'
      when exists (select 1 from operational_incident_events event where event.incident_id = incident.id and event.source_kind = 'BACKUP') then 'BACKUP_POLICY'
      when exists (select 1 from operational_incident_events event where event.incident_id = incident.id and event.source_kind = 'DATABASE') then 'DATABASE'
      else 'HOST_RESOURCE'
    end,
    active_affected_count = case when incident.status in ('OPEN', 'ACKNOWLEDGED') then incident.affected_count else 0 end,
    burst_until = incident.first_seen_at + interval '5 minutes';

alter table operational_incidents
  alter column family_fingerprint set not null,
  alter column burst_until set not null,
  add constraint operational_incidents_observation_mode_check
    check (observation_mode in ('DISCRETE', 'CONTINUOUS')) not valid,
  add constraint operational_incidents_subject_type_check
    check (subject_type in ('TENANT', 'HOST_RESOURCE', 'BACKUP_POLICY', 'DATABASE', 'CONTAINER', 'LINE_PROVIDER')) not valid,
  add constraint operational_incidents_active_affected_check
    check (active_affected_count >= 0 and active_affected_count <= affected_count) not valid,
  add constraint operational_incidents_measurement_check
    check (
      (measurement_kind is null and measurement_value is null and measurement_threshold is null and measurement_unit is null)
      or (measurement_kind in ('DISK_USED_PERCENT', 'MEMORY_AVAILABLE_PERCENT', 'DATABASE_CONNECTIONS_PERCENT', 'QUEUE_AGE_SECONDS')
          and measurement_value is not null and measurement_threshold is not null
          and measurement_unit in ('PERCENT', 'SECONDS', 'COUNT'))
    ) not valid;

alter table operational_incidents validate constraint operational_incidents_observation_mode_check;
alter table operational_incidents validate constraint operational_incidents_subject_type_check;
alter table operational_incidents validate constraint operational_incidents_active_affected_check;
alter table operational_incidents validate constraint operational_incidents_measurement_check;

create table operational_incident_subjects (
  incident_id uuid not null references operational_incidents(id) on delete cascade,
  subject_key text not null check (subject_key ~ '^[0-9a-f]{64}$'),
  subject_type text not null check (subject_type in ('TENANT', 'HOST_RESOURCE', 'BACKUP_POLICY', 'DATABASE', 'CONTAINER', 'LINE_PROVIDER')),
  tenant_id uuid references tenants(id) on delete set null,
  source_kind text not null check (source_kind in ('NOTIFICATION', 'DELIVERY', 'REPORT', 'WORKER', 'SML_CIRCUIT', 'HOST', 'BACKUP', 'DATABASE')),
  status text not null default 'ACTIVE' check (status in ('ACTIVE', 'RECOVERED')),
  observation_mode text not null check (observation_mode in ('DISCRETE', 'CONTINUOUS')),
  first_seen_at timestamptz not null,
  last_seen_at timestamptz not null,
  last_persisted_at timestamptz not null,
  last_failure_at timestamptz,
  recovered_at timestamptz,
  occurrence_count integer not null default 1 check (occurrence_count > 0),
  safe_error_code text not null check (char_length(safe_error_code) between 2 and 96),
  failure_category text,
  failure_stage text,
  transport_phase text,
  measurement_kind text,
  measurement_value double precision,
  measurement_threshold double precision,
  measurement_unit text,
  primary key (incident_id, subject_key),
  check (last_seen_at >= first_seen_at),
  check ((status = 'RECOVERED') = (recovered_at is not null)),
  check (
    (measurement_kind is null and measurement_value is null and measurement_threshold is null and measurement_unit is null)
    or (measurement_kind in ('DISK_USED_PERCENT', 'MEMORY_AVAILABLE_PERCENT', 'DATABASE_CONNECTIONS_PERCENT', 'QUEUE_AGE_SECONDS')
        and measurement_value is not null and measurement_threshold is not null
        and measurement_unit in ('PERCENT', 'SECONDS', 'COUNT'))
  )
);

create table sml_connection_tests (
  tenant_id uuid primary key references tenants(id) on delete cascade,
  lease_id uuid,
  status text not null default 'IDLE' check (status in ('IDLE', 'RUNNING')),
  started_at timestamptz,
  lease_expires_at timestamptz,
  cooldown_until timestamptz,
  updated_at timestamptz not null default now(),
  check ((status = 'RUNNING') = (lease_id is not null and started_at is not null and lease_expires_at is not null))
);

insert into operational_incident_subjects (
  incident_id, subject_key, subject_type, tenant_id, source_kind, status, observation_mode,
  first_seen_at, last_seen_at, last_persisted_at, last_failure_at, recovered_at,
  occurrence_count, safe_error_code, failure_category, failure_stage, transport_phase
)
select incident.id,
       encode(digest(coalesce(event.tenant_id::text, coalesce(event.source_kind, 'PLATFORM') || ':' || coalesce(event.source_id::text, incident.id::text)), 'sha256'), 'hex'),
       case
         when event.tenant_id is not null then 'TENANT'
         when event.source_kind = 'BACKUP' then 'BACKUP_POLICY'
         when event.source_kind = 'DATABASE' then 'DATABASE'
         else 'HOST_RESOURCE'
       end,
       event.tenant_id,
       coalesce(event.source_kind, 'HOST'),
       case when incident.status in ('OPEN', 'ACKNOWLEDGED') then 'ACTIVE' else 'RECOVERED' end,
       'DISCRETE',
       min(event.observed_at), max(event.observed_at), max(event.observed_at), max(event.observed_at),
       case when incident.status in ('RESOLVED', 'CLOSED_ACCEPTED') then coalesce(incident.resolved_at, incident.accepted_at, incident.updated_at) end,
       count(*)::integer,
       coalesce(max(event.safe_error_code), incident.safe_error_code, 'UNKNOWN'),
       max(event.failure_category), max(event.failure_stage), max(event.failure_transport_phase)
from operational_incidents incident
join operational_incident_events event on event.incident_id = incident.id and event.event_kind = 'OBSERVED'
group by incident.id, event.tenant_id, event.source_kind, event.source_id, incident.status,
         incident.resolved_at, incident.accepted_at, incident.updated_at, incident.safe_error_code
on conflict do nothing;

alter table operational_incident_events drop constraint operational_incident_events_event_kind_check;
alter table operational_incident_events
  add constraint operational_incident_events_event_kind_check
  check (event_kind in (
    'OBSERVED', 'CONDITION_UPDATED', 'DOWNSTREAM_IMPACT', 'SUBJECT_RECOVERED',
    'ACKNOWLEDGED', 'EVIDENCE_RESOLVED', 'POLICY_CHANGED', 'RISK_ACCEPTED',
    'ALERT_SENT', 'ALERT_FAILED'
  )) not valid;
alter table operational_incident_events validate constraint operational_incident_events_event_kind_check;

alter table operational_alert_outbox drop constraint operational_alert_outbox_alert_kind_check;
alter table operational_alert_outbox
  add constraint operational_alert_outbox_alert_kind_check
  check (alert_kind in ('OPEN', 'UPDATE', 'REMINDER', 'RECOVERY')) not valid;
alter table operational_alert_outbox validate constraint operational_alert_outbox_alert_kind_check;

with changed as (
  update operational_incidents incident
  set status = 'RESOLVED', resolved_at = now(), reminder_due_at = null,
      active_affected_count = 0, version = version + 1, updated_at = now()
  where incident.status in ('OPEN', 'ACKNOWLEDGED')
    and incident.safe_error_code in (
      'BACKUP_OFFSITE_NOT_CONFIGURED', 'BACKUP_STALE', 'BACKUP_OVERDUE',
      'BACKUP_CHECKSUM_INVALID', 'RESTORE_VERIFICATION_STALE', 'RESTORE_VERIFICATION_OVERDUE'
    )
  returning incident.id
)
insert into operational_incident_events (incident_id, event_kind, safe_error_code, observed_at)
select changed.id, 'POLICY_CHANGED', 'BACKUP_POLICY_PRE_MIGRATION_ONLY', now() from changed;

update operational_incident_subjects subject
set status = 'RECOVERED', recovered_at = coalesce(subject.recovered_at, now())
from operational_incidents incident
where incident.id = subject.incident_id and incident.status = 'RESOLVED'
  and incident.safe_error_code in (
    'BACKUP_OFFSITE_NOT_CONFIGURED', 'BACKUP_STALE', 'BACKUP_OVERDUE',
    'BACKUP_CHECKSUM_INVALID', 'RESTORE_VERIFICATION_STALE', 'RESTORE_VERIFICATION_OVERDUE'
  );
