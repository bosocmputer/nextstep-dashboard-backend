-- nextstep:no-transaction
create index concurrently if not exists tenants_table_query_idx
  on tenants (updated_at desc, id desc)
  where archived_at is null;

create index concurrently if not exists notification_schedules_table_query_idx
  on notification_schedules (tenant_id, updated_at desc, id desc);

create index concurrently if not exists report_runs_table_query_idx
  on report_runs (created_at desc, id desc);

create index concurrently if not exists report_runs_tenant_table_query_idx
  on report_runs (tenant_id, created_at desc, id desc);

create index concurrently if not exists line_deliveries_table_query_idx
  on line_deliveries (created_at desc, id desc);

create index concurrently if not exists line_deliveries_recipient_table_query_idx
  on line_deliveries (recipient_id, created_at desc, id desc);

create index concurrently if not exists audit_logs_table_query_idx
  on audit_logs (created_at desc, id desc);

create index concurrently if not exists audit_logs_tenant_table_query_idx
  on audit_logs (tenant_id, created_at desc, id desc);

create index concurrently if not exists operational_incidents_active_table_query_idx
  on operational_incidents (last_seen_at desc, id desc)
  where status in ('OPEN', 'ACKNOWLEDGED');

create index concurrently if not exists operational_incident_events_occurrence_query_idx
  on operational_incident_events (incident_id, observed_at desc, id desc)
  where event_kind in ('OBSERVED', 'CONDITION_UPDATED') and not downstream;
