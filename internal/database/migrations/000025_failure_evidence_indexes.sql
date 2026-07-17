-- nextstep:no-transaction
create index concurrently if not exists operational_incident_events_correlation_idx
  on operational_incident_events (correlation_key, observed_at, id)
  where correlation_key is not null;

create index concurrently if not exists operational_incident_events_source_lookup_idx
  on operational_incident_events (source_kind, source_id, observed_at desc)
  where source_id is not null;
