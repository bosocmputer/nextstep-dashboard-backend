#!/bin/sh
set -eu

umask 077
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/compose.production.yml"
action=${1:-}
env_file=${2:-"$script_dir/.env.production"}
duration=${3:-30}
runtime_dir=${SENTINEL_HOST_RUNTIME_DIR:-/run/nextstep-dashboard}
state_file="$runtime_dir/deploy-maintenance.json"
token_file=${UPTIMEROBOT_API_TOKEN_FILE:-"$script_dir/secrets/uptimerobot/api-token"}
monitor_ids_file=${UPTIMEROBOT_MONITOR_IDS_FILE:-"$script_dir/secrets/uptimerobot/monitor-ids.json"}
api_base=https://api.uptimerobot.com/v3

if [ "$action" != open ] && [ "$action" != close ]; then
  echo "Usage: $0 <open|close> [production-env-file] [duration-minutes]" >&2
  exit 1
fi
if ! printf '%s' "$duration" | grep -Eq '^[0-9]+$' || [ "$duration" -lt 15 ] || [ "$duration" -gt 120 ]; then
  echo "Maintenance duration must be between 15 and 120 minutes." >&2
  exit 1
fi
for command in curl jq; do command -v "$command" >/dev/null 2>&1 || { echo "$command is required." >&2; exit 1; }; done

if docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then docker_prefix=; elif sudo -n docker info >/dev/null 2>&1 && sudo -n docker compose version >/dev/null 2>&1; then docker_prefix=sudo; else echo "Docker is unavailable." >&2; exit 1; fi
dc() { if [ -n "$docker_prefix" ]; then sudo docker compose --env-file "$env_file" -f "$compose_file" "$@"; else docker compose --env-file "$env_file" -f "$compose_file" "$@"; fi; }

temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/nextstep-maintenance.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT INT TERM
curl_config="$temporary_dir/curl.conf"
external_available=true
if [ ! -r "$token_file" ] || [ ! -r "$monitor_ids_file" ]; then external_available=false; fi
if [ "$external_available" = true ]; then
  monitor_ids=$(jq -ce 'if type == "array" and length > 0 and all(.[]; type == "number" and . > 0) then . else error("invalid monitor ids") end' "$monitor_ids_file") || external_available=false
  token=$(tr -d '\r\n\t ' < "$token_file")
  if [ -z "$token" ]; then external_available=false; fi
  if [ "$external_available" = true ]; then
    printf 'silent\nshow-error\nfail-with-body\nmax-time = 15\nheader = "Authorization: Bearer %s"\nheader = "Content-Type: application/json"\n' "$token" > "$curl_config"
    chmod 0600 "$curl_config"
    unset token
  fi
fi

external_request() {
  method=$1; path=$2; body_file=${3:-}
  if [ "$external_available" != true ]; then return 1; fi
  if [ -n "$body_file" ]; then curl --config "$curl_config" --request "$method" --data-binary "@$body_file" "$api_base$path"
  else curl --config "$curl_config" --request "$method" "$api_base$path"; fi
}

mkdir -p "$runtime_dir"
if [ "$action" = open ]; then
  if [ -e "$state_file" ]; then echo "A deploy maintenance window is already open." >&2; exit 1; fi
  external_id=
  if [ "$external_available" = true ]; then
    start_date=$(TZ=Asia/Bangkok date +%Y-%m-%d)
    start_time=$(TZ=Asia/Bangkok date +%H:%M:%S)
    body="$temporary_dir/create.json"
    jq -n --arg name "Nextstep Production Deploy" --arg date "$start_date" --arg time "$start_time" --argjson duration "$duration" --argjson monitorIds "$monitor_ids" \
      '{name:$name,autoAddMonitors:false,interval:"once",date:$date,time:$time,duration:$duration,monitorIds:$monitorIds}' > "$body"
    response="$temporary_dir/response.json"
    if external_request POST /maintenance-windows "$body" > "$response"; then external_id=$(jq -er '.id | select(type == "number" and . > 0)' "$response" 2>/dev/null || true); fi
  fi
  if [ -z "$external_id" ] && [ "${NEXTSTEP_MAINTENANCE_OVERRIDE:-false}" != true ]; then
    echo "External UptimeRobot maintenance could not be created; deployment must stop before mutation." >&2
    exit 1
  fi

  internal_id=$(printf "insert into operational_maintenance_windows (source, status, starts_at, ends_at, safe_reason) values ('DEPLOY','ACTIVE',now(),now() + make_interval(mins => %s),'PLANNED_PRODUCTION_DEPLOY') returning id;" "$duration" | \
    dc exec -T postgres sh -ceu 'exec psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --quiet --tuples-only --no-align' | tr -d '[:space:]')
  if ! printf '%s' "$internal_id" | grep -Eq '^[0-9a-f-]{36}$'; then
    if [ -n "$external_id" ]; then external_request DELETE "/maintenance-windows/$external_id" >/dev/null 2>&1 || true; fi
    echo "Internal maintenance window could not be created." >&2
    exit 1
  fi
  override=false
  if [ -z "$external_id" ]; then
    override=true
    external_id=0
    printf '%s PLANNED_PRODUCTION_DEPLOY external-maintenance-override\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$runtime_dir/maintenance-overrides.log"
    chmod 0600 "$runtime_dir/maintenance-overrides.log"
  fi
  jq -n --arg internalId "$internal_id" --argjson externalId "$external_id" --argjson override "$override" \
    '{version:1,internalId:$internalId,externalId:$externalId,override:$override}' > "$state_file.tmp.$$"
  chmod 0600 "$state_file.tmp.$$"
  mv "$state_file.tmp.$$" "$state_file"
  echo "Internal and external deployment maintenance are active (override=$override)."
  exit 0
fi

if [ ! -r "$state_file" ]; then echo "No deploy maintenance state was found." >&2; exit 1; fi
internal_id=$(jq -er '.internalId | select(type == "string")' "$state_file")
external_id=$(jq -er '.externalId | select(type == "number")' "$state_file")
printf "update operational_maintenance_windows set status='COMPLETED', updated_at=now() where id='%s' and status='ACTIVE';" "$internal_id" | \
  dc exec -T postgres sh -ceu 'exec psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --quiet' >/dev/null
if [ "$external_id" -gt 0 ]; then
  if ! external_request DELETE "/maintenance-windows/$external_id" >/dev/null; then
    echo "Internal maintenance closed but UptimeRobot maintenance deletion failed; inspect it manually." >&2
    exit 1
  fi
fi
rm -f "$state_file"
echo "Deployment maintenance closed."
