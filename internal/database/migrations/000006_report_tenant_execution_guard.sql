with ranked_active as (
  select id,
         row_number() over (partition by tenant_id order by coalesce(started_at, claimed_at, queued_at), id) as tenant_rank
  from report_runs
  where status in ('CLAIMED', 'RUNNING')
)
update report_runs r
set status = 'QUEUED', claimed_by = null, claimed_at = null,
    lease_expires_at = null, queued_at = now(), updated_at = now()
from ranked_active ranked
where r.id = ranked.id and ranked.tenant_rank > 1;

create unique index report_runs_one_active_per_tenant_idx
  on report_runs (tenant_id)
  where status in ('CLAIMED', 'RUNNING');
