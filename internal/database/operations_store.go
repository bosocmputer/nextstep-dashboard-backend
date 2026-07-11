package database

import (
	"context"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type OperationsStore struct {
	pool *pgxpool.Pool
}

func NewOperationsStore(pool *pgxpool.Pool) *OperationsStore {
	return &OperationsStore{pool: pool}
}

func (store *OperationsStore) GetLineQuota(ctx context.Context, now time.Time) (operations.LineQuotaStatus, error) {
	return NewQuotaStore(store.pool).Get(ctx, now)
}

func (store *OperationsStore) ListReportRuns(ctx context.Context, filter operations.ReportRunFilter) (operations.ReportRunPage, error) {
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return operations.ReportRunPage{}, err
	}
	var status *string
	if filter.Status != nil {
		value := string(*filter.Status)
		status = &value
	}
	rows, err := store.pool.Query(ctx, `
		select `+reportRunColumns+`, (select name from tenants where id = r.tenant_id) from report_runs r
		where ($1::uuid is null or r.tenant_id = $1)
		  and ($2::text is null or r.status = $2)
		  and ($3::timestamptz is null or (r.created_at, r.id) < ($3, $4))
		order by r.created_at desc, r.id desc
		limit $5`, filter.TenantID, status, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return operations.ReportRunPage{}, fmt.Errorf("list admin report runs: %w", err)
	}
	defer rows.Close()
	items := make([]operations.ReportRun, 0, filter.PageSize+1)
	for rows.Next() {
		var tenantName string
		run, err := scanReportRunWithExtras(rows, filter.Now, &tenantName)
		if err != nil {
			return operations.ReportRunPage{}, fmt.Errorf("scan admin report run: %w", err)
		}
		items = append(items, operations.ReportRun{Run: run, TenantName: tenantName})
	}
	if err := rows.Err(); err != nil {
		return operations.ReportRunPage{}, fmt.Errorf("iterate admin report runs: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1].Run
		nextCursor = encodeTenantCursor(last.CreatedAt, last.ID)
	}
	return operations.ReportRunPage{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func (store *OperationsStore) ListDeliveries(ctx context.Context, filter operations.DeliveryFilter) (operations.DeliveryPage, error) {
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return operations.DeliveryPage{}, err
	}
	rows, err := store.pool.Query(ctx, `
		select delivery.id, delivery.tenant_id, tenant.name,
		       recipient.id, recipient.line_user_id_hash,
		       recipient.display_name_ciphertext, recipient.display_name_nonce, recipient.encryption_key_id,
		       delivery.status, delivery.attempt, delivery.safe_error_code, delivery.provider_request_id,
		       delivery.accepted_at, delivery.created_at, delivery.expires_at
		from line_deliveries delivery
		join tenants tenant on tenant.id = delivery.tenant_id
		join line_recipients recipient on recipient.id = delivery.recipient_id
		where ($1::uuid is null or delivery.tenant_id = $1)
		  and ($2::timestamptz is null or (delivery.created_at, delivery.id) < ($2, $3))
		order by delivery.created_at desc, delivery.id desc
		limit $4`, filter.TenantID, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return operations.DeliveryPage{}, fmt.Errorf("list LINE deliveries: %w", err)
	}
	defer rows.Close()
	items := make([]operations.Delivery, 0, filter.PageSize+1)
	for rows.Next() {
		var item operations.Delivery
		if err := rows.Scan(
			&item.ID, &item.TenantID, &item.TenantName,
			&item.StoredRecipient.ID, &item.StoredRecipient.LineUserIDHash,
			&item.StoredRecipient.DisplayName.Ciphertext, &item.StoredRecipient.DisplayName.Nonce, &item.StoredRecipient.DisplayName.KeyID,
			&item.Status, &item.Attempt, &item.SafeErrorCode, &item.ProviderRequestID,
			&item.AcceptedAt, &item.CreatedAt, &item.ExpiresAt,
		); err != nil {
			return operations.DeliveryPage{}, fmt.Errorf("scan LINE delivery: %w", err)
		}
		item.StoredRecipient.TenantID = item.TenantID
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return operations.DeliveryPage{}, fmt.Errorf("iterate LINE deliveries: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1]
		nextCursor = encodeTenantCursor(last.CreatedAt, last.ID)
	}
	return operations.DeliveryPage{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func (store *OperationsStore) ListAudit(ctx context.Context, filter operations.AuditFilter) (operations.AuditPage, error) {
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return operations.AuditPage{}, err
	}
	rows, err := store.pool.Query(ctx, `
		select audit.id, audit.tenant_id, tenant.name, audit.actor_type, audit.action,
		       audit.resource_type, audit.resource_id, audit.result, audit.safe_error_code, audit.created_at
		from audit_logs audit
		left join tenants tenant on tenant.id = audit.tenant_id
		where ($1::uuid is null or audit.tenant_id = $1)
		  and ($2::timestamptz is null or (audit.created_at, audit.id) < ($2, $3))
		order by audit.created_at desc, audit.id desc
		limit $4`, filter.TenantID, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return operations.AuditPage{}, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	items := make([]operations.AuditEvent, 0, filter.PageSize+1)
	for rows.Next() {
		var item operations.AuditEvent
		if err := rows.Scan(&item.ID, &item.TenantID, &item.TenantName, &item.ActorType, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.SafeErrorCode, &item.CreatedAt); err != nil {
			return operations.AuditPage{}, fmt.Errorf("scan audit event: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return operations.AuditPage{}, fmt.Errorf("iterate audit events: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1]
		nextCursor = encodeTenantCursor(last.CreatedAt, last.ID)
	}
	return operations.AuditPage{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func operationsCursor(cursor string) (*time.Time, *uuid.UUID, error) {
	if cursor == "" {
		return nil, nil, nil
	}
	valueTime, valueID, err := decodeTenantCursor(cursor)
	if err != nil {
		return nil, nil, operations.ErrInvalidCursor
	}
	return &valueTime, &valueID, nil
}
