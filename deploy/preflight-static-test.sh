#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
temporary=$(mktemp -d "${TMPDIR:-/tmp}/nextstep-preflight.XXXXXX")
trap 'rm -rf "$temporary"' EXIT INT TERM
tls_dir="$temporary/tls"
env_file="$temporary/production.env"
bad_env_file="$temporary/production-bad.env"

chmod_line=$(awk '/chmod 600 .*server\.key/ { print NR; exit }' "$script_dir/generate-postgres-tls.sh")
chown_line=$(awk '/chown .*server\.key/ { print NR; exit }' "$script_dir/generate-postgres-tls.sh")
if [ -z "$chmod_line" ] || [ -z "$chown_line" ] || [ "$chmod_line" -ge "$chown_line" ]; then
  echo "PostgreSQL TLS key permissions must be set before ownership is transferred" >&2
  exit 1
fi

POSTGRES_TLS_DIR="$tls_dir" POSTGRES_UID=$(id -u) "$script_dir/generate-postgres-tls.sh" >/dev/null
cat > "$env_file" <<'EOF'
DASHBOARD_DOMAIN=dashboard.nextstep-soft.com
BACKEND_SHA=1111111111111111111111111111111111111111
FRONTEND_SHA=2222222222222222222222222222222222222222
FRONTEND_BIND_ADDRESS=127.0.0.1
POSTGRES_DB=nextstep
POSTGRES_USER=nextstep
POSTGRES_PASSWORD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
DATABASE_URL=postgres://nextstep:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa@postgres:5432/nextstep?sslmode=verify-full&sslrootcert=/run/secrets/postgres-root.crt
DATABASE_MAX_CONNECTIONS=20
DATABASE_MIN_CONNECTIONS=2
ADMIN_USERNAME=superadmin
ADMIN_PASSWORD_HASH=$argon2id$v=19$m=65536,t=3,p=2$fake$fake
SESSION_HMAC_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
ENCRYPTION_MASTER_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
ENCRYPTION_KEY_ID=test-key
SML_ALLOWED_CIDRS=10.0.0.0/8
SML_ALLOWED_HOSTS=sml.internal.local
REPORT_WORKER_CONCURRENCY=4
DELIVERY_WORKER_CONCURRENCY=4
LINE_LOGIN_CHANNEL_ID=2010662588
LINE_MESSAGING_CHANNEL_ACCESS_TOKEN=fake-token-value-that-is-long-enough-for-static-test
EOF
chmod 600 "$env_file"
POSTGRES_TLS_DIR="$tls_dir" "$script_dir/preflight.sh" "$env_file" static >/dev/null

sed 's/SML_ALLOWED_HOSTS=sml.internal.local/SML_ALLOWED_HOSTS=sml-shop.example.com/' "$env_file" > "$bad_env_file"
chmod 600 "$bad_env_file"
if POSTGRES_TLS_DIR="$tls_dir" "$script_dir/preflight.sh" "$bad_env_file" static >/dev/null 2>&1; then
  echo "Preflight accepted an example SML hostname" >&2
  exit 1
fi

echo "Static production preflight tests passed."
