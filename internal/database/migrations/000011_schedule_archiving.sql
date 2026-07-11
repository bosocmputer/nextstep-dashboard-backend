alter table notification_schedules
  drop constraint notification_schedules_status_check;

alter table notification_schedules
  add column archived_at timestamptz,
  add constraint notification_schedules_status_check
    check (status in ('DRAFT', 'ACTIVE', 'PAUSED', 'EXPIRED', 'ARCHIVED')),
  add constraint notification_schedules_archived_at_check
    check (
      (status = 'ARCHIVED' and archived_at is not null and next_run_at is null)
      or (status <> 'ARCHIVED' and archived_at is null)
    );

create index notification_schedules_archived_idx
  on notification_schedules (tenant_id, archived_at desc, id)
  where status = 'ARCHIVED';
