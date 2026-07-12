-- nextstep:no-transaction
create unique index concurrently if not exists report_runs_background_execution_active_idx
  on report_runs (tenant_id, execution_key)
  where source = 'BACKGROUND' and execution_key is not null
    and status in ('QUEUED', 'CLAIMED', 'RUNNING');
