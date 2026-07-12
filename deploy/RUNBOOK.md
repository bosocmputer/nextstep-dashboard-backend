# Nextstep Dashboard production runbook

## Preconditions

- DNS for `dashboard.nextstep-soft.com` is attached to a Cloudflare Tunnel or an
  IT-managed reverse proxy whose origin is `http://127.0.0.1:6324` on the app
  host. If the proxy is on another host, set `FRONTEND_BIND_ADDRESS=0.0.0.0`,
  route it to `http://10.121.20.83:6324`, and firewall the port to that proxy.
- PostgreSQL runs only on the private Compose network with a persistent named
  volume and verified TLS. It must never publish a host port.
- The LINE Login channel and central Messaging API channel are production
  channels. The LIFF endpoint is `https://dashboard.nextstep-soft.com/app`.
- SML endpoints are private addresses inside `SML_ALLOWED_CIDRS` and are
  reachable from the worker network.
- Secrets come from the deployment secret store. Never commit the populated env
  file or print `docker compose config` in shared CI logs.
- Authenticate the host to GHCR with a read-only package token before `pull`;
  never store that token in the Compose env file.

Generate independent key material:

```bash
openssl rand -base64 32 # SESSION_HMAC_KEY
openssl rand -base64 32 # ENCRYPTION_MASTER_KEY
```

Generate `ADMIN_PASSWORD_HASH` with `make hash-password`; it reads and confirms
the password with terminal echo disabled. Store the generated value inside
literal single quotes in the production env file, for example
`ADMIN_PASSWORD_HASH='<generated Argon2id hash>'`, so Docker Compose preserves
the hash's dollar signs. Do not place the plaintext password in shell history.

For the first deployment, generate PostgreSQL TLS material before validating
Compose. The script refuses to overwrite existing keys and deletes the signing
CA private key after issuing the server certificate:

```bash
./deploy/generate-postgres-tls.sh
```

Generate `POSTGRES_PASSWORD` with `openssl rand -hex 32`, then place that same
URL-safe value in `DATABASE_URL`. Keep the TLS hostname as `postgres` and use
`sslmode=verify-full&sslrootcert=/run/secrets/postgres-root.crt`.

## Backup and restore

Create an owner-only custom-format backup before every migration and at least
daily:

```bash
./deploy/backup.sh ./deploy/.env.production
```

The script writes a SHA-256 sidecar under `backups/`. Copy backups to encrypted
off-host storage; a backup on the same disk is not disaster recovery. At least
monthly, and before the first schedule is activated, restore the newest backup
into a temporary database and validate the catalog/migration ledger:

```bash
./deploy/restore-drill.sh ./deploy/.env.production ./backups/<backup>.dump
```

The drill always drops its temporary database on exit and never restores over
the production database.

## Release

1. CI builds immutable GHCR tags from reviewed commits. Record the independent
   frontend and backend commit SHAs as one release pair.
2. Copy `.env.production.example` to an owner-readable deployment env file and
   populate it from the secret store.
3. Validate the owner-only env file, paired SHAs, PostgreSQL certificates,
   domain, SML allowlist and effective Compose model without printing secrets:

   ```bash
   ./deploy/preflight.sh ./deploy/.env.production full
   ```

4. Run `backup.sh` and verify the most recent `restore-drill.sh` result.
5. Run the report-management data preflight against the current database. It
   prints affected rows and exits non-zero if an active schedule has a recipient
   missing any selected report permission. Resolve every row manually; do not
   auto-pause schedules:

   ```bash
   sudo docker compose --env-file <env> -f deploy/compose.production.yml \
     exec -T postgres sh -ceu 'exec psql --username "$POSTGRES_USER" --dbname "$POSTGRES_DB"' \
     < deploy/report-management-preflight.sql
   ```

6. Deploy and verify the backend API and worker image first while keeping the
   prior frontend SHA. Confirm the admin report catalog, name-only tenant create
   compatibility, schedule readiness, and LINE Flex validation without sending
   a message. Then deploy the new frontend SHA.
7. Run `sudo docker compose --env-file <env> -f deploy/compose.production.yml
   pull`, then `sudo docker compose --env-file <env> -f
   deploy/compose.production.yml up -d`. The API and worker start only after the
   checksummed migration job succeeds.
8. Verify `/api/v1/health/live` and `/api/v1/health/ready`, admin login, one SML
   connection test, a fresh viewer report run, and a LINE delivery to a test
   recipient.
   Before enabling a new tenant, run `/app/sml-smoke` with
   `SML_SMOKE_TENANT_ID`, `SML_SMOKE_DATE_FROM`, and `SML_SMOKE_DATE_TO`. It
   executes only the approved report SQL and returns at most one sample row per
   step; logs contain status/count only, never customer row values.
9. Watch JSON logs, failed report runs, delivery retries, quota responses, and
   worker heartbeat freshness for at least 30 minutes.

Do not activate a schedule until the tenant SML connection is READY, at least
one verified recipient has permission for every selected report, and a manual
test delivery succeeds.

## Rollback

- Application rollback: restore both `BACKEND_SHA` and `FRONTEND_SHA` from the
  prior recorded release pair, pull, and recreate the services. Never mix an
  unverified pair or reuse a tag for different bytes.
- Frontend rollback remains safe while the new backend and worker stay running.
  Do not roll the backend/worker back to a version limited to five reports until
  the rollback-guard query in `report-management-preflight.sql` returns no
  schedules with more than five reports.
- Database rollback: migrations are forward-only. If a schema change cannot
  remain backward-compatible, stop API/worker writers and restore the verified
  pre-release dump into a replacement database volume; never improvise a down
  migration against the only production copy.
- LINE incident: pause affected schedules first. Keep delivery outbox records so
  their persisted retry keys remain stable; do not manually replay with new
  keys unless LINE has conclusively rejected the original request.

## Capacity and performance

- Count pool capacity across every process: `(API replicas + worker replicas) ×
  DATABASE_MAX_CONNECTIONS` must remain below the database connection budget
  after reserving capacity for migrations and operators.
- Start with one worker, `REPORT_WORKER_CONCURRENCY=4`, and
  `DELIVERY_WORKER_CONCURRENCY=4`. Increase only after measuring SML latency,
  worker memory, PostgreSQL wait time, LINE 429 responses, and queue age.
- Dashboard detail rows are paginated and expire after 24 hours. Summary
  snapshots are versioned by report definition and SML connection and may be
  reused for their configured freshness window. Historical summaries remain
  explicitly timestamped and readable for up to 90 days; expired detail rows
  still require a new SML query when the user asks for row-level detail.
- Snapshot-first rollout is feature-gated. Start with
  `SNAPSHOT_FIRST_ENABLED=true` plus an explicit
  `SNAPSHOT_FIRST_TENANT_IDS` allowlist, then observe cache hit ratio, queue age,
  SML timeouts, and schedule latency before expanding the allowlist.

## Retention and privacy checks

- Report row payloads: 24 hours.
- Scheduled summary/outbox payloads: scrubbed after 90 days.
- Delivery metadata and redacted audit events: deleted after 365 days.
- Verify the hourly retention heartbeat and deletion counters. A missing
  heartbeat is an alert; do not assume retention is working because the service
  is otherwise healthy.
- Logs and admin screens must not contain SML credentials, LINE access tokens,
  raw LINE user IDs, invitation references, delivery references, or message
  payloads.

## Secret and encryption-key rotation

- Rotate the LINE access token in the secret store and recreate API/worker.
- Rotating `SESSION_HMAC_KEY` invalidates all admin/viewer sessions and signed
  links; perform it in an announced maintenance window.
- For `ENCRYPTION_MASTER_KEY`, deploy application support for both old and new
  key IDs, re-encrypt all SML/recipient secrets, verify zero records remain on
  the old key, then remove it. The current single-key runtime must not be changed
  in place before that migration support exists.
