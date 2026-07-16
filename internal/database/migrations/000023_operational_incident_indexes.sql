-- nextstep:no-transaction
create unique index concurrently operational_incidents_active_fingerprint_idx on operational_incidents (fingerprint) where status in ('OPEN', 'ACKNOWLEDGED');
create index concurrently operational_incidents_list_idx on operational_incidents (status, severity, last_seen_at desc, id desc);
create index concurrently operational_incident_events_incident_idx on operational_incident_events (incident_id, observed_at, id);
create index concurrently operational_alert_outbox_claim_idx on operational_alert_outbox (available_at, id) where status = 'PENDING';
create index concurrently operational_maintenance_windows_active_idx on operational_maintenance_windows (starts_at, ends_at) where status = 'ACTIVE';
create index concurrently notification_runs_sentinel_terminal_idx on notification_runs (updated_at, id) where trigger_kind = 'SCHEDULED' and status in ('FAILED', 'PARTIAL_FAILED', 'BLOCKED_QUOTA');
create index concurrently line_deliveries_sentinel_terminal_idx on line_deliveries (updated_at, id) where status = 'FAILED_PERMANENT';
create index concurrently report_runs_sentinel_terminal_idx on report_runs (updated_at, id) where source = 'SCHEDULE' and status = 'FAILED';
