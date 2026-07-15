alter table tenant_sml_connections
  add column endpoint_host_key bytea;

update tenant_sml_connections
set endpoint_host_key = digest(
  lower(split_part(endpoint_url, '://', 1)) || '://' ||
  lower(
    case
      when split_part(split_part(endpoint_url, '://', 2), '/', 1) ~ ':[0-9]+$'
        then split_part(split_part(endpoint_url, '://', 2), '/', 1)
      else split_part(split_part(endpoint_url, '://', 2), '/', 1) || ':' ||
        case when lower(split_part(endpoint_url, '://', 1)) = 'https' then '443' else '80' end
    end
  ),
  'sha256'
);

alter table tenant_sml_connections
  alter column endpoint_host_key set not null;

create or replace function set_sml_endpoint_host_key()
returns trigger language plpgsql as $$
declare
  endpoint_scheme text;
  endpoint_authority text;
begin
  endpoint_scheme := lower(split_part(new.endpoint_url, '://', 1));
  endpoint_authority := lower(split_part(split_part(new.endpoint_url, '://', 2), '/', 1));
  if endpoint_authority !~ ':[0-9]+$' then
    endpoint_authority := endpoint_authority || ':' || case when endpoint_scheme = 'https' then '443' else '80' end;
  end if;
  new.endpoint_host_key := digest(endpoint_scheme || '://' || endpoint_authority, 'sha256');
  return new;
end;
$$;

create trigger tenant_sml_connections_host_key
before insert or update of endpoint_url on tenant_sml_connections
for each row execute function set_sml_endpoint_host_key();

create index tenant_sml_connections_host_key_idx
  on tenant_sml_connections (endpoint_host_key);

create table sml_host_circuits (
  host_key bytea primary key check (octet_length(host_key) = 32),
  consecutive_failures smallint not null default 0 check (consecutive_failures >= 0),
  window_started_at timestamptz,
  open_until timestamptz,
  half_open_run_id uuid references report_runs(id) on delete set null,
  updated_at timestamptz not null default now()
);

create index sml_host_circuits_open_idx
  on sml_host_circuits (open_until)
  where open_until is not null;

create table tenant_query_runtime (
  tenant_id uuid primary key references tenants(id) on delete cascade,
  last_claimed_at timestamptz,
  updated_at timestamptz not null default now()
);
