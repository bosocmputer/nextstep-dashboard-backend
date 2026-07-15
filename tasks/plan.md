# Implementation Plan: Nextstep Dashboard Backend

## Status

Proposed — planning only. No implementation, commit, push or deployment starts until this plan is approved.

## Architecture Decisions

- Contract-first API under `/api/v1` with one error envelope.
- PostgreSQL is the source of truth for tenants, permissions, schedules, runs and deliveries.
- API and worker are separate commands/images but share Go internal packages.
- Scheduler uses database claims and idempotency keys rather than in-memory cron state.
- Existing 10 TypeScript report contracts are ported to Go in small report-family batches with parity fixtures.
- Existing LINE OA is outbound-only for Nextstep V1; Nexflow webhook is unchanged.
- LINE Login/LIFF identity is the viewer security boundary.
- Production images are built by GitHub Actions and pulled from GHCR.

## Phase 0: Plan Approval

### Task 0.1 — Review blueprint and open decisions

**Acceptance criteria:**

- Product scope, domain, admin password policy, retention and TLS path are approved.
- No unresolved decision changes the database or auth design.

**Verification:** User approval in the task before implementation.

**Dependencies:** None.

## Phase 1: Executable Foundation

### Task 1.1 — Scaffold Go service and configuration contract

- Create Go module and API command.
- Validate required environment configuration at startup.
- Add live/ready health handlers and table-driven tests.

**Acceptance:** invalid config fails safely; live and ready responses follow API contract.

**Verify:** `go test ./...`, `go vet ./...`, container build.

### Task 1.2 — Add PostgreSQL migrations and repository boundary

- Add versioned schema migrations for foundation tables.
- Add pgx pool lifecycle and migration command.
- Add PostgreSQL integration test workflow.

**Acceptance:** empty database migrates idempotently; readiness fails when database is unavailable.

**Verify:** migration up/down test and integration test on PostgreSQL 16.

### Task 1.3 — Implement Platform Admin authentication

- Environment-backed username/password hash.
- Rate-limited login, server-side hashed session, logout and session endpoint.
- Secure cookie and audit events.

**Acceptance:** valid login works; wrong password, expired session and rate limit are covered; password never appears in logs.

**Verify:** unit and API integration tests.

### Checkpoint 1

- Tests, vet and Docker build pass.
- Database starts cleanly.
- Admin session works through the API.

## Phase 2: Tenant and SML Vertical Slice

### Task 2.1 — Tenant lifecycle CRUD

- Create/list/get/patch tenants.
- Enforce status, lifetime plan and access end date.
- Add pagination and audit log.

### Task 2.2 — Encrypted tenant secret store

- AES-256-GCM envelope using environment master key.
- Store encrypted SML credentials without returning them.
- Add key/config failure tests.

### Task 2.3 — SML Java Web Service connection test

- Implement bounded HTTP client and response decoder.
- Validate configured endpoint and prevent unintended destinations.
- Return safe diagnostic result.

### Checkpoint 2

- Admin can create a tenant and test its SML connection through API tests.
- Secret values never appear in API responses, logs or fixtures.

## Phase 3: Approved Report Engine

### Task 3.1 — Report contract and registry

- Define Go report interfaces, parameter/period types and snapshot envelope.
- Seed all 10 report definitions.
- Reject unknown report keys.

### Task 3.2 — Shared SML query executor

- Approved-query-only execution path.
- Timeouts, compressed/XML decoding, row limits and safe errors.
- Fake JavaWS server tests.

### Task 3.3 — Sales and purchase reports

- Port `sales_goods_services` and `purchase_goods_payables`.
- Add fixtures and compare totals/row semantics with approved source behavior.

### Task 3.4 — Gross profit reports

- Port product and AR-customer gross profit reports with margin edge cases.

### Task 3.5 — Stock reports

- Port stock balance and reorder reports with as-of period semantics.

### Task 3.6 — AR reports

- Port customer movement and debt receipt reports.

### Task 3.7 — Cash/bank reports

- Port receipt and payment reports with allocation/mismatch rules.

### Task 3.8 — Persist run and snapshot lifecycle

- Queue/claim/run/succeeded/failed states.
- Save validated snapshots only.
- Record quality and reconciliation metadata.

### Checkpoint 3

- All 10 report fixture/parity tests pass.
- Manual dry-runs against an approved SML test tenant reconcile before enabling LINE.

## Phase 4: Recipients, Permissions and Scheduler

### Task 4.1 — LINE recipient and tenant membership API

- Create pending/active recipients and tenant memberships.
- Enforce unique LINE identity per membership.

### Task 4.2 — Report permission API

- Assign/revoke report keys per recipient and tenant.
- Enforce catalog-only keys.

### Task 4.3 — Notification schedule API

- CRUD schedule, days, period, reports and recipients in a transaction.
- Validate local time and timezone.

### Task 4.4 — Due-schedule worker and idempotency

- Database claim, heartbeat, retry and duplicate prevention.
- Handle tenant disabled/expired between claim and execution.

### Checkpoint 4

- Concurrent worker test proves a schedule is claimed once.
- Retry test proves a recipient is not sent twice.

## Phase 5: LINE Delivery

### Task 5.1 — LINE channel routing and encrypted credentials

- System shared channel plus optional tenant override.
- Safe connection/test status without token exposure.

### Task 5.2 — Compact numeric Flex renderer

- One bubble, permission-filtered report sections and payload-size guard.
- Golden JSON tests for empty, one-report and ten-report cases.

### Task 5.3 — LINE sender, retry and delivery history

- Handle success, 4xx, 429 and retryable 5xx/timeout.
- Persist request identity, provider response status and safe error.

### Checkpoint 5

- Dry-run renders the expected card.
- Test send requires explicit admin action and records one delivery.

## Phase 6: LIFF Viewer Security

### Task 6.1 — Verify LINE ID/access tokens server-side

- Verify tokens with LINE Platform and validate audience/expiry.
- Never trust profile data supplied directly by frontend.

### Task 6.2 — Viewer session and delivery access link

- Opaque link, recipient binding, session rotation and expiry.
- Copied-link denial tests using a different LINE subject.

### Task 6.3 — Permission-filtered viewer endpoints

- Tenant/report list and snapshot/detail endpoints.
- Server-side authorization on every request.

### Checkpoint 6

- Valid recipient can view; another user and another tenant receive 403.
- Revoked membership invalidates access.

## Phase 7: Operations and Retention

### Task 7.1 — Run/delivery/audit admin queries

- Pagination, filters and safe metadata.

### Task 7.2 — Retention and cleanup jobs

- Configurable snapshot and session retention.
- Audit deletion counts without deleting active references.

### Task 7.3 — Backup/restore and operational health

- PostgreSQL backup script, restore drill and worker health.

## Phase 8: CI/CD and Deployment

### Task 8.1 — Backend GitHub Actions and GHCR

- Vet/test/integration/build/audit gates.
- Publish SHA and main tags only after gates pass.

### Task 8.2 — Docker images and production Compose

- API, worker and PostgreSQL services on isolated network/volumes.
- Healthchecks, resource boundaries and no public DB/backend ports.

### Task 8.3 — Production bootstrap

- Create `/mnt/data/nextstep-dashboard` with controlled ownership.
- Configure `.env` and GHCR read-only login.
- Pull/up, smoke test and verify no port/container collision.

### Task 8.4 — Complete LINE Console after HTTPS is live

- Link existing OA, add callback and LIFF endpoint.
- Keep Nexflow webhook unchanged.
- Perform test identity and copied-link denial.

## Release Gate

- All tests, vet, migrations and builds pass.
- No secret in git diff or image metadata.
- Backup and rollback commands are tested.
- Frontend/runtime browser test passes.
- LINE message and viewer authorization pass end-to-end.
- Existing Nexflow webhook and other server projects remain healthy.

## Risks and Mitigations

| Risk | Impact | Mitigation |
| --- | --- | --- |
| SQL/report parity differs from existing system | Wrong business numbers | Fixture and live sample reconciliation per report family |
| Shared OA quota | Missing messages/429 | Usage ledger, quota warning and per-delivery retry policy |
| Weak bootstrap password | Cross-tenant compromise | Environment hash, rate limit, TLS and change/IP restriction before launch |
| Copied LIFF URL | Data disclosure | LINE token verification plus recipient-bound authorization |
| Worker duplicate claim | Duplicate LINE messages | Unique idempotency keys and DB transaction claims |
| Production host collision | Other projects affected | Unique compose project, port preflight and private service ports |
| GHCR or migration failure | Deployment outage | Immutable tags, health-gated startup, backup and rollback |

