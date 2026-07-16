---
status: current
last_verified: 2026-07-16
source_of_truth: [internal/auth/session.go, internal/sml/endpoint.go, internal/retention/worker.go, internal/sentinel/service.go, internal/database/sentinel_store.go, deploy/RUNBOOK.md]
tags: [backend, security, operations, retention]
---

# Security and Operations

## Authentication and Authorization

- Admin and Viewer use separate secure, httpOnly, same-site sessions and CSRF cookies for unsafe requests.
- LINE identity is verified server-side before a viewer session is issued.
- Every viewer resource rechecks tenant membership and report permission.
- Invitation/delivery references are opaque entry references, not authorization tokens.
- Explicit tenant/delivery mismatch fails closed without revealing whether another tenant resource exists.

## SML Boundary

- Tenant SML credentials are encrypted at rest with the configured master key/key ID.
- Endpoint policy validates schemes, hosts/addresses, ports, and redirect behavior before JavaWS access.
- Only approved read-only query plans are sent; never add write-back or schema changes to customer SML.
- JavaWS responses are size/row bounded and validated before dashboards or rows publish.
- Logs use safe error codes and operational timing, never endpoint credentials, SQL, raw rows, or KPI values.

## Retention

- Detail rows follow their per-run expiry (normally 24 hours for viewer detail work).
- Dashboard snapshots/generations and scheduled report payload fields are retained/scrubbed at the 90-day policy boundary.
- Delivery/audit/history records use the 365-day policy boundary where their own expiry/status permits deletion.
- Expired access links and sessions are removed in bounded retention batches.
- Retention updates generation heads before deleting expired generations so stale pointers are not served.

## Production Operations

- `deploy/RUNBOOK.md` is the operational source for preflight, backup, migration, health, smoke, rollback, and key rotation.
- Never deploy from these knowledge notes or assume a branch/image is live.
- Verify current image digests, worker heartbeats, queue state, feature flags, and next schedules from the runtime before an operational change.
- Migration changes require a backup and rollback/compatibility analysis.
- Release maintenance opens the external window before application mutation,
  records the internal window by an exact UUID, and suppresses PostgreSQL
  command tags so successful inserts cannot be misclassified as failures.

## Nextstep Sentinel

- Sentinel is a separate process and database pool. Report, Notification, and Delivery transactions never call the incident writer, so monitoring failure cannot roll back business work.
- Terminal Report, Notification, and Delivery scans use durable source state,
  bounded batches, per-source cursors, a five-minute overlap, deduplication, and
  backlog-aware advancement so the 500-row limit cannot skip later events.
  Historical notification rows remain `UNKNOWN` and do not generate alerts.
- P1 Telegram delivery is disabled in `off`/`observe` modes. `send` requires root-owned token/chat files and must follow the runbook preflight and observation window.
- Incidents group by root cause and severity to avoid a multi-tenant message storm. Acknowledge stops reminders; only system evidence resolves an incident. Manual closure is `CLOSED_ACCEPTED` with a reason.
- The emergency database alert lane stores only a safe reference and timestamps in a protected volume. Host probe and monitor heartbeat files are bounded, schema-checked, and contain no customer data.
- Admin incident APIs are Admin-only, CSRF-protected for mutations, and `no-store`. Telegram carries only an alert reference and safe operational fields; tenant names are resolved only inside the authenticated Admin detail page.
- Daily backup/host probes and isolated restore drills are host systemd jobs. Restore validation uses a temporary PostgreSQL container/volume and never targets the Production PostgreSQL instance.

## Living Context Gate

- `docs/knowledge/context-map.json` maps production-sensitive source paths to the notes that must be reviewed.
- `make context-verify` validates map/schema/path/marker safety and checks the generated report catalog without writing.
- Pull requests changing mapped source must update a mapped note or include auditable `Context-Reviewed` and `Context-Reason` lines.
- Graphify remains local-only and is never installed or executed by CI.

## Incident Documentation

Use the sanitized incident template. Record safe error codes, time windows, affected subsystem, evidence sources, containment, fix, and regression tests. Do not copy customer identifiers, payloads, tokens, SQL, KPI values, or full logs into Git.
