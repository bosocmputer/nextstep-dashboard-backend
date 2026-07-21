---
status: current
last_verified: 2026-07-21
source_of_truth: [internal/worker/report_worker.go, internal/database/report_store.go, internal/database/schedule_execution_store.go, internal/notification/worker.go, internal/delivery/worker.go, internal/failure/catalog.go]
tags: [backend, reports, queue, line]
---

# Report and LINE Pipeline

## Projections

- `SUMMARY` serves dashboard overview, background refresh, and scheduled LINE work. Aggregate SQL returns bounded KPI/trend/ranking data and does not write raw `report_run_rows`.
- `DETAIL` is created for explicit report-detail work and may retain pageable rows until their per-run expiry.
- Summary query results are capped by the query plan; totals cover the source set while only bounded presentation rows cross JavaWS.
- `QueryPlanFingerprint` covers normalized SQL, projection, dashboard builder source, formatter source, and contract versions to invalidate incompatible cache output automatically.

## Scheduled Flow

```text
Due schedule
  -> resolve effective period per report
  -> materialize immutable notification report positions
  -> enqueue SUMMARY report runs at schedule priority
  -> report worker validates JavaWS output and builds dashboards
  -> notification worker requires the complete report set
  -> render one bounded Flex bubble per eligible recipient
  -> publish delivery/outbox records
  -> delivery worker pushes to LINE with lease-safe status changes
```

`Work.Partial` causes `REPORT_SET_INCOMPLETE` before recipient selection or rendering. No LINE delivery or outbox payload is created from an incomplete report set. `ALL_REPORTS_FAILED` and `NO_ELIGIBLE_RECIPIENTS` remain distinct failure causes.

Notification occurrences are classified at materialization time: due schedules
write `SCHEDULED`, manual test sends write `TEST`, and historical rows remain
`UNKNOWN`. Sentinel may alert terminal `SCHEDULED` failures but must never infer
that an old or test occurrence was a scheduled customer delivery.

## Queue and Lease Safety

- Default priorities are Schedule 100, Viewer Dashboard 90, and Background FAST/STANDARD/HEAVY 30/25/20.
- An active/recent compatible request may be joined where the store explicitly permits it; schedule occurrences retain their own materialization.
- A lease-lost worker cannot publish a result.
- Recovery marks abandoned work with safe codes and protects incomplete notification sets.
- Browser cancellation only stops tracking; only queued work is safely cancellable through the report API.

## JavaWS Failure Semantics

- Failure before sending a request may receive one bounded retry.
- Timeout/reset after request send has unknown remote state, is not automatically retried, and opens tenant protection before another query starts.
- Repeated connection failures contribute to tenant/host circuit state.
- Admin/Viewer errors expose safe codes and request IDs, not SQL or rows.
- A terminal Report failure persists sanitized `FailureEvidence` atomically with
  the run state. It records the stage and transport phase known by the Worker at
  failure time; no later reader infers those facts from an error code.
- `BEFORE_REQUEST_SENT`, `REQUEST_SENT_RESULT_UNKNOWN`, and `RESPONSE_STARTED`
  remain distinct. Unknown remote state must not be presented as a stopped
  customer query or as safe to retry immediately.
- The shared failure catalog is the source for Thai Admin/Telegram wording and
  next checks. Unknown codes use a generic Thai fallback and never raw error
  text.
- Evidence version 2 attaches an opaque `NXR-...` request reference to the
  JavaWS HTTP request and persists only bounded protocol metadata: request and
  retry counts, transport timestamps, HTTP/content metadata, SOAP/Base64/ZIP
  validation, response byte counts/hash, and observed admission concurrency.
  It never persists SQL, SOAP/response bodies, rows, or KPI values.
- Admin attribution distinguishes a customer JavaWS/network response from
  Nextstep report build, storage, queue, notification, LINE, database, and
  capacity stages. A claim that no abnormal Nextstep load signal was found
  requires at least five exact successful samples for the same tenant, report,
  projection, query fingerprint, connection version, and resolved period.

## Viewer Snapshot Flow

- An exact authorized snapshot is returned before revalidation decisions.
- Fresh cache produces no JavaWS call.
- Revalidation and generation-cache behavior is feature-flagged; inspect configuration before asserting it is active.
- Delivery context always uses the report runs attached to that immutable occurrence and never revalidates automatically.
- Viewer report tables can filter and exact-page existing `report_run_rows` by
  catalog-approved text, identifier, date, or number columns. The query reads
  only an authorized succeeded run before row expiry, keeps identifiers exact,
  supports PostgreSQL scientific numeric strings, returns stable row ordinals,
  and never starts SML work. Date ranges are inclusive in Bangkok business-date
  semantics and Global Search is limited to catalog-approved columns.
