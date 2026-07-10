alter table notification_runs
  add column attempt integer not null default 0 check (attempt >= 0),
  add column next_attempt_at timestamptz not null default now();

drop index notification_runs_claim_idx;
create index notification_runs_claim_idx
on notification_runs (status, next_attempt_at, scheduled_for, id)
where status in ('QUEUED', 'COLLECTING', 'READY', 'SENDING');
