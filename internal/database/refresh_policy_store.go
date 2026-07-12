package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RefreshPolicyStore struct {
	pool *pgxpool.Pool
}

func NewRefreshPolicyStore(pool *pgxpool.Pool) *RefreshPolicyStore {
	return &RefreshPolicyStore{pool: pool}
}

func (store *RefreshPolicyStore) GetRefreshPolicy(ctx context.Context, tenantID uuid.UUID) (report.RefreshPolicy, error) {
	var policy report.RefreshPolicy
	var exists bool
	err := store.pool.QueryRow(ctx, `
		select $1::uuid, p.fast_interval_minutes, p.standard_interval_minutes,
		       p.heavy_interval_minutes, coalesce(p.version, 0), p.tenant_id is not null
		from tenants t
		left join tenant_dashboard_refresh_policies p on p.tenant_id = t.id
		where t.id = $1`, tenantID).Scan(
		&policy.TenantID, &policy.FastIntervalMinutes, &policy.StandardIntervalMinutes,
		&policy.HeavyIntervalMinutes, &policy.Version, &exists,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return report.RefreshPolicy{}, tenant.ErrNotFound
	}
	if err != nil {
		return report.RefreshPolicy{}, fmt.Errorf("get dashboard refresh policy: %w", err)
	}
	if !exists {
		return report.DefaultRefreshPolicy(tenantID), nil
	}
	return policy, nil
}

func (store *RefreshPolicyStore) PutRefreshPolicy(ctx context.Context, actorHash []byte, requestID string, policy report.RefreshPolicy, expectedVersion int, now time.Time) (report.RefreshPolicy, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return report.RefreshPolicy{}, fmt.Errorf("begin dashboard refresh policy update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenantExists bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from tenants where id = $1)`, policy.TenantID).Scan(&tenantExists); err != nil {
		return report.RefreshPolicy{}, fmt.Errorf("validate refresh policy tenant: %w", err)
	}
	if !tenantExists {
		return report.RefreshPolicy{}, tenant.ErrNotFound
	}
	var currentVersion int
	err = tx.QueryRow(ctx, `select version from tenant_dashboard_refresh_policies where tenant_id = $1 for update`, policy.TenantID).Scan(&currentVersion)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if expectedVersion != 0 {
			return report.RefreshPolicy{}, report.ErrRefreshPolicyConflict
		}
		_, err = tx.Exec(ctx, `
			insert into tenant_dashboard_refresh_policies (
			  tenant_id, fast_interval_minutes, standard_interval_minutes,
			  heavy_interval_minutes, version, created_at, updated_at
			) values ($1, $2, $3, $4, 1, $5, $5)`, policy.TenantID,
			policy.FastIntervalMinutes, policy.StandardIntervalMinutes, policy.HeavyIntervalMinutes, now)
	case err != nil:
		return report.RefreshPolicy{}, fmt.Errorf("lock dashboard refresh policy: %w", err)
	default:
		if expectedVersion != currentVersion {
			return report.RefreshPolicy{}, report.ErrRefreshPolicyConflict
		}
		_, err = tx.Exec(ctx, `
			update tenant_dashboard_refresh_policies
			set fast_interval_minutes = $2, standard_interval_minutes = $3,
			    heavy_interval_minutes = $4, version = version + 1, updated_at = $5
			where tenant_id = $1`, policy.TenantID, policy.FastIntervalMinutes,
			policy.StandardIntervalMinutes, policy.HeavyIntervalMinutes, now)
	}
	if err != nil {
		return report.RefreshPolicy{}, fmt.Errorf("save dashboard refresh policy: %w", err)
	}
	updated := report.RefreshPolicy{TenantID: policy.TenantID}
	if err := tx.QueryRow(ctx, `
		select tenant_id, fast_interval_minutes, standard_interval_minutes,
		       heavy_interval_minutes, version
		from tenant_dashboard_refresh_policies where tenant_id = $1`, policy.TenantID).Scan(
		&updated.TenantID, &updated.FastIntervalMinutes, &updated.StandardIntervalMinutes,
		&updated.HeavyIntervalMinutes, &updated.Version,
	); err != nil {
		return report.RefreshPolicy{}, fmt.Errorf("read saved dashboard refresh policy: %w", err)
	}
	auditJSON, _ := json.Marshal(map[string]any{
		"fastIntervalMinutes":     updated.FastIntervalMinutes,
		"standardIntervalMinutes": updated.StandardIntervalMinutes,
		"heavyIntervalMinutes":    updated.HeavyIntervalMinutes,
		"version":                 updated.Version,
	})
	if err := insertAudit(ctx, tx, policy.TenantID, actorHash, "DASHBOARD_REFRESH_POLICY_UPDATED", "TENANT", policy.TenantID.String(), requestID, nil, auditJSON, now); err != nil {
		return report.RefreshPolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return report.RefreshPolicy{}, fmt.Errorf("commit dashboard refresh policy update: %w", err)
	}
	return updated, nil
}
