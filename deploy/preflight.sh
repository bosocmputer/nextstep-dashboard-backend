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

required_keys='DASHBOARD_DOMAIN BACKEND_SHA FRONTEND_SHA FRONTEND_BIND_ADDRESS POSTGRES_DB POSTGRES_USER POSTGRES_PASSWORD DATABASE_URL ADMIN_USERNAME ADMIN_PASSWORD_HASH SESSION_HMAC_KEY ENCRYPTION_MASTER_KEY ENCRYPTION_KEY_ID SML_ALLOWED_CIDRS SML_ALLOWED_HOSTS LINE_LOGIN_CHANNEL_ID LINE_MESSAGING_CHANNEL_ACCESS_TOKEN'
for key in $required_keys; do
  require_value "$key"
done

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
