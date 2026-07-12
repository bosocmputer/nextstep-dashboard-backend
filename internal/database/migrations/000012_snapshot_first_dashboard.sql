alter table report_runs
  drop constraint report_runs_source_check;

alter table report_runs
  add constraint report_runs_source_check
  check (source in ('DASHBOARD', 'SCHEDULE', 'BACKGROUND'));

alter table report_runs
  add column result_kind text not null default 'DETAIL',
  add column priority smallint not null default 50,
  add column execution_key text,
  add column report_definition_version text,
  add column data_source_version integer,
  add column progress_phase text not null default 'QUEUED',
  add column progress_sequence integer not null default 0,
  add column progress_completed_steps integer not null default 0,
  add column progress_total_steps integer not null default 0,
  add column progress_updated_at timestamptz,
  add column expected_p50_ms bigint,
  add column expected_p90_ms bigint,
  add column expected_sample_count integer not null default 0,
  add constraint report_runs_result_kind_check check (result_kind in ('SUMMARY', 'DETAIL')),
  add constraint report_runs_priority_check check (priority between 0 and 100),
  add constraint report_runs_progress_phase_check check (progress_phase in (
    'QUEUED', 'CONNECTING', 'QUERYING_CURRENT', 'QUERYING_COMPARISON',
    'BUILDING_DASHBOARD', 'SAVING_RESULT', 'WAITING_RETRY', 'COMPLETED'
  )),
  add constraint report_runs_progress_values_check check (
    progress_sequence >= 0 and progress_completed_steps >= 0 and progress_total_steps >= 0
    and progress_completed_steps <= progress_total_steps
    and expected_sample_count >= 0
  );

update report_runs
set result_kind = case when source = 'SCHEDULE' then 'SUMMARY' else 'DETAIL' end,
    priority = case when source = 'SCHEDULE' then 100 else 80 end,
    progress_phase = case
      when status = 'SUCCEEDED' then 'COMPLETED'
      when status = 'QUEUED' then 'QUEUED'
      else 'CONNECTING'
    end;

create table tenant_dashboard_refresh_policies (
  tenant_id uuid primary key references tenants(id) on delete cascade,
  fast_interval_minutes smallint,
  standard_interval_minutes smallint,
  heavy_interval_minutes smallint,
  version integer not null default 1 check (version > 0),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  check (fast_interval_minutes is null or fast_interval_minutes in (5, 10, 15, 30, 60)),
  check (standard_interval_minutes is null or standard_interval_minutes in (15, 30, 60)),
  check (heavy_interval_minutes is null or heavy_interval_minutes in (30, 60, 120))
);

create table tenant_sml_circuits (
  tenant_id uuid primary key references tenants(id) on delete cascade,
  consecutive_failures smallint not null default 0 check (consecutive_failures >= 0),
  window_started_at timestamptz,
  open_until timestamptz,
  half_open_run_id uuid references report_runs(id) on delete set null,
  updated_at timestamptz not null default now()
);
