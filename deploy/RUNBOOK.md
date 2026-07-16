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
- Private or raw-IP SML endpoints must be inside `SML_ALLOWED_CIDRS` and be
  reachable from the worker network. Customer-owned public DNS endpoints can
  be enabled with `SML_ALLOW_PUBLIC_ENDPOINTS=true`; their resolved addresses
  must remain public. Set `SML_ALLOWED_PORTS=*` to allow any customer TCP port,
  or provide an explicit comma-separated port allowlist when a deployment needs one.
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
into an isolated temporary PostgreSQL container, private Docker network, and
disposable volume. The script first performs a read-only busy check against
Production, then validates the catalog/migration ledger in isolation:

```bash
./deploy/restore-drill.sh ./deploy/.env.production ./backups/<backup>.dump
```

The drill has CPU/memory limits, publishes no port, destroys its container,
network, and volume on exit, and never restores into Production PostgreSQL.

Install the host probe, daily backup, and monthly restore units from
`deploy/systemd/`. `host-probe.sh` inspects only Compose services labelled for
this project and writes bounded sanitized JSON under
`/run/nextstep-dashboard/host`.

## Operational incident alerting

Nextstep Sentinel runs independently from API/Worker and is rolled out in three
phases:

1. Apply migrations and start with `OPERATIONAL_ALERTS_MODE=observe` for at
   least 24 hours. Inspect Admin incidents, evaluation time, false positives,
   and host-probe freshness; no Telegram message is sent in this mode.
2. Rotate any credential ever pasted into chat. Store the new bot token and chat
   identifier in separate root-owned files with mode `0440`, group-readable only
   by the Sentinel container identity. Never place either value in Git, Compose
   environment variables, or logs. Run `sentinel-preflight`; send its one fixed
   test message only with the explicit operator flag.
3. Configure an external watchdog for `/api/v1/health/live`, `/ready`, and
   `/watchdog`, then change to `send`. P1 only is sent to Telegram; P2 remains in
   Admin. Acknowledge is not recovery, and manual closure is accepted risk.

Sentinel reads at most 500 terminal events per cycle and does not call JavaWS.
If application PostgreSQL fails twice, the file-backed emergency lane can alert
directly; after two successful checks it records recovery evidence in PostgreSQL
and sends one recovery message. External monitoring remains required when the
entire host or network is unavailable.

Release restarts use `deploy/release.sh`, which opens the external maintenance
window before any mutation and records the internal window. If the external
maintenance API fails, the release stops unless an explicit audited emergency
override is supplied. The external account timezone must be `Asia/Bangkok` and
its credential remains a deploy-only root secret.

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
- Keep JavaWS admission at `REPORT_GLOBAL_QUERY_CONCURRENCY=4` and
  `REPORT_HOST_QUERY_CONCURRENCY=2`; the runtime additionally permits only one
  active SML query per tenant. Lower these limits before increasing worker
  concurrency when multiple tenants share one JavaWS host.
- Dashboard detail rows are paginated and expire after 24 hours. Summary
  snapshots are versioned by report definition and SML connection and may be
  reused for their configured freshness window. Historical summaries remain
  explicitly timestamped and readable for up to 90 days; expired detail rows
  still require a new SML query when the user asks for row-level detail.
- Snapshot-first rollout is feature-gated. Start with
  `SNAPSHOT_FIRST_ENABLED=true` plus an explicit
  `SNAPSHOT_FIRST_TENANT_IDS` allowlist, then observe cache hit ratio, queue age,
  SML timeouts, and schedule latency before expanding the allowlist.

### Summary generation and heavy-query rollout

`SUMMARY_QUERY_ENABLED` defaults to `true` for every tenant because Overview,
Background, and LINE need bounded aggregate results rather than raw detail
rows. Keep it configurable as an emergency kill switch; setting it to `false`
intentionally falls back to the heavier detail plans. Query/cache capabilities
remain independent:

1. Validate Summary/Detail parity for all ten reports off-peak before deploying
   a query-contract change. Deploy API and Worker from the same immutable image.
2. Keep `GENERATION_CACHE_ENABLED=false` until generation publication and cache
   invalidation have passed their separate rollout gate.
3. Enable `STALE_REVALIDATION_ENABLED=true` only after published generations,
   cache-hit behavior, schedule queue age, and SML load are healthy.
4. Keep `HEAVY_CHUNK_ENABLED=false` until Direct Stock or AR breaches its SLA
   and DEV parity has passed. When needed, set exact allowlist entries such as
   `<tenant-uuid>/stock_balance` or
   `<tenant-uuid>/ar_customer_movement` in
   `HEAVY_CHUNK_TENANT_REPORTS` before enabling the flag.
5. Keep `SCHEDULE_CHUNK_ENABLED=false`; Schedule stays Direct until LINE
   parity and chunk-window consistency have passed a separate release gate.

If JavaWS times out after a request was sent, do not retry or restart the
worker. The tenant uncertainty circuit intentionally blocks new SML queries for
ten minutes because PostgreSQL may still be executing the first query.

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
