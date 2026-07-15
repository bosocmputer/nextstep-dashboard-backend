alter table report_runs
  add column query_plan_fingerprint text,
  add column execution_strategy text not null default 'DIRECT',
  add column source_consistency text not null default 'STATEMENT',
  add column source_started_at timestamptz,
  add column source_finished_at timestamptz,
  add column progress_completed_chunks integer not null default 0,
  add column progress_total_chunks integer not null default 0,
  add constraint report_runs_query_plan_fingerprint_check check (
    query_plan_fingerprint is null or query_plan_fingerprint ~ '^[0-9a-f]{64}$'
  ),
  add constraint report_runs_execution_strategy_check check (execution_strategy in ('DIRECT', 'CHUNKED')),
  add constraint report_runs_source_consistency_check check (source_consistency in ('STATEMENT', 'SERIAL_WINDOW', 'CHUNK_WINDOW')),
  add constraint report_runs_chunk_progress_check check (
    progress_completed_chunks >= 0 and progress_total_chunks >= 0
    and progress_completed_chunks <= progress_total_chunks
  ),
  add constraint report_runs_id_tenant_key unique (id, tenant_id);

create table dashboard_generations (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id) on delete cascade,
  generation_key text not null check (generation_key ~ '^[0-9a-f]{64}$'),
  status text not null default 'BUILDING' check (status in ('BUILDING', 'PUBLISHED', 'FAILED', 'EXPIRED')),
  period_preset text not null check (period_preset in ('YESTERDAY', 'TODAY_TO_NOW', 'MONTH_TO_DATE', 'AS_OF_RUN', 'CUSTOM')),
  period_from date,
  period_to date,
  request_json jsonb not null default '{}'::jsonb,
  report_set_hash text not null check (report_set_hash ~ '^[0-9a-f]{64}$'),
  query_plan_set_fingerprint text not null check (query_plan_set_fingerprint ~ '^[0-9a-f]{64}$'),
  data_source_version integer not null check (data_source_version >= 0),
  projection text not null default 'SUMMARY' check (projection = 'SUMMARY'),
  source_consistency text not null default 'SERIAL_WINDOW' check (source_consistency in ('SERIAL_WINDOW', 'CHUNK_WINDOW')),
  total integer not null check (total between 1 and 10),
  completed integer not null default 0 check (completed >= 0),
  failed integer not null default 0 check (failed >= 0),
  source_started_at timestamptz,
  source_finished_at timestamptz,
  published_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  finished_at timestamptz,
  unique (id, tenant_id),
  check (completed + failed <= total),
  check ((status = 'PUBLISHED') = (published_at is not null))
);

create table dashboard_generation_reports (
  generation_id uuid not null,
  tenant_id uuid not null,
  report_key text not null references report_definitions(report_key),
  report_run_id uuid not null references report_runs(id) on delete cascade,
  position smallint not null check (position between 1 and 10),
  primary key (generation_id, report_key),
  unique (generation_id, report_run_id),
  unique (generation_id, position),
  foreign key (generation_id, tenant_id) references dashboard_generations(id, tenant_id) on delete cascade,
  foreign key (report_run_id, tenant_id) references report_runs(id, tenant_id) on delete cascade
);

create table dashboard_generation_heads (
  tenant_id uuid not null references tenants(id) on delete cascade,
  generation_key text not null check (generation_key ~ '^[0-9a-f]{64}$'),
  published_generation_id uuid,
  version integer not null default 1 check (version > 0),
  updated_at timestamptz not null default now(),
  primary key (tenant_id, generation_key),
  foreign key (published_generation_id, tenant_id) references dashboard_generations(id, tenant_id)
);

alter table dashboard_refreshes
  add column generation_id uuid,
  add column request_hash text,
  add constraint dashboard_refreshes_request_hash_check check (
    request_hash is null or request_hash ~ '^[0-9a-f]{64}$'
  ),
  add foreign key (generation_id, tenant_id) references dashboard_generations(id, tenant_id);

create table report_run_chunks (
  run_id uuid not null references report_runs(id) on delete cascade,
  chunk_no integer not null check (chunk_no > 0),
  chunk_key text not null check (char_length(chunk_key) between 8 and 160),
  status text not null default 'QUEUED' check (status in ('QUEUED', 'RUNNING', 'SUCCEEDED', 'FAILED')),
  cursor_from text,
  cursor_to text,
  unit_count integer not null default 0 check (unit_count >= 0),
  total_units integer not null default 0 check (total_units >= 0),
  row_count integer not null default 0 check (row_count >= 0),
  attempt integer not null default 0 check (attempt >= 0),
  metadata_json jsonb not null default '{}'::jsonb,
  result_json jsonb not null default '{}'::jsonb,
  safe_error_code text,
  started_at timestamptz,
  finished_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  primary key (run_id, chunk_no),
  unique (run_id, chunk_key),
  check (unit_count <= total_units or total_units = 0)
);
