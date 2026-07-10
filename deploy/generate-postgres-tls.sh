#!/bin/sh
set -eu

umask 077
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
target_dir=${POSTGRES_TLS_DIR:-"$script_dir/secrets/postgres"}
target_parent=$(dirname -- "$target_dir")

if [ -e "$target_dir/server.key" ] || [ -e "$target_dir/server.crt" ] || [ -e "$target_dir/root.crt" ]; then
  echo "PostgreSQL TLS material already exists; refusing to overwrite it." >&2
  exit 1
fi

mkdir -p "$target_dir"
trap 'rm -f "$target_dir/ca.key" "$target_dir/server.csr" "$target_dir/server-ext.cnf" "$target_dir/root.srl"' EXIT

openssl genrsa -out "$target_dir/ca.key" 4096
openssl req -x509 -new -sha256 -days 3650 \
  -key "$target_dir/ca.key" \
  -subj "/CN=Nextstep Dashboard PostgreSQL CA" \
  -out "$target_dir/root.crt"
openssl genrsa -out "$target_dir/server.key" 3072
openssl req -new -sha256 \
  -key "$target_dir/server.key" \
  -subj "/CN=postgres" \
  -out "$target_dir/server.csr"

cat > "$target_dir/server-ext.cnf" <<'EOF'
subjectAltName=DNS:postgres,DNS:nextstep-postgres
extendedKeyUsage=serverAuth
keyUsage=digitalSignature,keyEncipherment
EOF

openssl x509 -req -sha256 -days 825 \
  -in "$target_dir/server.csr" \
  -CA "$target_dir/root.crt" \
  -CAkey "$target_dir/ca.key" \
  -CAcreateserial \
  -extfile "$target_dir/server-ext.cnf" \
  -out "$target_dir/server.crt"
postgres_uid=${POSTGRES_UID:-}
if [ -z "$postgres_uid" ] && docker version >/dev/null 2>&1; then
  postgres_uid=$(docker run --rm postgres:16-alpine id -u postgres)
elif [ -z "$postgres_uid" ]; then
  postgres_uid=$(sudo docker run --rm postgres:16-alpine id -u postgres)
fi
if [ "$(id -u)" -eq 0 ]; then
  chown "$postgres_uid:$postgres_uid" "$target_dir/server.key" "$target_dir/server.crt"
elif [ "$(id -u)" -ne "$postgres_uid" ]; then
  sudo chown "$postgres_uid:$postgres_uid" "$target_dir/server.key" "$target_dir/server.crt"
fi
chmod 600 "$target_dir/server.key"
chmod 644 "$target_dir/server.crt" "$target_dir/root.crt"
chmod 755 "$target_parent" "$target_dir"

echo "PostgreSQL TLS material created in $target_dir"
