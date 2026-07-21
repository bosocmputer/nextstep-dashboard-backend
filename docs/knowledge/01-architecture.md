---
status: current
last_verified: 2026-07-21
source_of_truth: [cmd/api/main.go, cmd/worker/main.go, cmd/sentinel/main.go, internal/database/pool.go, internal/httpapi/operations_handler.go, internal/httpapi/incident_handler.go, internal/database/sentinel_subject_store.go, internal/failure/catalog.go, deploy/compose.production.yml]
tags: [backend, architecture, multitenant]
---

# Architecture

## Runtime Components

```text
Browser / LINE LIFF
  -> Frontend Nginx (same-origin /api proxy)
     -> Go API
        -> Nextstep PostgreSQL

Distributed worker
  -> scheduler -> report queue -> tenant JavaWS -> customer SML PostgreSQL (read-only)
  -> notification preparation -> LINE delivery outbox -> LINE Messaging API
  -> retention, quota, recovery, and heartbeats

Sentinel monitor (independent process)
  -> bounded durable-state scans + host probe files
  -> operational incidents/outbox -> Telegram P1 alerts
  -> file-backed emergency lane when application PostgreSQL is unavailable
```

- `cmd/api` builds the stateless HTTP application and service graph.
- `cmd/worker` starts independent loops sharing the application database.
- `cmd/sentinel` observes terminal state and runtime probes without joining business transactions or querying SML.
- PostgreSQL stores configuration, encrypted credentials, sessions, permissions, schedules, report/snapshot state, delivery state, audit, idempotency, leases, and circuits.
- JavaWS and LINE are external dependencies with explicit timeout and safe failure behavior.

## Multi-tenant Isolation

- Customer-owned tables carry `tenant_id` and stores validate tenant relationships.
- Route IDs, run IDs, delivery IDs, and references never grant access by themselves.
- Recipient membership and report permission are independent, explicit records.
- Viewer and admin services resolve resources through tenant-scoped stores.
- Cross-tenant inconsistency fails closed rather than redirecting or falling back.

## Work Coordination

- PostgreSQL leases coordinate distributed workers and prevent a stale claimant from publishing.
- The report store defaults to one active query per tenant, two per JavaWS host, and four globally; environment values can lower or raise bounded host/global limits.
- Schedule runs have the highest queue priority; viewer work precedes background work.
- Tenant and host circuits protect SML after uncertain remote execution or repeated connection failures.
- Worker heartbeats expose role and safe operational metadata without business payloads.

## Consistency Model

- A single JavaWS result is not a cross-report database transaction.
- Summary generations publish only after their required report set satisfies the generation rules.
- Chunked work, when enabled, records a collection window and never claims a single instant snapshot.
- Cache keys/fingerprints include report/query/builder/data-source identity so incompatible output is not reused.
- Admin history/schedule filters and viewer stored-row filters are bounded,
  parameterized database reads. Recipient name search decrypts only the bounded
  supported tenant set in the service. None enters the report queue or contacts
  customer JavaWS.
- Admin table-query POSTs use resource-specific typed filter allowlists, exact
  totals, stable ordering with an ID tie-breaker, and a read-only repeatable-read
  transaction for count plus page data. They are CSRF-protected, `no-store`,
  cancelled with the HTTP request, and bounded by a two-second statement timeout.

## Operational Evidence API

- Admin Report Run detail returns persisted, sanitized failure evidence and the
  verified impact on its materialized LINE occurrence; it never queries SML.
- Admin Incident list/detail reads bounded incident evidence retained separately
  from Report Run rows. Thai presentation comes from the shared failure catalog,
  while technical codes remain additive contract fields.
- The incident API also returns an evidence-backed lifecycle presentation with
  the current Thai headline, operator summary, verified timestamp, and whether
  Admin action is required. Clients must not infer recovery from an acknowledge
  action or from a successful manual JavaWS test.
- Sentinel separates an incident family, a five-minute episode, and per-subject
  state. Discrete failures can aggregate a burst without merging later episodes;
  continuous conditions persist bounded updates and recover each subject before
  resolving the incident.
- Incident correlation records an incomplete LINE report set as downstream of
  its proven Report failure, avoiding a duplicate P1 without hiding the timeline.
- Incident occurrences are a separately paginated Admin-only endpoint. It may
  return a sanitized JavaWS URL reference for investigation, but Incident list,
  Telegram, metrics, logs, and Codex clipboard never contain that URL.
- The per-occurrence diagnosis endpoint combines persisted Evidence V2 with at
  most two matching-run lookups for prior/subsequent success and a bounded
  duration baseline. It maps the confirmed failure stage to an investigation
  owner and may create a separate Admin-only customer message; opening it never
  contacts JavaWS, retries a report, or changes incident state.
- These endpoints are Admin-only, `no-store`, and never return SQL, raw response,
  credentials, KPI values, or delivery/invitation references.

## Deployment Boundary

Production deployment uses private API/PostgreSQL networking behind the frontend reverse proxy. TLS/public routing and operational commands live in `deploy/RUNBOOK.md`; never infer the currently deployed image from repository history alone.
