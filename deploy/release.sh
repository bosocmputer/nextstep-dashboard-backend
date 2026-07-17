#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
env_file=${1:-"$script_dir/.env.production"}
compose_file="$script_dir/compose.production.yml"
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
backup_dir=${BACKUP_DIR:-"$project_dir/backups"}

if docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then docker_prefix=; elif sudo -n docker info >/dev/null 2>&1 && sudo -n docker compose version >/dev/null 2>&1; then docker_prefix=sudo; else echo "Docker is unavailable." >&2; exit 1; fi
dc() { if [ -n "$docker_prefix" ]; then sudo docker compose --env-file "$env_file" -f "$compose_file" "$@"; else docker compose --env-file "$env_file" -f "$compose_file" "$@"; fi; }

"$script_dir/preflight.sh" "$env_file" full
if command -v systemctl >/dev/null 2>&1; then
  systemctl_run() { if [ "$(id -u)" -eq 0 ]; then systemctl "$@"; else sudo -n systemctl "$@"; fi; }
  if systemctl is-enabled nextstep-dashboard-backup.timer >/dev/null 2>&1; then systemctl_run disable --now nextstep-dashboard-backup.timer; fi
  if systemctl is-enabled nextstep-dashboard-restore-drill.timer >/dev/null 2>&1; then systemctl_run disable --now nextstep-dashboard-restore-drill.timer; fi
fi
"$script_dir/maintenance-window.sh" open "$env_file" 30
maintenance_open=true
close_maintenance() {
  if [ "${maintenance_open:-false}" = true ]; then "$script_dir/maintenance-window.sh" close "$env_file" 30 || true; fi
}
trap close_maintenance EXIT INT TERM

dc pull
pending=$(dc run --rm --no-deps migrate /app/migrate --pending | tail -1 | tr -cd '0-9')
case "$pending" in ''|*[!0-9]*) echo "Unable to determine pending migrations." >&2; exit 1;; esac
if [ "$pending" -gt 0 ]; then
  "$script_dir/backup.sh" "$env_file"
  latest=$(find "$backup_dir" -maxdepth 1 -type f -name 'nextstep-pre-migration-*.dump' -printf '%T@ %p\n' | sort -nr | head -1 | cut -d' ' -f2-)
  [ -n "$latest" ] || { echo "Verified pre-migration backup was not found." >&2; exit 1; }
  "$script_dir/restore-drill.sh" "$env_file" "$latest"
fi
dc up -d

for endpoint in live ready watchdog; do
  attempts=0
  until curl -fsS --max-time 5 "https://dashboard.nextstep-soft.com/api/v1/health/$endpoint" >/dev/null; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 30 ]; then echo "Health endpoint failed after deployment: $endpoint" >&2; exit 1; fi
    sleep 2
  done
done

"$script_dir/maintenance-window.sh" close "$env_file" 30
maintenance_open=false
echo "Production release completed with maintenance and health verification."
