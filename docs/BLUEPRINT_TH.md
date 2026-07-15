---
status: historical
last_verified: 2026-07-15
source_of_truth: [api/openapi.yaml, internal/report/catalog.go]
tags: [backend, historical, blueprint]
---

# Nextstep Dashboard Blueprint

> Historical proposal: เอกสารนี้เก็บบริบทการออกแบบเริ่มต้นเท่านั้น หลาย contract และพฤติกรรมไม่ตรงกับ Production ปัจจุบัน ห้ามใช้เป็น source of truth สำหรับการ implement ให้เริ่มที่ `docs/knowledge/00-project-map.md` และตรวจ source/tests/OpenAPI จริง

## สถานะเอกสาร

- สถานะ: Proposed — รออนุมัติก่อนเริ่ม implementation
- Product: Nextstep Dashboard
- Frontend repository: `bosocmputer/nextstep-dashboard-frontend`
- Backend repository: `bosocmputer/nextstep-dashboard-backend`
- Production target: `/mnt/data/nextstep-dashboard` บน `nextstep-node-2`
- Public frontend port: `6324`

## 1. Objective

สร้างศูนย์กลางรายงาน SML แบบ multi-tenant สำหรับหลายร้านค้า ระบบรันรายงานตามวันและเวลาที่กำหนด ส่งตัวเลขผ่าน LINE Flex Message และให้ผู้รับเปิด Dashboard เพื่อดูเฉพาะร้านและรายงานที่ได้รับสิทธิ์

ระบบนี้ไม่มี AI, chatbot, recommendation, prediction หรือการสร้าง SQL แบบอิสระ

## 2. Product Users

### Platform Admin

- มีผู้ดูแลระบบส่วนกลางเพียงบทบาทเดียว
- เข้า `/admin` ด้วย username/password
- เพิ่ม แก้ไข เปิด ปิด และกำหนดวันสิ้นสุดร้าน
- ตั้งค่า SML Java Web Service, LINE channel, ผู้รับ, สิทธิ์ และ schedule
- ดู report run, delivery history, audit และ system health

ค่า username เริ่มต้นที่ผู้ใช้ระบุคือ `superadmin` ส่วนรหัสผ่านเริ่มต้นเป็นข้อมูลลับที่ตั้งผ่าน production environment เท่านั้นและห้ามบันทึกลง repository ค่า password hash ต้องมาจาก environment และต้องมี login rate limit, secure session cookie และ audit log

### Store Viewer

- ไม่มีสิทธิ์เข้า `/admin`
- เข้า `/app` ผ่าน LINE Login/LIFF
- ผูกตัวตนด้วย LINE user ID
- หนึ่งคนอาจอยู่ได้หลายร้าน
- เห็นเฉพาะ report keys ที่ Platform Admin อนุญาตในร้านนั้น

## 3. Core Flow

```text
Platform Admin configures tenant, SML, recipients and schedules
  -> Go scheduler claims a due schedule
  -> approved report runners query SML Java Web Service
  -> validate output and persist report run/snapshot
  -> intersect schedule reports with each recipient permission
  -> render one compact LINE Flex bubble per recipient
  -> push through tenant LINE OA or the system shared OA
  -> recipient opens LIFF URL
  -> backend verifies LINE ID token and recipient identity
  -> dashboard returns only authorized tenant/report data
```

## 4. Functional Scope

### Tenant Management

- Tenant name, slug and timezone
- Status: `ACTIVE`, `DISABLED`, `EXPIRED`
- One plan: `LIFETIME`
- Required `accessEndsAt`
- Feature availability is the intersection of status and access end date
- Disabled or expired tenants cannot run reports, receive messages or open the viewer

### SML Connection

- One SML Java Web Service connection per tenant in V1
- Endpoint URL, database name and credentials
- Credentials encrypted at rest using AES-256-GCM and a master key from environment
- Connection test with timeout and safe error response
- Only approved read-only SELECT/CTE queries
- No write-back to SML

### Report Catalog

The first production release includes all 10 approved reports:

1. `sales_goods_services` — รายงานขายสินค้าและบริการ
2. `purchase_goods_payables` — รายงานซื้อสินค้า/ตั้งหนี้
3. `gross_profit_by_product` — กำไรขั้นต้นตามสินค้า
4. `gross_profit_by_ar_customer` — กำไรขั้นต้นตามลูกหนี้
5. `stock_balance` — สต็อกคงเหลือ
6. `stock_reorder` — สินค้าถึงจุดสั่งซื้อ
7. `ar_customer_movement` — ความเคลื่อนไหวลูกหนี้
8. `ar_debt_receipt` — รับชำระหนี้
9. `cash_bank_receipts` — รับเงิน
10. `cash_bank_payments` — จ่ายเงิน

Each report contract must define:

- Stable report key and version
- Parameter and output schema
- Approved SQL/query builders
- Scheduled period mapping
- Summary metrics for LINE
- Full snapshot shape for Dashboard
- Timeout, row and date-range limits
- Reconciliation/data-quality rules
- Safe empty, stale and error behavior

### Notification Schedule

One tenant can have multiple schedules. Each schedule stores:

- Name and enabled status
- Days of week
- Local send time and timezone
- Period preset: `YESTERDAY`, `TODAY_TO_NOW`, `MONTH_TO_DATE`, `AS_OF_RUN`
- Selected report keys
- Selected recipient IDs
- Retry policy

Every scheduled execution has an idempotency key derived from tenant, schedule, recipient, local run date/time and period. A retry must never send a duplicate message.

### LINE Recipients and Permissions

- Recipient identity is a LINE user ID obtained from verified LINE Login/LIFF
- Recipient membership is explicit per tenant
- Report permission is explicit per tenant, recipient and report key
- Flex content is `schedule report keys ∩ recipient report permissions`
- Empty intersections do not send a message

### LINE Channel Routing

- System shared OA is the default channel
- A tenant may override with its own OA
- Existing `NEXT STEP OFFICER` Messaging API channel may be used for outbound messages
- Existing Nexflow webhook remains unchanged
- Nextstep V1 does not require a Messaging API webhook because recipients onboard through LIFF
- Message quota is shared when the same OA is used by multiple systems

### Flex Message

- One Flex bubble per recipient per schedule execution
- Store name, period and generated time
- 2–4 numeric metrics per selected report
- Dividers and label/value rows based on the supplied reference image
- One `เปิดรายงาน` action
- No recommendation, anomaly wording or AI-generated content
- Renderer enforces LINE payload/component limits before sending

### Secure Viewer

- Button URL is a LIFF URL containing an opaque delivery reference, not authorization by itself
- Frontend sends the raw LINE ID token or access token to backend
- Backend verifies the token with LINE Platform
- Backend checks token subject against the intended recipient
- Backend creates an httpOnly, secure, sameSite session
- Every viewer endpoint re-checks tenant membership and report permission
- Copying the URL to a different LINE account must return `403 FORBIDDEN`

## 5. Data Quality Invariants

- Every number has tenant, report key, run ID, period and generated timestamp
- Snapshot is written only after output validation succeeds
- Failed runs never masquerade as fresh data
- Dashboard may display the last successful snapshot only when clearly labelled with its age
- LINE scheduled delivery uses only the snapshot created for that execution
- Reconciliation stores row count, summary totals and warning status
- No raw SML credentials, LINE tokens, full signed URLs or full sensitive rows in logs

## 6. High-Level Data Model

```text
tenants
tenant_sml_connections
line_channels
line_recipients
tenant_memberships
report_definitions
recipient_report_permissions
notification_schedules
notification_schedule_days
notification_schedule_reports
notification_schedule_recipients
report_runs
report_snapshots
notification_runs
line_deliveries
line_login_nonces
viewer_sessions
delivery_access_links
audit_logs
worker_heartbeats
```

All customer-owned records carry `tenant_id`. Unique constraints enforce idempotency and prevent duplicate memberships, permissions, runs and deliveries.

## 7. API Boundary

Base path: `/api/v1`

### Public/System

- `GET /health/live`
- `GET /health/ready`

### Admin Auth

- `POST /auth/admin/login`
- `POST /auth/admin/logout`
- `GET /auth/admin/session`

### Admin Resources

- `/admin/tenants`
- `/admin/tenants/{tenantId}/sml-connection`
- `/admin/tenants/{tenantId}/sml-connection/test`
- `/admin/tenants/{tenantId}/recipients`
- `/admin/tenants/{tenantId}/report-permissions`
- `/admin/tenants/{tenantId}/schedules`
- `/admin/tenants/{tenantId}/line-channel`
- `/admin/report-runs`
- `/admin/line-deliveries`
- `/admin/audit-logs`

### Viewer/LIFF

- `POST /viewer/line/session`
- `POST /viewer/logout`
- `GET /viewer/me`
- `GET /viewer/tenants`
- `GET /viewer/tenants/{tenantId}/reports`
- `GET /viewer/tenants/{tenantId}/reports/{reportKey}/latest`
- `GET /viewer/tenants/{tenantId}/reports/{reportKey}/runs/{runId}`

All errors use one structure:

```json
{
  "error": {
    "code": "FORBIDDEN",
    "message": "You do not have access to this report.",
    "requestId": "..."
  }
}
```

## 8. Technology Decisions

### Frontend

- Vue 3 and Vite
- Sakai Vue 5.0.0 source template
- PrimeVue 4, PrimeIcons and Tailwind PrimeUI
- Vue Router
- Pinia only when cross-page state is required
- Vitest and Vue Test Utils
- Nginx static serving and same-origin `/api` reverse proxy

### Backend

- Go current stable release, pinned at implementation time
- `net/http` with a small router such as Chi
- PostgreSQL 16 through `pgx`
- Embedded, versioned SQL migrations
- Separate API and worker commands sharing internal packages
- Standard structured logging with secret redaction
- Table-driven unit tests and PostgreSQL integration tests

## 9. Deployment Model

```text
Internet / HTTPS domain
  -> reverse proxy/TLS
  -> nextstep-frontend :6324
       -> /api proxy to nextstep-backend:8080

nextstep-worker
  -> nextstep-backend/shared packages
  -> nextstep-postgres
  -> SML Java Web Services
  -> LINE Messaging API
```

Production path:

```text
/mnt/data/nextstep-dashboard/
  docker-compose.yml
  .env
  backups/
```

Only frontend port `6324` is published. Backend and PostgreSQL remain private on the project Docker network. Compose project name, network, container names and volumes are unique to Nextstep Dashboard.

## 10. CI/CD and Release

- Frontend and backend have independent GitHub Actions
- Pull request gate: lint/static checks, tests, build and dependency audit
- Push to `main`: build image and push to GHCR
- Image tags: immutable commit SHA plus moving `main`
- Server uses a GHCR read-only credential
- Normal release: `docker compose pull && docker compose up -d`
- Rollback: pin the previous immutable SHA and run compose again
- Compose/migration changes require an explicit release note and pre-deploy backup

## 11. LINE Console Sequence

1. Deploy application and HTTPS domain
2. Link `NEXT STEP OFFICER` to `Next Dashboard Login`
3. Configure LINE Login callback URL
4. Add LIFF app with `profile` and `openid`
5. Configure LIFF endpoint to the deployed HTTPS viewer
6. Test with channel-role accounts while status is Developing
7. Verify copied-link denial and message delivery
8. Publish LINE Login channel only after security and privacy review

The existing Nexflow webhook must not be changed.

## 12. Security Boundaries

- No secret values in source, commits, CI logs or API responses
- Channel secret supplied during discovery is treated as exposed and must be rotated through a coordinated change with Nexflow before final production launch
- Admin password stored as a strong hash in environment, never plaintext
- Auth and sensitive mutation endpoints rate limited
- Session cookies are secure/httpOnly/sameSite
- SML and LINE calls use timeouts, bounded retries and redacted errors
- Every tenant/report query is authorized server-side
- PostgreSQL is not exposed on the host network
- Production backup/restore is tested before enabling schedules

## 13. Acceptance Criteria

- Platform Admin can create, disable and expire a tenant
- Admin can configure and test a tenant SML Java Web Service connection
- All 10 report contracts pass fixture and parity tests
- Admin can create multiple schedules with report/recipient selection
- Each recipient receives one numeric Flex bubble containing only authorized reports
- Copied LIFF links fail for a different LINE identity
- Viewer menu and API expose only authorized reports
- Failed reports do not send stale numbers as fresh
- GitHub Actions publish both images to GHCR
- Production deploy uses isolated Docker resources and port 6324 without changing other projects
- Existing Nexflow webhook continues operating unchanged

## 14. Decisions Required Before Implementation

1. Confirm public HTTPS hostname. Proposed: `dashboard.nextstep-soft.com`.
2. Confirm how TLS/DNS will route hostname port 443 to frontend port 6324 without modifying another project's proxy unexpectedly.
3. Confirm whether the user-supplied bootstrap password is allowed only for bootstrap or must remain after public launch. Security recommendation: bootstrap only and never record the plaintext value in git.
4. Confirm default retention: proposed 90 days for report snapshots and one year for audit/delivery metadata.
