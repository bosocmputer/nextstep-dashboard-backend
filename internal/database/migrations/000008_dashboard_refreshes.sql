create table dashboard_refreshes (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id) on delete cascade,
  requested_by_recipient_id uuid not null references line_recipients(id) on delete cascade,
  idempotency_key text not null check (char_length(idempotency_key) between 8 and 200),
  status text not null default 'QUEUED' check (status in ('QUEUED', 'RUNNING', 'PARTIAL', 'SUCCEEDED', 'FAILED')),
  total integer not null check (total between 1 and 10),
  completed integer not null default 0 check (completed >= 0),
  failed integer not null default 0 check (failed >= 0),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  finished_at timestamptz,
  unique (tenant_id, requested_by_recipient_id, idempotency_key)
);
create index dashboard_refreshes_tenant_created_idx on dashboard_refreshes (tenant_id, created_at desc, id desc);

create table dashboard_refresh_runs (
  refresh_id uuid not null references dashboard_refreshes(id) on delete cascade,
  report_key text not null references report_definitions(report_key),
  report_run_id uuid not null references report_runs(id) on delete cascade,
  primary key (refresh_id, report_key),
  unique (refresh_id, report_run_id)
);
