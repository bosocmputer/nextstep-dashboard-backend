create extension if not exists pgcrypto;

create table tenants (
  id uuid primary key default gen_random_uuid(),
  slug text not null unique check (slug ~ '^[a-z0-9]+(?:-[a-z0-9]+)*$'),
  name text not null check (char_length(trim(name)) between 1 and 160),
  timezone text not null default 'Asia/Bangkok' check (char_length(timezone) between 1 and 64),
  status text not null default 'DISABLED' check (status in ('ACTIVE', 'DISABLED', 'EXPIRED')),
  access_ends_at timestamptz not null,
  version integer not null default 1 check (version > 0),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (id, status)
);

create index tenants_status_expiry_idx on tenants (status, access_ends_at);

create table admin_sessions (
  id_hash bytea primary key,
  username text not null,
  csrf_hash bytea not null,
  created_at timestamptz not null default now(),
  last_seen_at timestamptz not null default now(),
  expires_at timestamptz not null,
  revoked_at timestamptz,
  ip_hash bytea,
  user_agent_hash bytea
);
create index admin_sessions_expiry_idx on admin_sessions (expires_at) where revoked_at is null;

create table admin_login_attempts (
  identity_hash bytea primary key,
  failed_count integer not null default 0 check (failed_count >= 0),
  first_failed_at timestamptz,
  locked_until timestamptz,
  updated_at timestamptz not null default now()
);

create table tenant_sml_connections (
  tenant_id uuid primary key references tenants(id) on delete cascade,
  endpoint_url text not null,
  database_name text not null,
  username_ciphertext bytea not null,
  username_nonce bytea not null,
  password_ciphertext bytea not null,
  password_nonce bytea not null,
  encryption_key_id text not null,
  version integer not null default 1 check (version > 0),
  readiness_status text not null default 'UNTESTED' check (readiness_status in ('UNTESTED', 'READY', 'FAILED')),
  last_tested_at timestamptz,
  last_safe_error_code text,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table line_recipients (
  id uuid primary key default gen_random_uuid(),
  line_user_id_hash bytea not null unique,
  line_user_id_ciphertext bytea not null,
  line_user_id_nonce bytea not null,
  display_name_ciphertext bytea,
  display_name_nonce bytea,
  encryption_key_id text not null,
  status text not null default 'PENDING' check (status in ('PENDING', 'ACTIVE', 'REVOKED')),
  verified_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

create table tenant_memberships (
  tenant_id uuid not null references tenants(id) on delete cascade,
  recipient_id uuid not null references line_recipients(id) on delete cascade,
  status text not null default 'ACTIVE' check (status in ('ACTIVE', 'REVOKED')),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  primary key (tenant_id, recipient_id)
);

create table report_definitions (
  report_key text primary key,
  version text not null,
  label_th text not null,
  category text not null,
  is_sensitive boolean not null default false,
  contract_json jsonb not null default '{}'::jsonb,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now()
);

insert into report_definitions (report_key, version, label_th, category, is_sensitive, contract_json) values
  ('sales_goods_services', '1.0.0', 'รายงานขายสินค้าและบริการ', 'SALES', false, '{"periodParams":["dateFrom","dateTo"]}'),
  ('purchase_goods_payables', '1.0.0', 'รายงานซื้อสินค้าและตั้งหนี้', 'PURCHASE', true, '{"periodParams":["dateFrom","dateTo"]}'),
  ('gross_profit_by_product', '1.0.0', 'กำไรขั้นต้นตามสินค้า', 'GROSS_PROFIT', true, '{"periodParams":["dateFrom","dateTo"]}'),
  ('gross_profit_by_ar_customer', '1.0.0', 'กำไรขั้นต้นตามลูกหนี้', 'GROSS_PROFIT', true, '{"periodParams":["dateFrom","dateTo"]}'),
  ('stock_balance', '1.0.0', 'รายงานสต็อกคงเหลือ', 'INVENTORY', true, '{"periodParams":["asOfDate"]}'),
  ('stock_reorder', '1.0.0', 'รายงานสินค้าถึงจุดสั่งซื้อ', 'INVENTORY', false, '{"periodParams":["asOfDate"]}'),
  ('ar_customer_movement', '1.0.0', 'รายงานความเคลื่อนไหวลูกหนี้', 'AR', true, '{"periodParams":["asOfDate"]}'),
  ('ar_debt_receipt', '1.0.0', 'รายงานรับชำระหนี้', 'AR', true, '{"periodParams":["dateFrom","dateTo"]}'),
  ('cash_bank_receipts', '1.0.0', 'รายงานรับเงิน', 'CASH_BANK', true, '{"periodParams":["dateFrom","dateTo"]}'),
  ('cash_bank_payments', '1.0.0', 'รายงานจ่ายเงิน', 'CASH_BANK', true, '{"periodParams":["dateFrom","dateTo"]}');

create table recipient_report_permissions (
  tenant_id uuid not null,
  recipient_id uuid not null,
  report_key text not null references report_definitions(report_key),
  created_at timestamptz not null default now(),
  primary key (tenant_id, recipient_id, report_key),
  foreign key (tenant_id, recipient_id) references tenant_memberships(tenant_id, recipient_id) on delete cascade
);

create table notification_schedules (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id) on delete cascade,
  name text not null check (char_length(trim(name)) between 1 and 160),
  status text not null default 'DRAFT' check (status in ('DRAFT', 'ACTIVE', 'PAUSED', 'EXPIRED')),
  local_time time not null,
  timezone text not null check (char_length(timezone) between 1 and 64),
  period_preset text not null check (period_preset in ('YESTERDAY', 'TODAY_TO_NOW', 'MONTH_TO_DATE', 'AS_OF_RUN')),
  next_run_at timestamptz,
  version integer not null default 1 check (version > 0),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (id, tenant_id)
);
create unique index notification_schedules_tenant_name_idx on notification_schedules (tenant_id, lower(name));
create index notification_schedules_due_idx on notification_schedules (next_run_at, id) where status = 'ACTIVE';

create table notification_schedule_days (
  tenant_id uuid not null,
  schedule_id uuid not null,
  day_of_week smallint not null check (day_of_week between 0 and 6),
  primary key (schedule_id, day_of_week),
  foreign key (schedule_id, tenant_id) references notification_schedules(id, tenant_id) on delete cascade
);

create table notification_schedule_reports (
  tenant_id uuid not null,
  schedule_id uuid not null,
  report_key text not null references report_definitions(report_key),
  position smallint not null check (position between 1 and 5),
  primary key (schedule_id, report_key),
  unique (schedule_id, position),
  foreign key (schedule_id, tenant_id) references notification_schedules(id, tenant_id) on delete cascade
);

create table notification_schedule_recipients (
  tenant_id uuid not null,
  schedule_id uuid not null,
  recipient_id uuid not null,
  primary key (schedule_id, recipient_id),
  foreign key (schedule_id, tenant_id) references notification_schedules(id, tenant_id) on delete cascade,
  foreign key (tenant_id, recipient_id) references tenant_memberships(tenant_id, recipient_id) on delete cascade
);

create table report_runs (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id) on delete cascade,
  report_key text not null references report_definitions(report_key),
  source text not null check (source in ('DASHBOARD', 'SCHEDULE')),
  idempotency_key text not null check (char_length(idempotency_key) between 8 and 200),
  status text not null default 'QUEUED' check (status in ('QUEUED', 'CLAIMED', 'RUNNING', 'SUCCEEDED', 'FAILED', 'CANCELLED', 'EXPIRED')),
  period_preset text not null check (period_preset in ('YESTERDAY', 'TODAY_TO_NOW', 'MONTH_TO_DATE', 'AS_OF_RUN', 'CUSTOM')),
  period_from date,
  period_to date,
  params_json jsonb not null default '{}'::jsonb,
  requested_by_recipient_id uuid references line_recipients(id),
  claimed_by text,
  claimed_at timestamptz,
  lease_expires_at timestamptz,
  attempt integer not null default 0 check (attempt >= 0),
  row_count integer not null default 0 check (row_count >= 0),
  is_truncated boolean not null default false,
  summary_json jsonb not null default '{}'::jsonb,
  reconciliation_json jsonb not null default '{}'::jsonb,
  safe_error_code text,
  safe_error_message text,
  queued_at timestamptz not null default now(),
  started_at timestamptz,
  finished_at timestamptz,
  expires_at timestamptz not null default (now() + interval '24 hours'),
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (tenant_id, source, idempotency_key)
);
create index report_runs_claim_idx on report_runs (status, queued_at, id) where status in ('QUEUED', 'CLAIMED');
create index report_runs_tenant_history_idx on report_runs (tenant_id, report_key, created_at desc);
create index report_runs_expiry_idx on report_runs (expires_at) where status = 'SUCCEEDED';

create table report_run_rows (
  run_id uuid not null references report_runs(id) on delete cascade,
  tenant_id uuid not null references tenants(id) on delete cascade,
  ordinal integer not null check (ordinal > 0),
  row_json jsonb not null,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  primary key (created_at, run_id, ordinal)
) partition by range (created_at);
create table report_run_rows_default partition of report_run_rows default;
create index report_run_rows_run_idx on report_run_rows (run_id, ordinal);
create index report_run_rows_expiry_idx on report_run_rows (expires_at);

create table notification_runs (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id) on delete cascade,
  schedule_id uuid not null,
  scheduled_for timestamptz not null,
  status text not null default 'QUEUED' check (status in ('QUEUED', 'COLLECTING', 'READY', 'SENDING', 'COMPLETED', 'PARTIAL_FAILED', 'FAILED', 'BLOCKED_QUOTA', 'CANCELLED')),
  claimed_by text,
  claimed_at timestamptz,
  lease_expires_at timestamptz,
  safe_error_code text,
  started_at timestamptz,
  finished_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  unique (schedule_id, scheduled_for),
  foreign key (schedule_id, tenant_id) references notification_schedules(id, tenant_id) on delete cascade
);
create index notification_runs_claim_idx on notification_runs (status, scheduled_for, id) where status in ('QUEUED', 'COLLECTING');

create table notification_run_reports (
  notification_run_id uuid not null references notification_runs(id) on delete cascade,
  report_key text not null references report_definitions(report_key),
  report_run_id uuid not null references report_runs(id),
  primary key (notification_run_id, report_key),
  unique (notification_run_id, report_run_id)
);

create table line_deliveries (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid not null references tenants(id) on delete cascade,
  notification_run_id uuid not null references notification_runs(id) on delete cascade,
  recipient_id uuid not null references line_recipients(id),
  status text not null default 'PENDING' check (status in ('PENDING', 'SENDING', 'ACCEPTED', 'RETRY_WAIT', 'UNCERTAIN', 'FAILED_PERMANENT')),
  retry_key uuid not null unique default gen_random_uuid(),
  attempt integer not null default 0 check (attempt >= 0),
  provider_request_id text,
  safe_error_code text,
  next_attempt_at timestamptz,
  accepted_at timestamptz,
  created_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  expires_at timestamptz not null default (now() + interval '365 days'),
  unique (notification_run_id, recipient_id)
);
create index line_deliveries_history_idx on line_deliveries (tenant_id, created_at desc);
create index line_deliveries_retry_idx on line_deliveries (status, next_attempt_at, id) where status in ('PENDING', 'RETRY_WAIT', 'UNCERTAIN');

create table line_delivery_outbox (
  id uuid primary key default gen_random_uuid(),
  delivery_id uuid not null unique references line_deliveries(id) on delete cascade,
  tenant_id uuid not null references tenants(id) on delete cascade,
  payload_json jsonb not null,
  available_at timestamptz not null default now(),
  claimed_by text,
  claimed_at timestamptz,
  lease_expires_at timestamptz,
  attempt integer not null default 0 check (attempt >= 0),
  completed_at timestamptz,
  created_at timestamptz not null default now()
);
create index line_delivery_outbox_claim_idx on line_delivery_outbox (available_at, id) where completed_at is null;

create table viewer_sessions (
  id_hash bytea primary key,
  recipient_id uuid not null references line_recipients(id) on delete cascade,
  csrf_hash bytea not null,
  created_at timestamptz not null default now(),
  last_seen_at timestamptz not null default now(),
  expires_at timestamptz not null,
  revoked_at timestamptz
);
create index viewer_sessions_expiry_idx on viewer_sessions (expires_at) where revoked_at is null;

create table line_login_nonces (
  nonce_hash bytea primary key,
  state_hash bytea not null unique,
  pkce_verifier_ciphertext bytea,
  pkce_verifier_nonce bytea,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  used_at timestamptz
);

create table delivery_access_links (
  reference_hash bytea primary key,
  delivery_id uuid not null unique references line_deliveries(id) on delete cascade,
  tenant_id uuid not null references tenants(id) on delete cascade,
  recipient_id uuid not null references line_recipients(id) on delete cascade,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null default (now() + interval '365 days')
);
create index delivery_access_links_expiry_idx on delivery_access_links (expires_at);

create table idempotency_requests (
  actor_scope text not null,
  idempotency_key text not null,
  request_hash bytea not null,
  response_status integer,
  response_json jsonb,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null,
  primary key (actor_scope, idempotency_key)
);
create index idempotency_requests_expiry_idx on idempotency_requests (expires_at);

create table line_monthly_quota (
  quota_month date primary key,
  provider_limit integer check (provider_limit >= 0),
  provider_consumed integer check (provider_consumed >= 0),
  locally_accepted integer not null default 0 check (locally_accepted >= 0),
  operational_reserve_percent integer not null default 10 check (operational_reserve_percent between 0 and 50),
  synced_at timestamptz,
  updated_at timestamptz not null default now()
);

create table audit_logs (
  id uuid primary key default gen_random_uuid(),
  tenant_id uuid references tenants(id) on delete set null,
  actor_type text not null check (actor_type in ('ADMIN', 'VIEWER', 'WORKER', 'SYSTEM')),
  actor_id_hash bytea,
  action text not null,
  resource_type text not null,
  resource_id text,
  request_id text,
  before_json jsonb,
  after_json jsonb,
  result text not null check (result in ('SUCCESS', 'DENIED', 'FAILED')),
  safe_error_code text,
  created_at timestamptz not null default now(),
  expires_at timestamptz not null default (now() + interval '365 days')
);
create index audit_logs_tenant_history_idx on audit_logs (tenant_id, created_at desc);
create index audit_logs_expiry_idx on audit_logs (expires_at);

create table worker_heartbeats (
  worker_id text primary key,
  worker_type text not null check (worker_type in ('SCHEDULER', 'REPORT', 'DELIVERY', 'RETENTION')),
  node_name text not null,
  metadata_json jsonb not null default '{}'::jsonb,
  started_at timestamptz not null default now(),
  heartbeat_at timestamptz not null default now()
);
