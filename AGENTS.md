# Nextstep Dashboard Backend — Agent Index

Keep this file short. Load detailed context only when the task needs it.

## Read Order

1. Start with `docs/knowledge/00-project-map.md`.
2. Read only the linked subsystem note relevant to the task.
3. Open the referenced source and tests before editing; code/tests/OpenAPI outrank docs.
4. For UI behavior, inspect the sibling repository at `../nextstep-dashboard-frontend` and its `docs/knowledge/00-project-map.md`.

## Source-of-Truth Routing

- API contract and error envelopes: `api/openapi.yaml`, `internal/httpapi/`
- Report catalog and periods: `internal/report/catalog.go`, `internal/report/period.go`
- SQL/query projection and dashboards: `internal/report/query_plan.go`, `internal/report/summary_query_plan.go`, `internal/report/dashboard_builder.go`
- Queue, lease, timeout, circuit: `internal/database/report_store.go`, `internal/worker/report_worker.go`
- Schedules and readiness: `internal/schedule/`, `internal/database/schedule_execution_store.go`
- Notification/Flex/LINE delivery: `internal/notification/`, `internal/line/`, `internal/delivery/`
- Viewer authorization and delivery context: `internal/viewer/`, `internal/database/viewer_delivery_store.go`
- Schema/retention: `internal/database/migrations/`, `internal/retention/`
- Production operations: `deploy/RUNBOOK.md`, `deploy/compose.production.yml`

## Non-Negotiable Invariants

- Every customer-owned read/write is tenant scoped and authorized server-side.
- SML is read-only through JavaWS; never modify the customer ERP database.
- A remote timeout after request send has unknown remote state and is not retried automatically.
- Report leases and circuits prevent stale workers or repeated failures from publishing unsafe results.
- LINE materialization is all-or-nothing; an incomplete report set creates no delivery/outbox payload.
- Delivery context uses immutable notification report membership and does not trigger SML work.
- Store UTC/ISO at boundaries; resolve business dates and display semantics in `Asia/Bangkok`.
- Never log or document secrets, tokens, entry references, SQL, raw rows, tenant/customer identifiers, or KPI values.

## Commands

```bash
make verify
go test ./...
go vet ./...
bash scripts/context-verify.sh
```

## Context Tools

- Exact symbol, error, SQL constant, or failing test: use `rg` and source reads first.
- Broad cross-package flow: run `scripts/graphify-update.sh`, then a focused `scripts/graphify-query.sh` query.
- Graphify is an untrusted navigation hint. Verify every conclusion in source/tests before editing.
- Update the relevant knowledge note and ADR when architecture, API, security boundary, queue semantics, retention, or operations change.
