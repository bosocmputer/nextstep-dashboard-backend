alter table report_runs
  add column dashboard_version text,
  add column dashboard_json jsonb not null default '{}'::jsonb,
  add constraint report_runs_dashboard_size_check
    check (octet_length(dashboard_json::text) <= 131072);

create index report_runs_latest_dashboard_idx
  on report_runs (tenant_id, report_key, finished_at desc, id desc)
  where status = 'SUCCEEDED' and dashboard_json <> '{}'::jsonb;
