---
status: current
last_verified: 2026-07-19
source_of_truth: [internal/auth/session.go, internal/sml/endpoint.go, internal/sml/config_service.go, internal/database/sml_test_coordinator.go, internal/retention/worker.go, internal/sentinel/service.go, internal/database/sentinel_store.go, internal/database/sentinel_subject_store.go, internal/failure/catalog.go, deploy/RUNBOOK.md]
tags: [backend, security, operations, retention]
---

# Security and Operations

## Authentication and Authorization

- Admin and Viewer use separate secure, httpOnly, same-site sessions and CSRF cookies for unsafe requests.
- LINE identity is verified server-side before a viewer session is issued.
- Every viewer resource rechecks tenant membership and report permission.
- Viewer stored-row query POSTs require the Viewer CSRF check even though they
  are read-only queries, then re-authorize tenant, report, run, and row expiry.
- Row filter columns/operators are selected from the backend report catalog;
  client-supplied value types are ignored and SQL values remain parameterized.
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
- Admin DataTable queries are read-only, CSRF-protected POST requests with typed
  filter allowlists, fixed page sizes, a two-second database statement timeout,
  and count/page reads from one repeatable-read snapshot. Viewer stored-row
  queries use the same fail-closed pattern with a three-second timeout and never
  invoke JavaWS, create a Report Run, or send LINE.
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
- Incidents use root-cause/severity families, five-minute episodes, and per-subject lifecycle state. Discrete bursts send at most OPEN plus one UPDATE; continuous probes do not inflate occurrence/version on every heartbeat. Acknowledge stops reminders; only system evidence resolves every active subject. Manual closure is `CLOSED_ACCEPTED` with a reason.
- The emergency database alert lane stores only a safe reference and timestamps in a protected volume. Host probe and monitor heartbeat files are bounded, schema-checked, and contain no customer data.
- Admin incident APIs are Admin-only, CSRF-protected for mutations, and `no-store`. Telegram tenant context is disabled by default. The `private_chat` mode is enabled only after `getChat` verifies the exact private destination; it may carry a sanitized tenant name and JavaWS Base URL for tenant-scoped P1 alerts. Failed or non-private verification redacts that context without blocking the P1.
- Incident events copy sanitized failure evidence and LINE/report impact at
  observation time, so the 365-day incident record remains useful after source
  Report or Notification retention. The occurrence resolver may match a
  connection version to a sanitized historical audit URL, or explicitly label a
  current-only fallback. URL userinfo, query, and fragment are removed.
- The authenticated Incident list includes at most two tenant-name examples per
  incident from the same bounded SQL query; Codex clipboard output never
  includes those names. Telegram loads at most five complete tenant/URL pairs
  with one bounded best-effort query only when verified private mode is active.
  Incident detail returns at most 200 newest events
  to keep the response bounded below its payload budget.
- A linked `REPORT_SET_INCOMPLETE` is downstream impact of its failed report and
  is suppressed as a second P1 alert. If no root report is provable inside the
  aggregation window, the notification failure remains eligible as a standalone
  incident rather than being hidden.
- Telegram uses Thai local time and lifecycle-specific wording. SML P1 messages
  state only whether JavaWS is unavailable or reachable again; RECOVERY omits
  the earlier cause/impact repetition and uses the evidence-backed resolved
  timestamp. In verified private mode, OPEN, REMINDER, UPDATE, and RECOVERY may
  include the sanitized URL matching the failure connection version; a
  current-only fallback is labelled explicitly. Acknowledge only stops
  reminders; recovery still requires system evidence for each subject.
- Admin JavaWS investigation separates opening a sanitized URL in the operator's
  browser from a guarded Server Dashboard test. The test uses fixed `select 1`,
  shares report admission limits, is single-flight with cooldown, yields to an
  active/nearby Schedule, and never resolves an incident. An uncertain remote
  outcome opens the tenant circuit and is not retried automatically.
- Backup policy is `PRE_MIGRATION_ONLY`: the release checks pending migrations,
  then creates a checksummed backup and performs an isolated restore verification
  only when a migration is pending. There are no daily/offsite/monthly stale P2s;
  the two newest verified pre-migration backup sets are retained.

## Living Context Gate

- `docs/knowledge/context-map.json` maps production-sensitive source paths to the notes that must be reviewed.
- `make context-verify` validates map/schema/path/marker safety and checks the generated report catalog without writing.
- Pull requests changing mapped source must update a mapped note or include auditable `Context-Reviewed` and `Context-Reason` lines.
- Graphify remains local-only and is never installed or executed by CI.

## Incident Documentation

Use the sanitized incident template. Record safe error codes, time windows, affected subsystem, evidence sources, containment, fix, and regression tests. Do not copy customer identifiers, payloads, tokens, SQL, KPI values, or full logs into Git.
