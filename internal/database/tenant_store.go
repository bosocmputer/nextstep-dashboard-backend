package database

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TenantStore struct {
	pool        *pgxpool.Pool
	slugEntropy io.Reader
}

func NewTenantStore(pool *pgxpool.Pool) *TenantStore {
	return &TenantStore{pool: pool, slugEntropy: rand.Reader}
}

func (store *TenantStore) Create(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, input tenant.CreateInput, now time.Time) (tenant.Tenant, error) {
	requestBytes, err := json.Marshal(input)
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("encode tenant create request: %w", err)
	}
	requestHash := sha256.Sum256(requestBytes)
	actorScope := "admin:create-tenant:" + hex.EncodeToString(actorHash)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("begin create tenant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		insert into idempotency_requests (actor_scope, idempotency_key, request_hash, expires_at)
		values ($1, $2, $3, $4)
		on conflict (actor_scope, idempotency_key) do nothing`, actorScope, idempotencyKey, requestHash[:], now.Add(24*time.Hour)); err != nil {
		return tenant.Tenant{}, fmt.Errorf("reserve tenant idempotency key: %w", err)
	}
	var storedHash []byte
	var responseJSON []byte
	if err := tx.QueryRow(ctx, `
		select request_hash, response_json
		from idempotency_requests
		where actor_scope = $1 and idempotency_key = $2
		for update`, actorScope, idempotencyKey).Scan(&storedHash, &responseJSON); err != nil {
		return tenant.Tenant{}, fmt.Errorf("read tenant idempotency key: %w", err)
	}
	if subtle.ConstantTimeCompare(storedHash, requestHash[:]) != 1 {
		return tenant.Tenant{}, tenant.ErrIdempotencyConflict
	}
	if len(responseJSON) > 0 {
		var replay tenant.Tenant
		if err := json.Unmarshal(responseJSON, &replay); err != nil {
			return tenant.Tenant{}, fmt.Errorf("decode idempotent tenant response: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return tenant.Tenant{}, fmt.Errorf("commit tenant replay: %w", err)
		}
		return replay, nil
	}

	created, err := insertTenant(ctx, tx, input, now, store.slugEntropy)
	if err != nil {
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "23505" {
			return tenant.Tenant{}, tenant.ErrConflict
		}
		return tenant.Tenant{}, err
	}
	responseJSON, err = json.Marshal(created)
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("encode tenant response: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update idempotency_requests
		set response_status = 201, response_json = $3
		where actor_scope = $1 and idempotency_key = $2`, actorScope, idempotencyKey, responseJSON); err != nil {
		return tenant.Tenant{}, fmt.Errorf("complete tenant idempotency record: %w", err)
	}
	if err := insertAudit(ctx, tx, created.ID, actorHash, "TENANT_CREATED", "TENANT", created.ID.String(), requestID, nil, responseJSON, now); err != nil {
		return tenant.Tenant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return tenant.Tenant{}, fmt.Errorf("commit create tenant: %w", err)
	}
	return created, nil
}

func insertTenant(ctx context.Context, tx pgx.Tx, input tenant.CreateInput, now time.Time, entropy io.Reader) (tenant.Tenant, error) {
	if input.Slug != "" {
		created, inserted, err := insertTenantRecord(ctx, tx, input, now)
		if err != nil {
			return tenant.Tenant{}, err
		}
		if !inserted {
			return tenant.Tenant{}, tenant.ErrConflict
		}
		return created, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		slug, err := generateTenantSlug(entropy)
		if err != nil {
			return tenant.Tenant{}, fmt.Errorf("generate tenant slug: %w", err)
		}
		input.Slug = slug
		created, inserted, err := insertTenantRecord(ctx, tx, input, now)
		if err != nil {
			return tenant.Tenant{}, err
		}
		if inserted {
			return created, nil
		}
	}
	return tenant.Tenant{}, tenant.ErrConflict
}

func insertTenantRecord(ctx context.Context, tx pgx.Tx, input tenant.CreateInput, now time.Time) (tenant.Tenant, bool, error) {
	var created tenant.Tenant
	created.SMLReadiness = "UNCONFIGURED"
	err := tx.QueryRow(ctx, `
		insert into tenants (slug, name, timezone, status, access_ends_at, created_at, updated_at)
		values ($1, $2, $3, $4, $5, $6, $6)
		on conflict (slug) do nothing
		returning id, slug, name, timezone, status, access_ends_at, version, created_at, updated_at`,
		input.Slug, input.Name, input.Timezone, input.Status, input.AccessEndsAt, now,
	).Scan(&created.ID, &created.Slug, &created.Name, &created.Timezone, &created.Status, &created.AccessEndsAt, &created.Version, &created.CreatedAt, &created.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.Tenant{}, false, nil
	}
	if err != nil {
		return tenant.Tenant{}, false, fmt.Errorf("insert tenant: %w", err)
	}
	return created, true, nil
}

var tenantSlugEncoding = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func generateTenantSlug(entropy io.Reader) (string, error) {
	value := make([]byte, 8)
	if _, err := io.ReadFull(entropy, value); err != nil {
		return "", err
	}
	return "shop-" + strings.ToLower(tenantSlugEncoding.EncodeToString(value)[:12]), nil
}

func (store *TenantStore) List(ctx context.Context, filter tenant.ListFilter, now time.Time) (tenant.Page, error) {
	var cursorTime *time.Time
	var cursorID *uuid.UUID
	if filter.Cursor != "" {
		decodedTime, decodedID, err := decodeTenantCursor(filter.Cursor)
		if err != nil {
			return tenant.Page{}, &tenant.ValidationError{Field: "cursor", Code: "INVALID_CURSOR", Message: "Cursor is invalid."}
		}
		cursorTime, cursorID = &decodedTime, &decodedID
	}
	var status *string
	if filter.Status != nil {
		value := string(*filter.Status)
		status = &value
	}
	rows, err := store.pool.Query(ctx, `
		select t.id, t.slug, t.name, t.timezone,
		       case when t.access_ends_at <= $1 then 'EXPIRED' else t.status end as effective_status,
		       t.access_ends_at, t.version,
		       coalesce(s.readiness_status, 'UNCONFIGURED') as sml_readiness,
		       (select min(ns.next_run_at) from notification_schedules ns where ns.tenant_id = t.id and ns.status = 'ACTIVE'),
		       t.created_at, t.updated_at
		from tenants t
		left join tenant_sml_connections s on s.tenant_id = t.id
		where t.archived_at is null
		  and ($2::text is null or (case when t.access_ends_at <= $1 then 'EXPIRED' else t.status end) = $2)
		  and ($3 = '' or t.name ilike '%' || $3 || '%' or t.slug ilike '%' || $3 || '%')
		  and ($4::timestamptz is null or (t.updated_at, t.id) < ($4, $5))
		order by t.updated_at desc, t.id desc
		limit $6`, now, status, filter.Search, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return tenant.Page{}, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	items := make([]tenant.Tenant, 0, filter.PageSize+1)
	for rows.Next() {
		item, err := scanTenant(rows)
		if err != nil {
			return tenant.Page{}, fmt.Errorf("scan tenant list: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return tenant.Page{}, fmt.Errorf("iterate tenants: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		nextCursor = encodeTenantCursor(last.UpdatedAt, last.ID)
	}
	return tenant.Page{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func (store *TenantStore) Get(ctx context.Context, id uuid.UUID, now time.Time) (tenant.Tenant, error) {
	row := store.pool.QueryRow(ctx, `
		select t.id, t.slug, t.name, t.timezone,
		       case when t.access_ends_at <= $2 then 'EXPIRED' else t.status end,
		       t.access_ends_at, t.version,
		       coalesce(s.readiness_status, 'UNCONFIGURED'),
		       (select min(ns.next_run_at) from notification_schedules ns where ns.tenant_id = t.id and ns.status = 'ACTIVE'),
		       t.created_at, t.updated_at
		from tenants t
		left join tenant_sml_connections s on s.tenant_id = t.id
		where t.id = $1 and t.archived_at is null`, id, now)
	item, err := scanTenant(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.Tenant{}, tenant.ErrNotFound
	}
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("get tenant: %w", err)
	}
	return item, nil
}

func (store *TenantStore) Update(ctx context.Context, actorHash []byte, requestID string, id uuid.UUID, patch tenant.PatchInput, now time.Time) (tenant.Tenant, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("begin update tenant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var before tenant.Tenant
	err = tx.QueryRow(ctx, `
		select id, slug, name, timezone, status, access_ends_at, version, created_at, updated_at
		from tenants where id = $1 and archived_at is null for update`, id).Scan(
		&before.ID, &before.Slug, &before.Name, &before.Timezone, &before.Status,
		&before.AccessEndsAt, &before.Version, &before.CreatedAt, &before.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.Tenant{}, tenant.ErrNotFound
	}
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("lock tenant for update: %w", err)
	}
	if before.Version != patch.Version {
		return tenant.Tenant{}, tenant.ErrConflict
	}
	name, timezone, status, accessEndsAt := before.Name, before.Timezone, before.Status, before.AccessEndsAt
	if patch.Name != nil {
		name = *patch.Name
	}
	if patch.Timezone != nil {
		timezone = *patch.Timezone
	}
	if patch.Status != nil {
		status = *patch.Status
	}
	if patch.AccessEndsAt != nil {
		accessEndsAt = *patch.AccessEndsAt
	}
	var updated tenant.Tenant
	updated.SMLReadiness = "UNCONFIGURED"
	err = tx.QueryRow(ctx, `
		update tenants
		set name = $2, timezone = $3, status = $4, access_ends_at = $5, version = version + 1, updated_at = $6
		where id = $1
		returning id, slug, name, timezone,
		          case when access_ends_at <= $6 then 'EXPIRED' else status end,
		          access_ends_at, version, created_at, updated_at`,
		id, name, timezone, status, accessEndsAt, now,
	).Scan(&updated.ID, &updated.Slug, &updated.Name, &updated.Timezone, &updated.Status, &updated.AccessEndsAt, &updated.Version, &updated.CreatedAt, &updated.UpdatedAt)
	if err != nil {
		return tenant.Tenant{}, fmt.Errorf("update tenant: %w", err)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(updated)
	if err := insertAudit(ctx, tx, id, actorHash, "TENANT_UPDATED", "TENANT", id.String(), requestID, beforeJSON, afterJSON, now); err != nil {
		return tenant.Tenant{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return tenant.Tenant{}, fmt.Errorf("commit update tenant: %w", err)
	}
	return updated, nil
}

func (store *TenantStore) Archive(ctx context.Context, actorHash []byte, requestID string, id uuid.UUID, version int, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin archive tenant: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockTenantRecipientScheduleMutation(ctx, tx, id); err != nil {
		return err
	}

	var before tenant.Tenant
	err = tx.QueryRow(ctx, `
		select id, slug, name, timezone, status, access_ends_at, version, created_at, updated_at
		from tenants
		where id = $1 and archived_at is null
		for update`, id).Scan(
		&before.ID, &before.Slug, &before.Name, &before.Timezone, &before.Status,
		&before.AccessEndsAt, &before.Version, &before.CreatedAt, &before.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock tenant for archive: %w", err)
	}
	if before.Version != version {
		return tenant.ErrConflict
	}

	if _, err := tx.Exec(ctx, `
		update notification_schedules
		set status = 'PAUSED', next_run_at = null, version = version + 1, updated_at = $2
		where tenant_id = $1 and status = 'ACTIVE'`, id, now); err != nil {
		return fmt.Errorf("pause archived tenant schedules: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update report_runs
		set status = 'CANCELLED', safe_error_code = 'TENANT_ARCHIVED', finished_at = $2, updated_at = $2
		where tenant_id = $1 and status = 'QUEUED'`, id, now); err != nil {
		return fmt.Errorf("cancel archived tenant report queue: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update dashboard_refreshes
		set status = 'FAILED', failed = total, finished_at = $2, updated_at = $2
		where tenant_id = $1 and status = 'QUEUED'`, id, now); err != nil {
		return fmt.Errorf("cancel archived tenant dashboard refreshes: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update line_recipients recipient
		set status = 'REVOKED', updated_at = $2
		where recipient.status = 'PENDING'
		  and exists (
		    select 1 from tenant_memberships membership
		    where membership.tenant_id = $1 and membership.recipient_id = recipient.id
		  )`, id, now); err != nil {
		return fmt.Errorf("revoke archived tenant pending recipients: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update recipient_invitations
		set used_at = $2
		where tenant_id = $1 and used_at is null`, id, now); err != nil {
		return fmt.Errorf("expire archived tenant invitations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update tenant_memberships
		set status = 'REVOKED', updated_at = $2
		where tenant_id = $1 and status <> 'REVOKED'`, id, now); err != nil {
		return fmt.Errorf("revoke archived tenant memberships: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update tenants
		set status = 'DISABLED', archived_at = $2, version = version + 1, updated_at = $2
		where id = $1`, id, now); err != nil {
		return fmt.Errorf("archive tenant: %w", err)
	}

	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(map[string]any{"status": tenant.StatusDisabled, "archivedAt": now})
	if err := insertAudit(ctx, tx, id, actorHash, "TENANT_ARCHIVED", "TENANT", id.String(), requestID, beforeJSON, afterJSON, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit archive tenant: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(...any) error
}

func scanTenant(row rowScanner) (tenant.Tenant, error) {
	var item tenant.Tenant
	err := row.Scan(
		&item.ID, &item.Slug, &item.Name, &item.Timezone, &item.Status,
		&item.AccessEndsAt, &item.Version, &item.SMLReadiness, &item.NextScheduleAt,
		&item.CreatedAt, &item.UpdatedAt,
	)
	return item, err
}

func insertAudit(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, actorHash []byte, action, resourceType, resourceID, requestID string, beforeJSON, afterJSON []byte, now time.Time) error {
	_, err := tx.Exec(ctx, `
		insert into audit_logs (
		  tenant_id, actor_type, actor_id_hash, action, resource_type, resource_id,
		  request_id, before_json, after_json, result, created_at, expires_at
		) values ($1, 'ADMIN', $2, $3, $4, $5, $6, $7, $8, 'SUCCESS', $9, $10)`,
		tenantID, actorHash, action, resourceType, resourceID, requestID,
		nullableJSON(beforeJSON), nullableJSON(afterJSON), now, now.AddDate(1, 0, 0),
	)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	return nil
}

func nullableJSON(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func encodeTenantCursor(updatedAt time.Time, id uuid.UUID) string {
	value := updatedAt.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeTenantCursor(cursor string) (time.Time, uuid.UUID, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errors.New("invalid cursor parts")
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return updatedAt, id, nil
}
