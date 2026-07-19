---
status: current
last_verified: 2026-07-19
source_of_truth: [internal/report/catalog.go, internal/tenant/model.go, internal/schedule/service.go, internal/tablequery/service.go, api/openapi.yaml]
tags: [backend, domain, reports]
---

# Domain and Reports

## Core Vocabulary

| Term | Meaning |
| --- | --- |
| Tenant | One store/customer boundary with status, access end, timezone, and SML connection |
| Recipient | Verified LINE identity that may join one or more tenants |
| Membership | Recipient participation in one tenant; may exist before report permissions are granted |
| Report permission | Maximum report set the recipient may open in tenant scope |
| Schedule | Selected reports, recipients, days, local time, and period policy for future LINE occurrences |
| Report run | One approved report/projection/period execution with queue, lease, progress, and result state |
| Snapshot/generation | Bounded dashboard output for a precise report set and resolved period |
| Notification run | Immutable materialized report set for one schedule occurrence |
| Delivery | Per-recipient LINE send state and delivery-scoped access context |

Permission does not automatically send a report. A LINE occurrence contains only the schedule report set, and every selected recipient must be eligible for that complete set.

## Approved Report Catalog

This block is generated from `report.Definitions()`. Do not edit it manually.

<!-- BEGIN GENERATED: REPORT_CATALOG -->
| Key | Thai label | Version | Status | Period mode | Refresh class | Chunk safe |
| --- | --- | --- | --- | --- | --- | --- |
| `sales_goods_services` | รายงานขายสินค้าและบริการ | `1.0.0` | `ACTIVE` | `DATE_RANGE` | `FAST` | `false` |
| `purchase_goods_payables` | รายงานซื้อสินค้าและตั้งหนี้ | `1.0.0` | `ACTIVE` | `DATE_RANGE` | `STANDARD` | `false` |
| `gross_profit_by_product` | กำไรขั้นต้นตามสินค้า | `1.0.0` | `ACTIVE` | `DATE_RANGE` | `STANDARD` | `false` |
| `gross_profit_by_ar_customer` | กำไรขั้นต้นตามลูกหนี้ | `1.0.0` | `ACTIVE` | `DATE_RANGE` | `STANDARD` | `false` |
| `stock_balance` | รายงานสต็อกคงเหลือ | `1.0.0` | `ACTIVE` | `AS_OF_DATE` | `HEAVY` | `true` |
| `stock_reorder` | รายงานสินค้าถึงจุดสั่งซื้อ | `1.0.0` | `ACTIVE` | `CURRENT_ONLY` | `STANDARD` | `false` |
| `ar_customer_movement` | รายงานความเคลื่อนไหวลูกหนี้ | `1.0.0` | `ACTIVE` | `AS_OF_DATE` | `HEAVY` | `true` |
| `ar_debt_receipt` | รายงานรับชำระหนี้ | `1.0.0` | `ACTIVE` | `DATE_RANGE` | `FAST` | `false` |
| `cash_bank_receipts` | รายงานรับเงิน | `1.0.0` | `ACTIVE` | `DATE_RANGE` | `FAST` | `false` |
| `cash_bank_payments` | รายงานจ่ายเงิน | `1.0.0` | `ACTIVE` | `DATE_RANGE` | `FAST` | `false` |
<!-- END GENERATED: REPORT_CATALOG -->

The catalog owns labels, version, status, selection policy, period mode, metrics, timeout, range, refresh, and chunk eligibility. Never derive these rules from frontend metadata.

## Period Rules

- Backend resolves presets against `Asia/Bangkok` and validates a maximum 366-day range.
- Date-range reports accept resolved ranges.
- As-of reports use one effective date derived from the schedule/viewer selection.
- Current-only reports do not claim historical behavior.
- Schedule preview, due execution, and test send must share the effective-period resolver.

## Lifecycle Rules

- Tenant status/access end gates reporting, delivery, and viewer access.
- Recipient membership and permissions are versioned where concurrent admin edits can conflict.
- Active schedules cannot depend on permissions that are removed concurrently.
- Deprecated report keys remain readable for existing configuration but cannot be newly selected.
- Audit records operational actions without copying business output.
- Recipient query pages search at most the supported 500-recipient tenant set,
  return an exact filtered total, and preserve identifier/display-name values.
- Schedule listing accepts bounded name/status/archive filters before exact-page
  pagination; report and recipient eligibility rules are unchanged. Required
  schedule collections such as readiness blockers and next occurrences are
  serialized as empty arrays rather than `null`, including ready and archived
  rows.
