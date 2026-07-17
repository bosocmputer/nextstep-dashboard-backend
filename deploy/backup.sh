#!/bin/sh
set -eu

umask 077
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
compose_file="$script_dir/compose.production.yml"
env_file=${1:-"$script_dir/.env.production"}
backup_dir=${BACKUP_DIR:-"$project_dir/backups"}
lock_file=${BACKUP_LOCK_FILE:-"$backup_dir/.backup.lock"}

if [ ! -r "$env_file" ]; then
  echo "Production environment file is not readable: $env_file" >&2
  exit 1
fi
backup_policy=$(sed -n 's/^BACKUP_POLICY=//p' "$env_file" | tail -1)
if [ "${backup_policy:-PRE_MIGRATION_ONLY}" != "PRE_MIGRATION_ONLY" ]; then
  echo "backup.sh is reserved for PRE_MIGRATION_ONLY releases." >&2
  exit 1
fi
if ! command -v flock >/dev/null 2>&1 || ! command -v timeout >/dev/null 2>&1; then
  echo "flock and timeout are required for safe backups." >&2
  exit 1
fi

if docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  docker_prefix=
elif sudo -n docker info >/dev/null 2>&1 && sudo -n docker compose version >/dev/null 2>&1; then
  docker_prefix=sudo
else
  echo "Docker daemon is unavailable to the current user and passwordless sudo." >&2
  exit 1
fi

mkdir -p "$backup_dir"
exec 9>"$lock_file"
if ! flock -n 9; then
  echo "Another backup is already running." >&2
  exit 75
fi

latest_size=$(find "$backup_dir" -maxdepth 1 -type f -name 'nextstep-*.dump' -printf '%s\n' 2>/dev/null | sort -nr | head -1)
latest_size=${latest_size:-0}
required_bytes=$((latest_size * 2))
minimum_bytes=$((5 * 1024 * 1024 * 1024))
if [ "$required_bytes" -lt "$minimum_bytes" ]; then required_bytes=$minimum_bytes; fi
available_kb=$(df -Pk "$backup_dir" | awk 'NR == 2 { print $4 }')
available_bytes=$((available_kb * 1024))
if [ "$available_bytes" -le "$required_bytes" ]; then
  echo "Insufficient free space for a verified backup." >&2
  exit 1
fi

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
filename="nextstep-pre-migration-${timestamp}.dump"
target="$backup_dir/$filename"
temporary="$target.tmp"
checksum_temporary="$target.sha256.tmp"
trap 'rm -f "$temporary" "$checksum_temporary"' EXIT INT TERM

if [ -n "$docker_prefix" ]; then
  timeout 30m sudo docker compose --env-file "$env_file" -f "$compose_file" exec -T postgres \
    sh -ceu 'exec pg_dump --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --format=custom --compress=6 --no-owner --no-acl' > "$temporary"
else
  timeout 30m docker compose --env-file "$env_file" -f "$compose_file" exec -T postgres \
    sh -ceu 'exec pg_dump --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --format=custom --compress=6 --no-owner --no-acl' > "$temporary"
fi
test -s "$temporary"
(cd "$backup_dir" && sha256sum "$(basename -- "$temporary")" | sed 's/\.tmp$//' > "$checksum_temporary")
mv "$temporary" "$target"
mv "$checksum_temporary" "$target.sha256"
(cd "$backup_dir" && sha256sum -c "$(basename -- "$target.sha256")" >/dev/null)

# Retention happens only after a new backup and checksum have both succeeded.
# Keep at most the two most recent verified pre-migration sets.
find "$backup_dir" -maxdepth 1 -type f -name 'nextstep-pre-migration-*.dump' -printf '%T@ %p\n' | sort -nr | tail -n +3 | cut -d' ' -f2- | while IFS= read -r old; do
  [ -n "$old" ] || continue
  rm -f -- "$old" "$old.sha256"
done

echo "Backup completed and checksum verified: $target"
