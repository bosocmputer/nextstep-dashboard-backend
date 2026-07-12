-- nextstep:no-transaction
create index concurrently if not exists report_runs_exact_snapshot_idx
  on report_runs (
    tenant_id, report_key, period_from, period_to,
    report_definition_version, data_source_version, finished_at desc, id desc
  )
  where status = 'SUCCEEDED' and dashboard_json <> '{}'::jsonb;
