-- nextstep:no-transaction
create unique index concurrently if not exists dashboard_generations_active_key_idx
  on dashboard_generations (tenant_id, generation_key)
  where status = 'BUILDING';

create index concurrently if not exists dashboard_generations_lookup_idx
  on dashboard_generations (tenant_id, generation_key, published_at desc nulls last, id desc);

create index concurrently if not exists dashboard_generation_reports_run_idx
  on dashboard_generation_reports (report_run_id);

create unique index concurrently if not exists report_runs_summary_execution_active_idx
  on report_runs (tenant_id, execution_key)
  where source in ('DASHBOARD', 'BACKGROUND') and result_kind = 'SUMMARY'
    and execution_key is not null and status in ('QUEUED', 'CLAIMED', 'RUNNING');

create index concurrently if not exists report_run_chunks_status_idx
  on report_run_chunks (run_id, status, chunk_no);
