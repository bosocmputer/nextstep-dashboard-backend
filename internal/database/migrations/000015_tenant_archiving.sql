alter table tenants
  add column archived_at timestamptz;

create index tenants_unarchived_updated_idx
  on tenants (updated_at desc, id desc)
  where archived_at is null;
