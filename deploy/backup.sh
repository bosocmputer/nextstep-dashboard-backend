#!/bin/sh
set -eu

umask 077
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
compose_file="$script_dir/compose.production.yml"
env_file=${1:-"$script_dir/.env.production"}
backup_dir=${BACKUP_DIR:-"$project_dir/backups"}

if [ ! -r "$env_file" ]; then
  echo "Production environment file is not readable: $env_file" >&2
  exit 1
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

mkdir -p "$backup_dir"
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
filename="nextstep-${timestamp}.dump"
target="$backup_dir/$filename"
temporary="$target.tmp"
trap 'rm -f "$temporary"' EXIT

dc exec -T postgres sh -ceu 'exec pg_dump --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" --format=custom --compress=6 --no-owner --no-acl' > "$temporary"
test -s "$temporary"
mv "$temporary" "$target"
(cd "$backup_dir" && sha256sum "$filename" > "$filename.sha256")

echo "Backup completed: $target"
