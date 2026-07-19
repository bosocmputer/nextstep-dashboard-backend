#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/compose.production.yml"
env_file=${1:-"$script_dir/.env.production"}
mode=${2:-full}

if [ "$mode" != "full" ] && [ "$mode" != "static" ]; then
  echo "Usage: $0 [production-env-file] [full|static]" >&2
  exit 1
fi
if [ ! -r "$env_file" ]; then
  echo "Production environment file is not readable: $env_file" >&2
  exit 1
fi

file_mode() {
  if stat -c '%a' "$1" >/dev/null 2>&1; then
    stat -c '%a' "$1"
  else
    stat -f '%Lp' "$1"
  fi
}

env_value() {
  awk -v wanted="$1" '
    index($0, wanted "=") == 1 {
      count++
      value = substr($0, length(wanted) + 2)
    }
    END {
      if (count == 1) print value
      else exit 1
    }
  ' "$env_file"
}

require_value() {
  value=$(env_value "$1") || {
    echo "Environment key must appear exactly once: $1" >&2
    exit 1
  }
  if [ -z "$value" ]; then
    echo "Environment key must not be empty: $1" >&2
    exit 1
  fi
}

require_key() {
  if ! env_value "$1" >/dev/null; then
    echo "Environment key must appear exactly once: $1" >&2
    exit 1
  fi
}

required_keys='DASHBOARD_DOMAIN BACKEND_SHA FRONTEND_SHA FRONTEND_BIND_ADDRESS POSTGRES_DB POSTGRES_USER POSTGRES_PASSWORD DATABASE_URL ADMIN_USERNAME ADMIN_PASSWORD_HASH SESSION_HMAC_KEY ENCRYPTION_MASTER_KEY ENCRYPTION_KEY_ID SML_ALLOWED_CIDRS SML_ALLOW_PUBLIC_ENDPOINTS SML_ALLOWED_PORTS LINE_LOGIN_CHANNEL_ID LINE_MESSAGING_CHANNEL_ACCESS_TOKEN'
for key in $required_keys; do
  require_value "$key"
done
require_key SML_ALLOWED_HOSTS

feature_keys='SNAPSHOT_FIRST_ENABLED SNAPSHOT_FIRST_TENANT_IDS SUMMARY_QUERY_ENABLED GENERATION_CACHE_ENABLED STALE_REVALIDATION_ENABLED HEAVY_CHUNK_ENABLED HEAVY_CHUNK_TENANT_REPORTS SCHEDULE_CHUNK_ENABLED SMART_SCHEDULE_PERIODS_ENABLED SMART_SCHEDULE_PERIOD_TENANT_IDS REPORT_GLOBAL_QUERY_CONCURRENCY REPORT_HOST_QUERY_CONCURRENCY OPERATIONAL_ALERTS_MODE TELEGRAM_TENANT_CONTEXT_MODE SENTINEL_INTERVAL_SECONDS SENTINEL_HOST_RUNTIME_DIR WATCHDOG_ENABLED OFFSITE_BACKUP_CONFIGURED BACKUP_POLICY'
for key in $feature_keys; do
  require_key "$key"
done

for key in SML_ALLOW_PUBLIC_ENDPOINTS SNAPSHOT_FIRST_ENABLED SUMMARY_QUERY_ENABLED GENERATION_CACHE_ENABLED STALE_REVALIDATION_ENABLED HEAVY_CHUNK_ENABLED SCHEDULE_CHUNK_ENABLED SMART_SCHEDULE_PERIODS_ENABLED WATCHDOG_ENABLED OFFSITE_BACKUP_CONFIGURED; do
  value=$(env_value "$key")
  case "$value" in
    true|false) ;;
    *) echo "$key must be true or false" >&2; exit 1 ;;
  esac
done

operational_alerts_mode=$(env_value OPERATIONAL_ALERTS_MODE)
case "$operational_alerts_mode" in off|observe|send) ;; *) echo "OPERATIONAL_ALERTS_MODE must be off, observe, or send" >&2; exit 1 ;; esac
telegram_tenant_context_mode=$(env_value TELEGRAM_TENANT_CONTEXT_MODE)
case "$telegram_tenant_context_mode" in off|private_chat) ;; *) echo "TELEGRAM_TENANT_CONTEXT_MODE must be off or private_chat" >&2; exit 1 ;; esac
case "$(env_value BACKUP_POLICY)" in PRE_MIGRATION_ONLY|LOCAL_DAILY|LOCAL_AND_OFFSITE) ;; *) echo "BACKUP_POLICY must be PRE_MIGRATION_ONLY, LOCAL_DAILY, or LOCAL_AND_OFFSITE" >&2; exit 1 ;; esac
sentinel_interval=$(env_value SENTINEL_INTERVAL_SECONDS)
if ! printf '%s' "$sentinel_interval" | grep -Eq '^[0-9]+$' || [ "$sentinel_interval" -lt 15 ] || [ "$sentinel_interval" -gt 300 ]; then
  echo "SENTINEL_INTERVAL_SECONDS must be an integer between 15 and 300" >&2
  exit 1
fi
case "$(env_value SENTINEL_HOST_RUNTIME_DIR)" in /*) ;; *) echo "SENTINEL_HOST_RUNTIME_DIR must be an absolute path" >&2; exit 1 ;; esac

old_ifs=$IFS
allowed_ports=$(env_value SML_ALLOWED_PORTS)
if [ "$allowed_ports" != '*' ]; then
  IFS=,
  for port in $allowed_ports; do
    if ! printf '%s' "$port" | grep -Eq '^[0-9]{1,5}$' || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
      echo "SML_ALLOWED_PORTS must be * or contain comma-separated ports between 1 and 65535" >&2
      exit 1
    fi
  done
fi
IFS=$old_ifs

global_query_concurrency=$(env_value REPORT_GLOBAL_QUERY_CONCURRENCY)
host_query_concurrency=$(env_value REPORT_HOST_QUERY_CONCURRENCY)
if ! printf '%s' "$global_query_concurrency" | grep -Eq '^[0-9]+$' ||
   [ "$global_query_concurrency" -lt 1 ] || [ "$global_query_concurrency" -gt 32 ]; then
  echo "REPORT_GLOBAL_QUERY_CONCURRENCY must be an integer between 1 and 32" >&2
  exit 1
fi
if ! printf '%s' "$host_query_concurrency" | grep -Eq '^[0-9]+$' ||
   [ "$host_query_concurrency" -lt 1 ] || [ "$host_query_concurrency" -gt 16 ]; then
  echo "REPORT_HOST_QUERY_CONCURRENCY must be an integer between 1 and 16" >&2
  exit 1
fi
if [ "$(env_value GENERATION_CACHE_ENABLED)" = true ] && [ "$(env_value SUMMARY_QUERY_ENABLED)" != true ]; then
  echo "GENERATION_CACHE_ENABLED requires SUMMARY_QUERY_ENABLED" >&2
  exit 1
fi
if [ "$(env_value STALE_REVALIDATION_ENABLED)" = true ] && [ "$(env_value GENERATION_CACHE_ENABLED)" != true ]; then
  echo "STALE_REVALIDATION_ENABLED requires GENERATION_CACHE_ENABLED" >&2
  exit 1
fi
if [ "$(env_value SCHEDULE_CHUNK_ENABLED)" = true ] && [ "$(env_value HEAVY_CHUNK_ENABLED)" != true ]; then
  echo "SCHEDULE_CHUNK_ENABLED requires HEAVY_CHUNK_ENABLED" >&2
  exit 1
fi
if [ "$(env_value HEAVY_CHUNK_ENABLED)" = true ] && [ -z "$(env_value HEAVY_CHUNK_TENANT_REPORTS)" ]; then
  echo "HEAVY_CHUNK_ENABLED requires an explicit HEAVY_CHUNK_TENANT_REPORTS allowlist" >&2
  exit 1
fi

mode_value=$(file_mode "$env_file")
case "$mode_value" in
  400|600) ;;
  *) echo "Production environment file mode must be 400 or 600; found $mode_value" >&2; exit 1 ;;
esac

domain=$(env_value DASHBOARD_DOMAIN)
if [ "$domain" != "dashboard.nextstep-soft.com" ]; then
  echo "DASHBOARD_DOMAIN must be dashboard.nextstep-soft.com" >&2
  exit 1
fi
for sha_key in BACKEND_SHA FRONTEND_SHA; do
  sha=$(env_value "$sha_key")
  if ! printf '%s' "$sha" | grep -Eq '^[0-9a-f]{40}$'; then
    echo "$sha_key must be a full lowercase Git commit SHA" >&2
    exit 1
  fi
done

bind_address=$(env_value FRONTEND_BIND_ADDRESS)
case "$bind_address" in
  127.0.0.1) ;;
  0.0.0.0) echo "WARNING: port 6324 binds publicly; firewall it to the approved reverse proxy." >&2 ;;
  *) echo "FRONTEND_BIND_ADDRESS must be 127.0.0.1 or 0.0.0.0" >&2; exit 1 ;;
esac

database_url=$(env_value DATABASE_URL)
postgres_user=$(env_value POSTGRES_USER)
postgres_database=$(env_value POSTGRES_DB)
postgres_password=$(env_value POSTGRES_PASSWORD)
if ! printf '%s' "$postgres_user" | grep -Eq '^[a-z_][a-z0-9_-]{0,62}$' ||
   ! printf '%s' "$postgres_database" | grep -Eq '^[a-z_][a-z0-9_-]{0,62}$'; then
  echo "POSTGRES_USER and POSTGRES_DB must be compact lowercase identifiers" >&2
  exit 1
fi
if ! printf '%s' "$postgres_password" | grep -Eq '^[0-9a-fA-F]{64}$'; then
  echo "POSTGRES_PASSWORD must be a 32-byte hexadecimal value" >&2
  exit 1
fi
database_prefix="postgres://$postgres_user:$postgres_password@postgres:5432/$postgres_database?"
case "$database_url" in
  "$database_prefix"*'sslmode=verify-full'*'sslrootcert=/run/secrets/postgres-root.crt'*) ;;
  *) echo "DATABASE_URL must target postgres:5432 with verify-full and the mounted root certificate" >&2; exit 1 ;;
esac
case "$database_url" in
  *REPLACE*) echo "DATABASE_URL still contains a placeholder" >&2; exit 1 ;;
esac

admin_hash=$(env_value ADMIN_PASSWORD_HASH)
case "$admin_hash" in
  \'*\') ;;
  *) echo "ADMIN_PASSWORD_HASH must be single-quoted so Docker Compose preserves literal dollar signs" >&2; exit 1 ;;
esac
admin_hash=${admin_hash#\'}
admin_hash=${admin_hash%\'}
case "$admin_hash" in
  '$argon2id$'*) ;;
  *) echo "ADMIN_PASSWORD_HASH must be an Argon2id encoded hash" >&2; exit 1 ;;
esac

base64_length() {
  printf '%s' "$1" | openssl base64 -d -A 2>/dev/null | wc -c | tr -d ' '
}
session_length=$(base64_length "$(env_value SESSION_HMAC_KEY)")
encryption_length=$(base64_length "$(env_value ENCRYPTION_MASTER_KEY)")
if [ "$session_length" -lt 32 ]; then
  echo "SESSION_HMAC_KEY must decode to at least 32 bytes" >&2
  exit 1
fi
if [ "$encryption_length" -ne 32 ]; then
  echo "ENCRYPTION_MASTER_KEY must decode to exactly 32 bytes" >&2
  exit 1
fi

line_channel=$(env_value LINE_LOGIN_CHANNEL_ID)
if ! printf '%s' "$line_channel" | grep -Eq '^[0-9]{1,32}$'; then
  echo "LINE_LOGIN_CHANNEL_ID must contain 1 to 32 digits" >&2
  exit 1
fi
line_token=$(env_value LINE_MESSAGING_CHANNEL_ACCESS_TOKEN)
if [ "${#line_token}" -lt 32 ]; then
  echo "LINE_MESSAGING_CHANNEL_ACCESS_TOKEN is too short" >&2
  exit 1
fi
allowed_hosts=$(env_value SML_ALLOWED_HOSTS)
case "$allowed_hosts" in
  *example.com*|*replace*) echo "SML_ALLOWED_HOSTS still contains an example value" >&2; exit 1 ;;
esac

tls_dir=${POSTGRES_TLS_DIR:-"$script_dir/secrets/postgres"}
for file in root.crt server.crt; do
  if [ ! -r "$tls_dir/$file" ]; then
    echo "Missing or unreadable PostgreSQL TLS file: $tls_dir/$file" >&2
    exit 1
  fi
done
if [ ! -f "$tls_dir/server.key" ]; then
  echo "Missing PostgreSQL TLS file: $tls_dir/server.key" >&2
  exit 1
fi
if [ "$(file_mode "$tls_dir/server.key")" != "600" ]; then
  echo "PostgreSQL server.key mode must be 600" >&2
  exit 1
fi
openssl verify -CAfile "$tls_dir/root.crt" "$tls_dir/server.crt" >/dev/null
openssl x509 -in "$tls_dir/server.crt" -noout -checkhost postgres >/dev/null

if [ "$operational_alerts_mode" = send ]; then
  telegram_dir=${TELEGRAM_SECRET_DIR:-"$script_dir/secrets/telegram"}
  for file in bot-token chat-id; do
    path="$telegram_dir/$file"
    if [ ! -r "$path" ]; then echo "Missing protected Telegram secret file: $path" >&2; exit 1; fi
    case "$(file_mode "$path")" in 440) ;; *) echo "Telegram secret $file must be mode 440 for root:65532" >&2; exit 1 ;; esac
    if [ "$(stat -c '%u' "$path" 2>/dev/null || stat -f '%u' "$path")" != 0 ]; then
      echo "Telegram secret $file must be root-owned" >&2
      exit 1
    fi
    if [ "$(stat -c '%g' "$path" 2>/dev/null || stat -f '%g' "$path")" != 65532 ]; then
      echo "Telegram secret $file must use group 65532 for the non-root Sentinel process" >&2
      exit 1
    fi
  done
fi

if [ "$mode" = "full" ]; then
  if docker compose version >/dev/null 2>&1; then
    docker_command='docker'
  elif sudo docker compose version >/dev/null 2>&1; then
    docker_command='sudo docker'
  else
    echo "Docker Compose is unavailable" >&2
    exit 1
  fi
  # shellcheck disable=SC2086
  $docker_command compose --env-file "$env_file" -f "$compose_file" config --quiet
  if command -v ss >/dev/null 2>&1 && ss -ltn | awk '$4 ~ /:6324$/ { found=1 } END { exit !found }'; then
    echo "NOTICE: port 6324 is already listening; confirm it belongs to Nextstep before recreate." >&2
  fi
fi

echo "Production preflight passed ($mode mode)."
