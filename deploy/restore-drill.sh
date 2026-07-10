#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
compose_file="$script_dir/compose.production.yml"
env_file=${1:-"$script_dir/.env.production"}
backup_file=${2:-}

if [ ! -r "$env_file" ] || [ -z "$backup_file" ] || [ ! -r "$backup_file" ]; then
  echo "Usage: $0 <production-env-file> <backup.dump>" >&2
  exit 1
fi

checksum_file="$backup_file.sha256"
if [ -r "$checksum_file" ]; then
  (cd "$(dirname -- "$backup_file")" && sha256sum -c "$(basename -- "$checksum_file")")
fi

if docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  use_sudo=false
elif sudo -n docker info >/dev/null 2>&1 && sudo -n docker compose version >/dev/null 2>&1; then
  use_sudo=true
else
  echo "Docker daemon is unavailable to the current user and passwordless sudo." >&2
  exit 1
fi

dc() {
  if [ "$use_sudo" = true ]; then
    sudo docker compose --env-file "$env_file" -f "$compose_file" "$@"
  else
    docker compose --env-file "$env_file" -f "$compose_file" "$@"
  fi
}

drill_db="nextstep_restore_$(date -u +%Y%m%d%H%M%S)"
cleanup() {
  dc exec -T postgres sh -ceu 'dropdb --if-exists --username "$POSTGRES_USER" "$1"' sh "$drill_db" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

dc exec -T postgres sh -ceu 'createdb --username "$POSTGRES_USER" "$1"' sh "$drill_db"
dc exec -T postgres sh -ceu 'exec pg_restore --username "$POSTGRES_USER" --dbname "$1" --exit-on-error --no-owner --no-acl' sh "$drill_db" < "$backup_file"
validated=$(dc exec -T postgres sh -ceu 'psql --username "$POSTGRES_USER" --dbname "$1" --tuples-only --no-align --command "select (count(*) = 10 and (select max(version) from schema_migrations) >= 6) from report_definitions"' sh "$drill_db")

if [ "$validated" != "t" ]; then
  echo "Restore validation failed." >&2
  exit 1
fi

echo "Restore drill passed for $backup_file"
