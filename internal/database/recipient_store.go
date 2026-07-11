package database

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RecipientStore struct {
	pool *pgxpool.Pool
}

func NewRecipientStore(pool *pgxpool.Pool) *RecipientStore {
	return &RecipientStore{pool: pool}
}

func (store *RecipientStore) CreateInvitation(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, requestHash []byte, pending recipient.StoredRecipient, invitationHash []byte, expiresAt, now time.Time) (recipient.StoredRecipient, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("begin recipient invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	actorScope := "admin:create-recipient:" + pending.TenantID.String() + ":" + hex.EncodeToString(actorHash)
	if _, err := tx.Exec(ctx, `
		insert into idempotency_requests (actor_scope, idempotency_key, request_hash, expires_at)
		values ($1, $2, $3, $4)
		on conflict (actor_scope, idempotency_key) do nothing`, actorScope, idempotencyKey, requestHash, now.Add(24*time.Hour)); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("reserve recipient idempotency key: %w", err)
	}
	var storedHash, responseJSON []byte
	if err := tx.QueryRow(ctx, `
		select request_hash, response_json from idempotency_requests
		where actor_scope = $1 and idempotency_key = $2
		for update`, actorScope, idempotencyKey).Scan(&storedHash, &responseJSON); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("read recipient idempotency key: %w", err)
	}
	if subtle.ConstantTimeCompare(storedHash, requestHash) != 1 {
		return recipient.StoredRecipient{}, recipient.ErrIdempotencyConflict
	}
	if len(responseJSON) > 0 {
		var response struct {
			RecipientID uuid.UUID `json:"recipientId"`
		}
		if err := json.Unmarshal(responseJSON, &response); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("decode recipient idempotent response: %w", err)
		}
		replayed, err := loadRecipientForTenantAny(ctx, tx, pending.TenantID, response.RecipientID)
		if err != nil {
			return recipient.StoredRecipient{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("commit recipient invitation replay: %w", err)
		}
		return replayed, nil
	}
	if _, err := tx.Exec(ctx, `
		insert into line_recipients (
		  id, display_name_ciphertext, display_name_nonce, encryption_key_id,
		  status, created_at, updated_at
		) values ($1, $2, $3, $4, 'PENDING', $5, $5)`,
		pending.ID, pending.DisplayName.Ciphertext, pending.DisplayName.Nonce, pending.DisplayName.KeyID, now,
	); err != nil {
		return recipient.StoredRecipient{}, mapRecipientCreateError(err, "create pending recipient")
	}
	if _, err := tx.Exec(ctx, `
		insert into tenant_memberships (tenant_id, recipient_id, status, created_at, updated_at)
		values ($1, $2, 'PENDING', $3, $3)`, pending.TenantID, pending.ID, now); err != nil {
		return recipient.StoredRecipient{}, mapRecipientCreateError(err, "create pending recipient membership")
	}
	if _, err := tx.Exec(ctx, `
		insert into recipient_invitations (tenant_id, pending_recipient_id, reference_hash, created_at, expires_at)
		values ($1, $2, $3, $4, $5)`, pending.TenantID, pending.ID, invitationHash, now, expiresAt); err != nil {
		return recipient.StoredRecipient{}, mapRecipientCreateError(err, "create recipient invitation")
	}
	afterJSON, _ := json.Marshal(map[string]any{"recipientId": pending.ID, "status": recipient.StatusPending, "expiresAt": expiresAt})
	idempotentResponse, _ := json.Marshal(map[string]any{"recipientId": pending.ID})
	if _, err := tx.Exec(ctx, `
		update idempotency_requests set response_status = 201, response_json = $3
		where actor_scope = $1 and idempotency_key = $2`, actorScope, idempotencyKey, idempotentResponse); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("complete recipient idempotency key: %w", err)
	}
	if err := insertAudit(ctx, tx, pending.TenantID, actorHash, "RECIPIENT_INVITED", "LINE_RECIPIENT", pending.ID.String(), requestID, nil, afterJSON, now); err != nil {
		return recipient.StoredRecipient{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("commit recipient invitation: %w", err)
	}
	return pending, nil
}

func mapRecipientCreateError(err error, operation string) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) && postgresError.Code == "23505" {
		return recipient.ErrIdempotencyConflict
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (store *RecipientStore) List(ctx context.Context, tenantID uuid.UUID, pageSize int, cursor string) (recipient.Page, error) {
	var cursorTime *time.Time
	var cursorID *uuid.UUID
	if cursor != "" {
		valueTime, valueID, err := decodeTenantCursor(cursor)
		if err != nil {
			return recipient.Page{}, errors.New("recipient cursor is invalid")
		}
		cursorTime, cursorID = &valueTime, &valueID
	}
	rows, err := store.pool.Query(ctx, `
		select r.id, m.tenant_id, r.line_user_id_hash,
		       r.line_user_id_ciphertext, r.line_user_id_nonce,
		       r.display_name_ciphertext, r.display_name_nonce,
		       r.encryption_key_id, r.status, r.verified_at, r.created_at,
		       coalesce(array_agg(p.report_key order by p.report_key) filter (where p.report_key is not null), '{}'),
		       m.permissions_version
		from tenant_memberships m
		join line_recipients r on r.id = m.recipient_id
		left join recipient_report_permissions p on p.tenant_id = m.tenant_id and p.recipient_id = m.recipient_id
		where m.tenant_id = $1 and m.status <> 'REVOKED'
		  and ($2::timestamptz is null or (r.created_at, r.id) < ($2, $3))
		group by r.id, m.tenant_id, m.permissions_version
		order by r.created_at desc, r.id desc
		limit $4`, tenantID, cursorTime, cursorID, pageSize+1)
	if err != nil {
		return recipient.Page{}, fmt.Errorf("list recipients: %w", err)
	}
	defer rows.Close()
	items := make([]recipient.StoredRecipient, 0, pageSize+1)
	for rows.Next() {
		item, err := scanRecipient(rows)
		if err != nil {
			return recipient.Page{}, fmt.Errorf("scan recipient: %w", err)
		}
		items = append(items, item)
	}
	hasMore := len(items) > pageSize
	if hasMore {
		items = items[:pageSize]
	}
	nextCursor := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		nextCursor = encodeTenantCursor(last.CreatedAt, last.ID)
	}
	return recipient.Page{Stored: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func (store *RecipientStore) ReplacePermissions(ctx context.Context, actorHash []byte, requestID string, tenantID, recipientID uuid.UUID, keys []report.Key, version int, now time.Time) (recipient.StoredRecipient, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("begin recipient permissions: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var membershipStatus string
	var storedVersion int
	if err := tx.QueryRow(ctx, `
		select status, permissions_version from tenant_memberships
		where tenant_id = $1 and recipient_id = $2 and status <> 'REVOKED'
		for update`, tenantID, recipientID).Scan(&membershipStatus, &storedVersion); errors.Is(err, pgx.ErrNoRows) {
		return recipient.StoredRecipient{}, recipient.ErrRecipientNotFound
	} else if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("lock recipient membership: %w", err)
	}
	if storedVersion != version {
		return recipient.StoredRecipient{}, recipient.ErrVersionConflict
	}
	var existingKeyValues []string
	if err := tx.QueryRow(ctx, `
		select coalesce(array_agg(report_key), '{}')
		from recipient_report_permissions
		where tenant_id = $1 and recipient_id = $2`, tenantID, recipientID).Scan(&existingKeyValues); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("load existing recipient permissions: %w", err)
	}
	existingKeys := make(map[report.Key]struct{}, len(existingKeyValues))
	for _, key := range existingKeyValues {
		existingKeys[report.Key(key)] = struct{}{}
	}
	keyValues := make([]string, len(keys))
	for index, key := range keys {
		definition, ok := report.DefinitionFor(key)
		_, alreadySelected := existingKeys[key]
		if !ok || !report.CanSelect(definition, alreadySelected) {
			return recipient.StoredRecipient{}, recipient.ErrPermissionInvalid
		}
		keyValues[index] = string(key)
	}
	rows, err := tx.Query(ctx, `
		select distinct s.name
		from notification_schedules s
		join notification_schedule_recipients sr on sr.schedule_id = s.id and sr.recipient_id = $2
		join notification_schedule_reports scheduled on scheduled.schedule_id = s.id
		where s.tenant_id = $1 and s.status = 'ACTIVE'
		  and not (scheduled.report_key = any($3::text[]))
		order by s.name`, tenantID, recipientID, keyValues)
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("check active schedule permission dependencies: %w", err)
	}
	var scheduleNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return recipient.StoredRecipient{}, fmt.Errorf("scan active schedule permission dependency: %w", err)
		}
		scheduleNames = append(scheduleNames, name)
	}
	rows.Close()
	if len(scheduleNames) > 0 {
		return recipient.StoredRecipient{}, &recipient.PermissionInUseError{ScheduleNames: scheduleNames}
	}
	if _, err := tx.Exec(ctx, `delete from recipient_report_permissions where tenant_id = $1 and recipient_id = $2`, tenantID, recipientID); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("clear recipient permissions: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into recipient_report_permissions (tenant_id, recipient_id, report_key, created_at)
		select $1, $2, value, $4 from unnest($3::text[]) as value`, tenantID, recipientID, keyValues, now); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("insert recipient permissions: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update tenant_memberships set permissions_version = permissions_version + 1, updated_at = $3
		where tenant_id = $1 and recipient_id = $2`, tenantID, recipientID, now); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("advance recipient permission version: %w", err)
	}
	afterJSON, _ := json.Marshal(map[string]any{"reportKeys": keys})
	if err := insertAudit(ctx, tx, tenantID, actorHash, "RECIPIENT_PERMISSIONS_REPLACED", "REPORT_PERMISSION", recipientID.String(), requestID, nil, afterJSON, now); err != nil {
		return recipient.StoredRecipient{}, err
	}
	updated, err := loadRecipientForTenantAny(ctx, tx, tenantID, recipientID)
	if err != nil {
		return recipient.StoredRecipient{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("commit recipient permissions: %w", err)
	}
	return updated, nil
}

func (store *RecipientStore) RedeemInvitation(ctx context.Context, invitationHash, lineHash []byte, identity recipient.StoredRecipient, now time.Time) (recipient.StoredRecipient, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("begin redeem invitation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended(encode($1, 'hex'), 0))`, lineHash); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("lock LINE identity: %w", err)
	}
	var tenantID, pendingID uuid.UUID
	err = tx.QueryRow(ctx, `
		select tenant_id, pending_recipient_id
		from recipient_invitations
		where reference_hash = $1 and used_at is null and expires_at > $2
		for update`, invitationHash, now).Scan(&tenantID, &pendingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return recipient.StoredRecipient{}, recipient.ErrInvitationInvalid
	}
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("load recipient invitation: %w", err)
	}

	targetID := pendingID
	var existingID uuid.UUID
	err = tx.QueryRow(ctx, `select id from line_recipients where line_user_id_hash = $1 and status = 'ACTIVE' for update`, lineHash).Scan(&existingID)
	if err == nil && existingID != pendingID {
		targetID = existingID
		if _, err := tx.Exec(ctx, `
			update line_recipients
			set line_user_id_ciphertext = $2, line_user_id_nonce = $3,
			    display_name_ciphertext = $4, display_name_nonce = $5,
			    encryption_key_id = $6, verified_at = $7, updated_at = $7
			where id = $1`, targetID, identity.LineUserID.Ciphertext, identity.LineUserID.Nonce,
			identity.DisplayName.Ciphertext, identity.DisplayName.Nonce, identity.LineUserID.KeyID, now); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("refresh existing LINE recipient: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into tenant_memberships (tenant_id, recipient_id, status, created_at, updated_at)
			values ($1, $2, 'ACTIVE', $3, $3)
			on conflict (tenant_id, recipient_id) do update set status = 'ACTIVE', updated_at = excluded.updated_at`, tenantID, targetID, now); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("merge recipient membership: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into recipient_report_permissions (tenant_id, recipient_id, report_key, created_at)
			select tenant_id, $2, report_key, $3
			from recipient_report_permissions
			where tenant_id = $1 and recipient_id = $4
			on conflict do nothing`, tenantID, targetID, now, pendingID); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("merge recipient permissions: %w", err)
		}
		if _, err := tx.Exec(ctx, `update tenant_memberships set status = 'REVOKED', updated_at = $3 where tenant_id = $1 and recipient_id = $2`, tenantID, pendingID, now); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("revoke merged pending membership: %w", err)
		}
		if _, err := tx.Exec(ctx, `update line_recipients set status = 'REVOKED', updated_at = $2 where id = $1`, pendingID, now); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("revoke merged pending recipient: %w", err)
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			update line_recipients
			set line_user_id_hash = $2, line_user_id_ciphertext = $3, line_user_id_nonce = $4,
			    display_name_ciphertext = $5, display_name_nonce = $6,
			    encryption_key_id = $7, status = 'ACTIVE', verified_at = $8, updated_at = $8
			where id = $1`, pendingID, lineHash, identity.LineUserID.Ciphertext, identity.LineUserID.Nonce,
			identity.DisplayName.Ciphertext, identity.DisplayName.Nonce, identity.LineUserID.KeyID, now); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("activate invited recipient: %w", err)
		}
		if _, err := tx.Exec(ctx, `update tenant_memberships set status = 'ACTIVE', updated_at = $3 where tenant_id = $1 and recipient_id = $2`, tenantID, pendingID, now); err != nil {
			return recipient.StoredRecipient{}, fmt.Errorf("activate recipient membership: %w", err)
		}
	} else if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("find existing LINE recipient: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update recipient_invitations
		set used_at = $2, used_by_recipient_id = $3
		where reference_hash = $1`, invitationHash, now, targetID); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("complete recipient invitation: %w", err)
	}
	bound, err := loadRecipientForTenant(ctx, tx, tenantID, targetID)
	if err != nil {
		return recipient.StoredRecipient{}, err
	}
	afterJSON, _ := json.Marshal(map[string]any{"recipientId": targetID, "status": recipient.StatusActive})
	if err := insertAudit(ctx, tx, tenantID, lineHash, "RECIPIENT_IDENTITY_BOUND", "LINE_RECIPIENT", targetID.String(), "", nil, afterJSON, now); err != nil {
		return recipient.StoredRecipient{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("commit recipient invitation: %w", err)
	}
	return bound, nil
}

func (store *RecipientStore) FindByLineHash(ctx context.Context, lineHash []byte) (recipient.StoredRecipient, error) {
	row := store.pool.QueryRow(ctx, `
		select r.id, '00000000-0000-0000-0000-000000000000'::uuid, r.line_user_id_hash,
		       r.line_user_id_ciphertext, r.line_user_id_nonce,
		       r.display_name_ciphertext, r.display_name_nonce,
		       r.encryption_key_id, r.status, r.verified_at, r.created_at,
		       '{}'::text[], 1
		from line_recipients r
		where r.line_user_id_hash = $1 and r.status = 'ACTIVE'`, lineHash)
	item, err := scanRecipient(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return recipient.StoredRecipient{}, recipient.ErrRecipientNotFound
	}
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("find LINE recipient: %w", err)
	}
	return item, nil
}

func (store *RecipientStore) GetByID(ctx context.Context, recipientID uuid.UUID) (recipient.StoredRecipient, error) {
	row := store.pool.QueryRow(ctx, `
		select r.id, '00000000-0000-0000-0000-000000000000'::uuid, r.line_user_id_hash,
		       r.line_user_id_ciphertext, r.line_user_id_nonce,
		       r.display_name_ciphertext, r.display_name_nonce,
		       r.encryption_key_id, r.status, r.verified_at, r.created_at,
		       '{}'::text[], 1
		from line_recipients r
		where r.id = $1 and r.status = 'ACTIVE'`, recipientID)
	item, err := scanRecipient(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return recipient.StoredRecipient{}, recipient.ErrRecipientNotFound
	}
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("get LINE recipient: %w", err)
	}
	return item, nil
}

func (store *RecipientStore) GetForTenant(ctx context.Context, tenantID, recipientID uuid.UUID) (recipient.StoredRecipient, error) {
	row := store.pool.QueryRow(ctx, `
		select r.id, m.tenant_id, r.line_user_id_hash,
		       r.line_user_id_ciphertext, r.line_user_id_nonce,
		       r.display_name_ciphertext, r.display_name_nonce,
		       r.encryption_key_id, r.status, r.verified_at, r.created_at,
		       coalesce(array_agg(p.report_key order by p.report_key) filter (where p.report_key is not null), '{}'),
		       m.permissions_version
		from tenant_memberships m
		join line_recipients r on r.id = m.recipient_id
		left join recipient_report_permissions p on p.tenant_id = m.tenant_id and p.recipient_id = m.recipient_id
		where m.tenant_id = $1 and m.recipient_id = $2 and m.status <> 'REVOKED'
		group by r.id, m.tenant_id, m.permissions_version`, tenantID, recipientID)
	item, err := scanRecipient(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return recipient.StoredRecipient{}, recipient.ErrRecipientNotFound
	}
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("get recipient for tenant: %w", err)
	}
	return item, nil
}

func loadRecipientForTenant(ctx context.Context, tx pgx.Tx, tenantID, recipientID uuid.UUID) (recipient.StoredRecipient, error) {
	row := tx.QueryRow(ctx, `
		select r.id, m.tenant_id, r.line_user_id_hash,
		       r.line_user_id_ciphertext, r.line_user_id_nonce,
		       r.display_name_ciphertext, r.display_name_nonce,
		       r.encryption_key_id, r.status, r.verified_at, r.created_at,
		       coalesce(array_agg(p.report_key order by p.report_key) filter (where p.report_key is not null), '{}'),
		       m.permissions_version
		from tenant_memberships m
		join line_recipients r on r.id = m.recipient_id
		left join recipient_report_permissions p on p.tenant_id = m.tenant_id and p.recipient_id = m.recipient_id
		where m.tenant_id = $1 and m.recipient_id = $2 and m.status = 'ACTIVE'
		group by r.id, m.tenant_id, m.permissions_version`, tenantID, recipientID)
	item, err := scanRecipient(row)
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("load bound recipient: %w", err)
	}
	return item, nil
}

func loadRecipientForTenantAny(ctx context.Context, tx pgx.Tx, tenantID, recipientID uuid.UUID) (recipient.StoredRecipient, error) {
	row := tx.QueryRow(ctx, `
		select r.id, m.tenant_id, r.line_user_id_hash,
		       r.line_user_id_ciphertext, r.line_user_id_nonce,
		       r.display_name_ciphertext, r.display_name_nonce,
		       r.encryption_key_id, r.status, r.verified_at, r.created_at,
		       coalesce(array_agg(p.report_key order by p.report_key) filter (where p.report_key is not null), '{}'),
		       m.permissions_version
		from tenant_memberships m
		join line_recipients r on r.id = m.recipient_id
		left join recipient_report_permissions p on p.tenant_id = m.tenant_id and p.recipient_id = m.recipient_id
		where m.tenant_id = $1 and m.recipient_id = $2 and m.status <> 'REVOKED'
		group by r.id, m.tenant_id, m.permissions_version`, tenantID, recipientID)
	item, err := scanRecipient(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return recipient.StoredRecipient{}, recipient.ErrRecipientNotFound
	}
	if err != nil {
		return recipient.StoredRecipient{}, fmt.Errorf("load recipient idempotent replay: %w", err)
	}
	return item, nil
}

func scanRecipient(row rowScanner) (recipient.StoredRecipient, error) {
	var item recipient.StoredRecipient
	var keyID string
	var keys []string
	err := row.Scan(
		&item.ID, &item.TenantID, &item.LineUserIDHash,
		&item.LineUserID.Ciphertext, &item.LineUserID.Nonce,
		&item.DisplayName.Ciphertext, &item.DisplayName.Nonce,
		&keyID, &item.Status, &item.VerifiedAt, &item.CreatedAt, &keys, &item.PermissionsVersion,
	)
	item.LineUserID.KeyID = keyID
	item.DisplayName.KeyID = keyID
	item.ReportKeys = make([]report.Key, len(keys))
	for index, key := range keys {
		item.ReportKeys[index] = report.Key(key)
	}
	return item, err
}
