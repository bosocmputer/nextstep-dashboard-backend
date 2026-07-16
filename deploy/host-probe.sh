#!/bin/sh
set -eu

umask 077
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
project_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
env_file=${1:-"$script_dir/.env.production"}
runtime_dir=${SENTINEL_HOST_RUNTIME_DIR:-/run/nextstep-dashboard}
backup_dir=${BACKUP_DIR:-"$project_dir/backups"}

if [ ! -r "$env_file" ]; then
  echo "Production environment file is not readable: $env_file" >&2
  exit 1
fi
if docker info >/dev/null 2>&1; then docker_prefix=; elif sudo -n docker info >/dev/null 2>&1; then docker_prefix=sudo; else docker_prefix=unavailable; fi

container_ok() {
  service=$1
  if [ "$docker_prefix" = unavailable ]; then printf false; return; fi
  if [ -n "$docker_prefix" ]; then
    container_id=$(sudo docker ps -aq --filter label=com.docker.compose.project=nextstep-dashboard --filter "label=com.docker.compose.service=$service" | head -1)
  else
    container_id=$(docker ps -aq --filter label=com.docker.compose.project=nextstep-dashboard --filter "label=com.docker.compose.service=$service" | head -1)
  fi
  if [ -z "$container_id" ]; then printf false; return; fi
  if [ -n "$docker_prefix" ]; then state=$(sudo docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id" 2>/dev/null || true)
  else state=$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container_id" 2>/dev/null || true); fi
  case "$state" in 'running healthy'|'running none') printf true ;; *) printf false ;; esac
}

disk_used=$(df -P "$project_dir" | awk 'NR == 2 { gsub(/%/, "", $5); print $5 + 0 }')
inode_used=$(df -Pi "$project_dir" | awk 'NR == 2 { gsub(/%/, "", $5); print $5 + 0 }')
memory_available=$(awk '/MemTotal:/ { total=$2 } /MemAvailable:/ { available=$2 } END { if (total > 0) printf "%.2f", available * 100 / total; else print "0" }' /proc/meminfo)
ntp=false
if command -v timedatectl >/dev/null 2>&1 && [ "$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)" = yes ]; then ntp=true; fi

install -d -o root -g root -m 0755 "$runtime_dir" "$runtime_dir/host"
install -d -o 65532 -g 65532 -m 0750 "$runtime_dir/monitor"
critical_file="$runtime_dir/host/memory-critical-since"
memory_integer=${memory_available%.*}
if [ "$memory_integer" -le 5 ]; then
  if [ ! -s "$critical_file" ]; then date -u +%s > "$critical_file.tmp.$$"; chmod 0600 "$critical_file.tmp.$$"; mv "$critical_file.tmp.$$" "$critical_file"; fi
else
  rm -f "$critical_file"
fi

json_time_or_null() {
  epoch=$1
  if [ -n "$epoch" ] && [ "$epoch" -gt 0 ] 2>/dev/null; then printf '"%s"' "$(date -u -d "@$epoch" +%Y-%m-%dT%H:%M:%SZ)"; else printf null; fi
}

memory_critical_epoch=
if [ -s "$critical_file" ]; then memory_critical_epoch=$(tr -cd '0-9' < "$critical_file"); fi
latest_backup=$(find "$backup_dir" -maxdepth 1 -type f -name 'nextstep-*.dump' -printf '%T@ %p\n' 2>/dev/null | sort -nr | head -1 | cut -d' ' -f2-)
backup_epoch=
checksum_valid=false
if [ -n "$latest_backup" ] && [ -f "$latest_backup.sha256" ]; then
  backup_epoch=$(stat -c %Y "$latest_backup")
  if (cd "$(dirname -- "$latest_backup")" && sha256sum -c "$(basename -- "$latest_backup.sha256")" >/dev/null 2>&1); then checksum_valid=true; fi
fi
restore_epoch=
if [ -s "$runtime_dir/host/restore-verified-at" ]; then restore_epoch=$(tr -cd '0-9' < "$runtime_dir/host/restore-verified-at"); fi
offsite=false
if awk -F= '$1 == "OFFSITE_BACKUP_CONFIGURED" && $2 == "true" { found=1 } END { exit !found }' "$env_file"; then offsite=true; fi

checked_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
target="$runtime_dir/host/host-probe.json"
temporary="$target.tmp.$$"
printf '{"version":1,"checkedAt":"%s","containers":{"api":%s,"worker":%s,"frontend":%s,"postgres":%s,"sentinel":%s},"diskUsedPercent":%s,"inodeUsedPercent":%s,"memoryAvailablePercent":%s,"memoryCriticalSince":%s,"ntpSynchronized":%s,"backup":{"lastSuccessAt":%s,"checksumValid":%s,"restoreVerifiedAt":%s,"offsiteConfigured":%s}}\n' \
  "$checked_at" "$(container_ok api)" "$(container_ok worker)" "$(container_ok frontend)" "$(container_ok postgres)" "$(container_ok sentinel)" \
  "$disk_used" "$inode_used" "$memory_available" "$(json_time_or_null "$memory_critical_epoch")" "$ntp" \
  "$(json_time_or_null "$backup_epoch")" "$checksum_valid" "$(json_time_or_null "$restore_epoch")" "$offsite" > "$temporary"
if [ "$(wc -c < "$temporary")" -gt 16384 ]; then rm -f "$temporary"; echo "Host probe exceeded 16 KB." >&2; exit 1; fi
chmod 0644 "$temporary"
mv "$temporary" "$target"
