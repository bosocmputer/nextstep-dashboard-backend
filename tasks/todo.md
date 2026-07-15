# Nextstep Dashboard Backend Todo

## Gate 0 — Plan

- [ ] User approves Blueprint and implementation plan
- [ ] Confirm HTTPS hostname and TLS routing
- [ ] Confirm bootstrap admin password policy
- [ ] Confirm retention periods

## Gate 1 — Foundation

- [ ] Go service/config/health
- [ ] PostgreSQL migrations/repositories
- [ ] Platform Admin auth/session/rate limit

## Gate 2 — Tenant/SML

- [ ] Tenant lifecycle API
- [ ] Encrypted SML configuration
- [ ] SML Java Web Service connection test

## Gate 3 — Reports

- [ ] Report registry and execution contract
- [ ] Sales and purchase
- [ ] Gross profit product/customer
- [ ] Stock balance/reorder
- [ ] AR movement/debt receipt
- [ ] Cash/bank receipt/payment
- [ ] Run/snapshot persistence and parity tests

## Gate 4 — Scheduling and LINE

- [ ] Recipients and memberships
- [ ] Report permissions
- [ ] Notification schedules
- [ ] Worker claim/idempotency/retry
- [ ] LINE channel routing
- [ ] Numeric Flex renderer
- [ ] LINE sender and delivery history

## Gate 5 — Viewer

- [ ] LINE token verification
- [ ] Recipient-bound session/access links
- [ ] Permission-filtered viewer APIs

## Gate 6 — Operations/Deploy

- [ ] Audit/history/retention
- [ ] Backup/restore drill
- [ ] Backend CI and GHCR
- [ ] Production Compose and bootstrap
- [ ] Complete LINE Login/LIFF configuration
- [ ] End-to-end release gate

