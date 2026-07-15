package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ViewerStore struct {
	pool *pgxpool.Pool
}

func NewViewerStore(pool *pgxpool.Pool) *ViewerStore {
	return &ViewerStore{pool: pool}
}

func (store *ViewerStore) CreateSession(ctx context.Context, session viewer.SessionRecord) error {
	_, err := store.pool.Exec(ctx, `
		insert into viewer_sessions (id_hash, recipient_id, csrf_hash, expires_at)
		values ($1, $2, $3, $4)`, session.TokenHash, session.RecipientID, session.CSRFHash, session.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create viewer session: %w", err)
	}
	return nil
}

func (store *ViewerStore) FindSession(ctx context.Context, tokenHash []byte, now time.Time) (viewer.SessionRecord, error) {
	var session viewer.SessionRecord
	err := store.pool.QueryRow(ctx, `
		select s.id_hash, s.recipient_id, s.csrf_hash, s.expires_at, s.revoked_at
		from viewer_sessions s
		join line_recipients r on r.id = s.recipient_id and r.status = 'ACTIVE'
		where s.id_hash = $1 and s.revoked_at is null and s.expires_at > $2`, tokenHash, now).Scan(
		&session.TokenHash, &session.RecipientID, &session.CSRFHash, &session.ExpiresAt, &session.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return viewer.SessionRecord{}, viewer.ErrSessionInvalid
	}
	if err != nil {
		return viewer.SessionRecord{}, fmt.Errorf("find viewer session: %w", err)
	}
	return session, nil
}

func (store *ViewerStore) RevokeSession(ctx context.Context, tokenHash []byte, now time.Time) error {
	if _, err := store.pool.Exec(ctx, `update viewer_sessions set revoked_at = coalesce(revoked_at, $2) where id_hash = $1`, tokenHash, now); err != nil {
		return fmt.Errorf("revoke viewer session: %w", err)
	}
	return nil
}

func (store *ViewerStore) ListTenants(ctx context.Context, recipientID uuid.UUID, now time.Time) ([]viewer.TenantAccess, error) {
	rows, err := store.pool.Query(ctx, `
		select t.id, t.name, t.timezone,
		       coalesce(array_agg(p.report_key order by p.report_key) filter (where p.report_key is not null), '{}')
		from tenant_memberships m
		join tenants t on t.id = m.tenant_id
		join line_recipients r on r.id = m.recipient_id and r.status = 'ACTIVE'
		left join recipient_report_permissions p on p.tenant_id = m.tenant_id and p.recipient_id = m.recipient_id
		where m.recipient_id = $1 and m.status = 'ACTIVE'
		  and t.status = 'ACTIVE' and t.access_ends_at > $2
		group by t.id
		order by t.name, t.id`, recipientID, now)
	if err != nil {
		return nil, fmt.Errorf("list viewer tenants: %w", err)
	}
	defer rows.Close()
	items := make([]viewer.TenantAccess, 0)
	for rows.Next() {
		var item viewer.TenantAccess
		var keys []string
		if err := rows.Scan(&item.ID, &item.Name, &item.Timezone, &keys); err != nil {
			return nil, fmt.Errorf("scan viewer tenant: %w", err)
		}
		item.ReportKeys = make([]report.Key, len(keys))
		for index, key := range keys {
			item.ReportKeys[index] = report.Key(key)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *ViewerStore) ListReports(ctx context.Context, recipientID, tenantID uuid.UUID, now time.Time) ([]viewer.ReportAccess, error) {
	rows, err := store.pool.Query(ctx, `
		select d.report_key, d.version, d.label_th, d.category, d.is_sensitive
		from recipient_report_permissions p
		join report_definitions d on d.report_key = p.report_key
		join tenant_memberships m on m.tenant_id = p.tenant_id and m.recipient_id = p.recipient_id and m.status = 'ACTIVE'
		join line_recipients r on r.id = p.recipient_id and r.status = 'ACTIVE'
		join tenants t on t.id = p.tenant_id and t.status = 'ACTIVE' and t.access_ends_at > $3
		where p.recipient_id = $1 and p.tenant_id = $2
		order by d.report_key`, recipientID, tenantID, now)
	if err != nil {
		return nil, fmt.Errorf("list viewer reports: %w", err)
	}
	defer rows.Close()
	items := make([]viewer.ReportAccess, 0)
	for rows.Next() {
		var item viewer.ReportAccess
		if err := rows.Scan(&item.Key, &item.Version, &item.Label, &item.Category, &item.IsSensitive); err != nil {
			return nil, fmt.Errorf("scan viewer report: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (store *ViewerStore) CanAccessReport(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, now time.Time) (bool, error) {
	var allowed bool
	err := store.pool.QueryRow(ctx, `
		select exists (
		  select 1
		  from recipient_report_permissions p
		  join tenant_memberships m on m.tenant_id = p.tenant_id and m.recipient_id = p.recipient_id and m.status = 'ACTIVE'
		  join tenants t on t.id = p.tenant_id and t.status = 'ACTIVE' and t.access_ends_at > $4
		  join line_recipients r on r.id = p.recipient_id and r.status = 'ACTIVE'
		  where p.recipient_id = $1 and p.tenant_id = $2 and p.report_key = $3
		)`, recipientID, tenantID, reportKey, now).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("check viewer report permission: %w", err)
	}
	return allowed, nil
}
