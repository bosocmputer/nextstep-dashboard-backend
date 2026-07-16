#!/bin/sh
set -eu

umask 077
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/compose.production.yml"
env_file=${1:-"$script_dir/.env.production"}
backup_file=${2:-}
runtime_dir=${SENTINEL_HOST_RUNTIME_DIR:-/run/nextstep-dashboard}

if [ ! -r "$env_file" ] || [ -z "$backup_file" ] || [ ! -r "$backup_file" ]; then
  echo "Usage: $0 <production-env-file> <backup.dump>" >&2
  exit 1
fi
checksum_file="$backup_file.sha256"
if [ ! -r "$checksum_file" ]; then
  echo "Restore drill requires the backup checksum sidecar." >&2
  exit 1
fi
(cd "$(dirname -- "$backup_file")" && sha256sum -c "$(basename -- "$checksum_file")")

env_value() {
  awk -v wanted="$1" 'index($0, wanted "=") == 1 { print substr($0, length(wanted) + 2); exit }' "$env_file"
}
# The disposable database never reuses Production credentials. The dump is
# owner/ACL-free, so an isolated role is sufficient for verification.
postgres_user=restorecheck
postgres_password=$(openssl rand -hex 32)

if docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  docker_prefix=
elif sudo -n docker info >/dev/null 2>&1 && sudo -n docker compose version >/dev/null 2>&1; then
  docker_prefix=sudo
else
  echo "Docker daemon is unavailable to the current user and passwordless sudo." >&2
  exit 1
fi
d() { if [ -n "$docker_prefix" ]; then sudo docker "$@"; else docker "$@"; fi; }
dc() { if [ -n "$docker_prefix" ]; then sudo docker compose --env-file "$env_file" -f "$compose_file" "$@"; else docker compose --env-file "$env_file" -f "$compose_file" "$@"; fi; }

busy=$(printf '%s\n' "select exists(
  select 1 from report_runs where status in ('CLAIMED', 'RUNNING')
  union all select 1 from notification_runs where status in ('COLLECTING', 'READY', 'SENDING')
  union all select 1 from notification_schedules where status = 'ACTIVE' and next_run_at <= now() + interval '30 minutes'
);" | dc exec -T postgres sh -ceu 'exec psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --tuples-only --no-align' | tr -d '[:space:]')
if [ "$busy" != "f" ]; then
  echo "Restore drill deferred: report/notification work or a schedule is due within 30 minutes." >&2
  exit 75
fi

suffix=$(date -u +%Y%m%d%H%M%S)-$$
container="nextstep-restore-${suffix}"
volume="nextstep-restore-${suffix}"
network="nextstep-restore-${suffix}"
cleanup() {
  d rm -f "$container" >/dev/null 2>&1 || true
  d volume rm "$volume" >/dev/null 2>&1 || true
  d network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

d network create --internal "$network" >/dev/null
d volume create "$volume" >/dev/null
d run -d --name "$container" --network "$network" --memory 512m --cpus 0.50 --pids-limit 128 \
  -e POSTGRES_USER="$postgres_user" -e POSTGRES_PASSWORD="$postgres_password" -e POSTGRES_DB=restorecheck \
  -v "$volume:/var/lib/postgresql/data" postgres:16-alpine >/dev/null

ready=false
for _ in $(seq 1 60); do
  if d exec "$container" pg_isready -U "$postgres_user" -d restorecheck >/dev/null 2>&1; then ready=true; break; fi
  sleep 1
done
if [ "$ready" != true ]; then
  echo "Isolated restore PostgreSQL did not become ready." >&2
  exit 1
fi

d exec -i "$container" pg_restore --username "$postgres_user" --dbname restorecheck --exit-on-error --no-owner --no-acl < "$backup_file"
validated=$(d exec "$container" psql --username "$postgres_user" --dbname restorecheck --tuples-only --no-align --command \
  "select (count(*) = 10 and (select max(version) from schema_migrations) >= 23) from report_definitions;" | tr -d '[:space:]')
if [ "$validated" != "t" ]; then
  echo "Isolated restore validation failed." >&2
  exit 1
fi

install -d -o root -g root -m 0755 "$runtime_dir" "$runtime_dir/host"
marker="$runtime_dir/host/restore-verified-at"
temporary="$marker.tmp.$$"
date -u +%s > "$temporary"
chmod 0600 "$temporary"
mv "$temporary" "$marker"
echo "Isolated restore drill passed; Production PostgreSQL was read only for the busy check."
