#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
env_file=${1:-"$script_dir/.env.production"}
compose_file="$script_dir/compose.production.yml"

if docker info >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then docker_prefix=; elif sudo -n docker info >/dev/null 2>&1 && sudo -n docker compose version >/dev/null 2>&1; then docker_prefix=sudo; else echo "Docker is unavailable." >&2; exit 1; fi
dc() { if [ -n "$docker_prefix" ]; then sudo docker compose --env-file "$env_file" -f "$compose_file" "$@"; else docker compose --env-file "$env_file" -f "$compose_file" "$@"; fi; }

"$script_dir/preflight.sh" "$env_file" full
"$script_dir/maintenance-window.sh" open "$env_file" 30
maintenance_open=true
close_maintenance() {
  if [ "${maintenance_open:-false}" = true ]; then "$script_dir/maintenance-window.sh" close "$env_file" 30 || true; fi
}
trap close_maintenance EXIT INT TERM

"$script_dir/backup.sh" "$env_file"
dc pull
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
