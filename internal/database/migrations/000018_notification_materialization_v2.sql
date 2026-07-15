alter table notification_runs
  add column materialization_version smallint not null default 1;

alter table notification_runs
  add constraint notification_runs_materialization_version_check
  check (materialization_version in (1, 2)) not valid;

alter table notification_runs
  validate constraint notification_runs_materialization_version_check;

alter table notification_run_reports
  add column position smallint;

alter table notification_run_reports
  add constraint notification_run_reports_position_check
  check (position is null or position between 1 and 10) not valid;

alter table notification_run_reports
  validate constraint notification_run_reports_position_check;

create or replace function prevent_notification_materialization_mutation()
returns trigger language plpgsql as $$
begin
  if tg_op = 'DELETE' then
    if pg_trigger_depth() = 1 then
      raise exception 'notification report materialization is immutable' using errcode = '23514';
    end if;
    return old;
  end if;
  if old.notification_run_id is distinct from new.notification_run_id
     or old.report_key is distinct from new.report_key
     or old.report_run_id is distinct from new.report_run_id
     or old.position is distinct from new.position then
    raise exception 'notification report materialization is immutable' using errcode = '23514';
  end if;
  return new;
end;
$$;

create trigger notification_run_reports_write_once
before update or delete on notification_run_reports
for each row execute function prevent_notification_materialization_mutation();

create or replace function prevent_notification_run_version_mutation()
returns trigger language plpgsql as $$
begin
  if old.materialization_version is distinct from new.materialization_version then
    raise exception 'notification materialization version is immutable' using errcode = '23514';
  end if;
  return new;
end;
$$;

create trigger notification_runs_version_write_once
before update on notification_runs
for each row execute function prevent_notification_run_version_mutation();
