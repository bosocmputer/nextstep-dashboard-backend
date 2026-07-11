alter table tenant_memberships
  add column permissions_version integer not null default 1 check (permissions_version > 0);

alter table notification_schedule_reports
  drop constraint notification_schedule_reports_position_check;

alter table notification_schedule_reports
  add constraint notification_schedule_reports_position_check
  check (position between 1 and 10);
