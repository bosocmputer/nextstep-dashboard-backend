-- nextstep:no-transaction
create index concurrently operational_incidents_family_active_idx on operational_incidents (family_fingerprint, burst_until desc) where status in ('OPEN', 'ACKNOWLEDGED');
create index concurrently operational_incident_subjects_active_idx on operational_incident_subjects (subject_key, incident_id) where status = 'ACTIVE';
create index concurrently operational_incident_subjects_incident_idx on operational_incident_subjects (incident_id, status, last_seen_at desc);
create index concurrently audit_logs_sml_connection_version_idx on audit_logs (tenant_id, (after_json ->> 'version'), created_at desc) where action = 'SML_CONNECTION_REPLACED';
