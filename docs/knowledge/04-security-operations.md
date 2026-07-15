---
status: current
last_verified: 2026-07-15
source_of_truth: [internal/auth/session.go, internal/sml/endpoint.go, internal/retention/worker.go, deploy/RUNBOOK.md]
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
- Migration changes require a backup and rollback/compatibility analysis; this knowledge-tooling change contains no migration.

## Incident Documentation

Use the sanitized incident template. Record safe error codes, time windows, affected subsystem, evidence sources, containment, fix, and regression tests. Do not copy customer identifiers, payloads, tokens, SQL, KPI values, or full logs into Git.
