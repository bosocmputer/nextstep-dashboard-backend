package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/quota"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const quotaFreshness = 15 * time.Minute

type QuotaStore struct {
	pool *pgxpool.Pool
}

func NewQuotaStore(pool *pgxpool.Pool) *QuotaStore { return &QuotaStore{pool: pool} }

func (store *QuotaStore) Sync(ctx context.Context, usage line.QuotaUsage, now time.Time) (quota.Status, error) {
	month := quotaMonth(now)
	var status quota.Status
	var consumed int
	if err := store.pool.QueryRow(ctx, `
		insert into line_monthly_quota (
		  quota_month, provider_limit, provider_consumed, locally_accepted,
		  operational_reserve_percent, synced_at, updated_at
		) values ($1, $2, $3, 0, 10, $4, $4)
		on conflict (quota_month) do update
		set provider_limit = excluded.provider_limit,
		    provider_consumed = excluded.provider_consumed,
		    synced_at = excluded.synced_at,
		    updated_at = excluded.updated_at
		returning provider_limit, provider_consumed, locally_accepted,
		          operational_reserve_percent, synced_at`,
		month, usage.Limit, usage.Consumed, now).Scan(
		&status.ProviderLimit, &consumed, &status.LocallyAccepted,
		&status.OperationalReservePercent, &status.SyncedAt,
	); err != nil {
		return quota.Status{}, fmt.Errorf("sync LINE quota: %w", err)
	}
	status.ProviderConsumed = &consumed
	status.State = quotaState(status, now)
	return status, nil
}

func (store *QuotaStore) Get(ctx context.Context, now time.Time) (quota.Status, error) {
	var status quota.Status
	var consumed *int
	err := store.pool.QueryRow(ctx, `
		select provider_limit, provider_consumed, locally_accepted,
		       operational_reserve_percent, synced_at
		from line_monthly_quota where quota_month = $1`, quotaMonth(now)).Scan(
		&status.ProviderLimit, &consumed, &status.LocallyAccepted,
		&status.OperationalReservePercent, &status.SyncedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return quota.Status{State: quota.StateUnsynced, OperationalReservePercent: 10}, nil
	}
	if err != nil {
		return quota.Status{}, fmt.Errorf("get LINE quota: %w", err)
	}
	status.ProviderConsumed = consumed
	status.State = quotaState(status, now)
	return status, nil
}

func quotaMonth(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func quotaState(status quota.Status, now time.Time) quota.State {
	if status.SyncedAt == nil || status.ProviderConsumed == nil {
		return quota.StateUnsynced
	}
	if now.Sub(*status.SyncedAt) > quotaFreshness {
		return quota.StateStale
	}
	if status.ProviderLimit == nil {
		return quota.StateUnlimited
	}
	return quota.StateReady
}
