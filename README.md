# Nextstep Dashboard Backend

Go API and distributed workers for the multi-tenant Nextstep Dashboard.

## Local verification

```bash
make verify
```

The API refuses to start until all required configuration is present. Copy
`.env.example` to a local untracked `.env`, then supply an Argon2id admin hash
and independently generated base64 keys. Never place plaintext credentials in
the repository.

Generate the admin hash without exposing the password on the command line:

```bash
make hash-password
```

## API contract

The backend owns `api/openapi.yaml`. Frontend API code is generated from that
file and pins its contract version; handwritten duplicate API types are not
allowed.

## Runtime commands

- `go run ./cmd/api` starts the HTTP API.
- `go run ./cmd/migrate` applies embedded, checksummed migrations.
- `go test ./...` runs unit and contract tests.
- `go vet ./...` runs static checks.

Health endpoints:

- `GET /api/v1/health/live`
- `GET /api/v1/health/ready`

## Production deployment

The reference deployment in `deploy/compose.production.yml` runs one migration
job, a stateless API, a distributed worker, and the static frontend on host port
`6324`. HTTPS terminates at the existing Cloudflare Tunnel or IT-managed reverse
proxy for `dashboard.nextstep-soft.com`. PostgreSQL 16 stays private on the
Compose network with a persistent volume, verified TLS, and tested backup/
restore scripts; neither PostgreSQL nor the backend API publishes a host port.

See `deploy/RUNBOOK.md` for the release, rollback, health, capacity, retention,
and key-rotation procedures.
