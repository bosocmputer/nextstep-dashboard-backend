\set ON_ERROR_STOP on

\echo 'Active schedules with recipients missing one or more scheduled permissions:'
select s.tenant_id,
       s.id as schedule_id,
       s.name as schedule_name,
       sr.recipient_id,
       array_agg(scheduled.report_key order by scheduled.position) as missing_report_keys
from notification_schedules s
join notification_schedule_recipients sr on sr.schedule_id = s.id
join notification_schedule_reports scheduled on scheduled.schedule_id = s.id
left join recipient_report_permissions permission
  on permission.tenant_id = s.tenant_id
 and permission.recipient_id = sr.recipient_id
 and permission.report_key = scheduled.report_key
where s.status = 'ACTIVE' and permission.report_key is null
group by s.tenant_id, s.id, s.name, sr.recipient_id
order by s.tenant_id, s.id, sr.recipient_id;

do $$
begin
  if exists (
    select 1
    from notification_schedules s
    join notification_schedule_recipients sr on sr.schedule_id = s.id
    join notification_schedule_reports scheduled on scheduled.schedule_id = s.id
    left join recipient_report_permissions permission
      on permission.tenant_id = s.tenant_id
     and permission.recipient_id = sr.recipient_id
     and permission.report_key = scheduled.report_key
    where s.status = 'ACTIVE' and permission.report_key is null
  ) then
    raise exception 'report management preflight failed: fix incomplete active schedule permissions manually; do not auto-pause';
  end if;
end $$;

\echo 'Schedules that prevent rollback to a worker supporting only five reports:'
select s.tenant_id,
       s.id as schedule_id,
       s.name as schedule_name,
       count(*) as report_count
from notification_schedules s
join notification_schedule_reports scheduled on scheduled.schedule_id = s.id
group by s.tenant_id, s.id, s.name
having count(*) > 5
order by report_count desc, s.tenant_id, s.id;
