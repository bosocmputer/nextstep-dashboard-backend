-- nextstep:no-transaction
create index concurrently if not exists report_runs_active_lease_expiry_idx
  on report_runs (lease_expires_at, id)
  where status in ('CLAIMED', 'RUNNING');
