alter table notification_runs
  add column trigger_kind text not null default 'UNKNOWN';

alter table notification_runs
  add constraint notification_runs_trigger_kind_check
  check (trigger_kind in ('UNKNOWN', 'SCHEDULED', 'TEST')) not valid;

alter table notification_runs
  validate constraint notification_runs_trigger_kind_check;

create table operational_incidents (
  id uuid primary key default gen_random_uuid(),
  alert_ref text not null unique check (alert_ref ~ '^NST-[A-Z0-9]{12}$'),
  fingerprint text not null check (char_length(fingerprint) between 16 and 128),
  incident_type text not null check (char_length(incident_type) between 3 and 80),
  root_cause text not null check (root_cause in ('SML_CONNECTIVITY', 'REPORT_DATA', 'LINE_DELIVERY', 'PLATFORM', 'CAPACITY')),
  severity text not null check (severity in ('P1', 'P2')),
  status text not null default 'OPEN' check (status in ('OPEN', 'ACKNOWLEDGED', 'RESOLVED', 'CLOSED_ACCEPTED')),
  safe_error_code text check (safe_error_code is null or char_length(safe_error_code) between 2 and 96),
  occurrence_count integer not null default 1 check (occurrence_count > 0),
  affected_count integer not null default 1 check (affected_count > 0),
  first_seen_at timestamptz not null,
  last_seen_at timestamptz not null,
  aggregation_until timestamptz not null,
  reminder_due_at timestamptz,
  acknowledged_at timestamptz,
  resolved_at timestamptz,
  accepted_at timestamptz,
  accepted_reason text check (accepted_reason is null or char_length(trim(accepted_reason)) between 12 and 500),
  version integer not null default 1 check (version > 0),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  check (last_seen_at >= first_seen_at),
  check (aggregation_until >= first_seen_at),
  check ((status = 'CLOSED_ACCEPTED') = (accepted_at is not null and accepted_reason is not null)),
  check ((status = 'RESOLVED') = (resolved_at is not null))
);

create table operational_incident_events (
  id uuid primary key default gen_random_uuid(),
  incident_id uuid not null references operational_incidents(id) on delete cascade,
  event_kind text not null check (event_kind in ('OBSERVED', 'ACKNOWLEDGED', 'EVIDENCE_RESOLVED', 'RISK_ACCEPTED', 'ALERT_SENT', 'ALERT_FAILED')),
  source_kind text check (source_kind is null or source_kind in ('NOTIFICATION', 'DELIVERY', 'REPORT', 'WORKER', 'SML_CIRCUIT', 'HOST', 'BACKUP', 'DATABASE')),
  source_id uuid,
  tenant_id uuid references tenants(id) on delete set null,
  safe_error_code text check (safe_error_code is null or char_length(safe_error_code) between 2 and 96),
  observed_at timestamptz not null,
  created_at timestamptz not null default now(),
  unique (incident_id, event_kind, source_kind, source_id, observed_at)
);

create table operational_alert_outbox (
  id uuid primary key default gen_random_uuid(),
  incident_id uuid not null references operational_incidents(id) on delete cascade,
  alert_kind text not null check (alert_kind in ('OPEN', 'REMINDER', 'RECOVERY')),
  status text not null default 'PENDING' check (status in ('PENDING', 'SENDING', 'SENT', 'FAILED_PERMANENT')),
  available_at timestamptz not null,
  claimed_by text,
  claimed_at timestamptz,
  lease_expires_at timestamptz,
  attempt integer not null default 0 check (attempt >= 0),
  last_safe_error_code text,
  sent_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (incident_id, alert_kind)
);

create table operational_monitor_cursors (
  monitor_key text primary key check (char_length(monitor_key) between 3 and 80),
  cursor_updated_at timestamptz not null,
  cursor_id uuid not null,
  initialized_at timestamptz not null,
  updated_at timestamptz not null
);

create table operational_maintenance_windows (
  id uuid primary key default gen_random_uuid(),
  source text not null check (source in ('DEPLOY', 'INTERNAL', 'EXTERNAL')),
  status text not null default 'ACTIVE' check (status in ('ACTIVE', 'CANCELLED', 'COMPLETED')),
  starts_at timestamptz not null,
  ends_at timestamptz not null,
  safe_reason text not null check (char_length(trim(safe_reason)) between 3 and 200),
  created_by_hash bytea,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  check (ends_at > starts_at),
  check (ends_at <= starts_at + interval '24 hours')
);
