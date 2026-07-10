alter table line_recipients
  alter column line_user_id_hash drop not null,
  alter column line_user_id_ciphertext drop not null,
  alter column line_user_id_nonce drop not null;

alter table tenant_memberships drop constraint tenant_memberships_status_check;
alter table tenant_memberships
  add constraint tenant_memberships_status_check
  check (status in ('PENDING', 'ACTIVE', 'REVOKED'));

create table recipient_invitations (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id) on delete cascade,
  pending_recipient_id uuid not null references line_recipients(id) on delete cascade,
  reference_hash bytea not null unique,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at timestamptz,
  used_by_recipient_id uuid references line_recipients(id),
  unique (tenant_id, pending_recipient_id)
);

create index recipient_invitations_active_idx
on recipient_invitations (expires_at)
where used_at is null;
