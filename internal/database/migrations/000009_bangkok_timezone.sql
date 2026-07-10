update tenants set timezone = 'Asia/Bangkok' where timezone <> 'Asia/Bangkok';
update notification_schedules set timezone = 'Asia/Bangkok' where timezone <> 'Asia/Bangkok';

alter table tenants
  add constraint tenants_bangkok_timezone_check check (timezone = 'Asia/Bangkok');

alter table notification_schedules
  add constraint notification_schedules_bangkok_timezone_check check (timezone = 'Asia/Bangkok');
