---
status: current
last_verified: 2026-07-15
source_of_truth: [internal/report/catalog.go, internal/tenant/model.go, internal/schedule/service.go, api/openapi.yaml]
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

| Key | Thai label | Period mode | Refresh class |
| --- | --- | --- | --- |
| `sales_goods_services` | รายงานขายสินค้าและบริการ | `DATE_RANGE` | FAST |
| `purchase_goods_payables` | รายงานซื้อสินค้าและตั้งหนี้ | `DATE_RANGE` | STANDARD |
| `gross_profit_by_product` | กำไรขั้นต้นตามสินค้า | `DATE_RANGE` | STANDARD |
| `gross_profit_by_ar_customer` | กำไรขั้นต้นตามลูกหนี้ | `DATE_RANGE` | STANDARD |
| `stock_balance` | รายงานสต็อกคงเหลือ | `AS_OF_DATE` | HEAVY |
| `stock_reorder` | รายงานสินค้าถึงจุดสั่งซื้อ | `CURRENT_ONLY` | STANDARD |
| `ar_customer_movement` | รายงานความเคลื่อนไหวลูกหนี้ | `AS_OF_DATE` | HEAVY |
| `ar_debt_receipt` | รายงานรับชำระหนี้ | `DATE_RANGE` | FAST |
| `cash_bank_receipts` | รายงานรับเงิน | `DATE_RANGE` | FAST |
| `cash_bank_payments` | รายงานจ่ายเงิน | `DATE_RANGE` | FAST |

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
