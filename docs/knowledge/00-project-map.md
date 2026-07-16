---
status: current
last_verified: 2026-07-16
source_of_truth: [cmd/api/main.go, cmd/worker/main.go, cmd/sentinel/main.go, api/openapi.yaml, internal/report/catalog.go]
tags: [nextstep, backend, project-map]
---

# Backend Project Map

Nextstep Dashboard Backend is the authorization and business-data boundary for a multi-tenant SML reporting platform. It exposes the Go HTTP API, persists application state in PostgreSQL, runs distributed workers, reads customer SML through JavaWS, and sends LINE messages.

## Read by Task

| Task | Read next | Verify in source |
| --- | --- | --- |
| Components, data boundaries, concurrency | [Architecture](01-architecture.md) | `cmd/`, `internal/database/`, deployment compose |
| Tenant, recipient, schedule, report rules | [Domain and reports](02-domain-and-reports.md) | `internal/report/catalog.go`, OpenAPI, migrations |
| Queue, Summary/Detail, LINE delivery | [Report and LINE pipeline](03-report-line-pipeline.md) | report worker/store, notification and delivery workers |
| Auth, SML safety, retention, release | [Security and operations](04-security-operations.md) | auth/SML/retention source and `deploy/RUNBOOK.md` |
| Production incidents, Telegram, host/backup probes | [Security and operations](04-security-operations.md) | `internal/sentinel/`, Sentinel store, deploy scripts |
| User-facing behavior | Frontend knowledge vault | `../nextstep-dashboard-frontend/docs/knowledge/00-project-map.md` |

## Trust Order

1. Current source, tests, migrations, and `api/openapi.yaml`
2. Deployment configuration and verified runbook
3. Notes in this vault with current `last_verified`
4. Historical blueprints and task notes
5. Graphify output and conversation history

If sources disagree, stop relying on the lower-ranked source and report the drift.

## Stable Boundaries

- PostgreSQL here is the application database, not the customer SML database.
- All SML queries go through a tenant-configured JavaWS endpoint and approved report plans.
- Report keys, query projections, dashboard builders, and LINE presentation are allowlisted code.
- Viewer sessions, tenant membership, report permissions, and delivery ownership are rechecked by the backend.
- Scheduler, report, notification preparation, LINE delivery, retention, quota, and heartbeat loops are worker responsibilities; Sentinel is a separate observer and alert sender.
- Feature flags may alter cache/chunk/revalidation behavior; inspect `internal/config/config.go` and live configuration before claiming a flag is enabled.

## Documentation Policy

- Record stable intent, invariants, and failure behavior—not copied SQL or implementation.
- Reference source paths for facts that can change.
- Do not record customer-specific state, production values, or credentials.
- Update the relevant note and `last_verified` after a material behavior change.
