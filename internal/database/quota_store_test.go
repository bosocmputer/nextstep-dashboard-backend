package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/quota"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestQuotaStorePersistsSharedProviderUsageAndDetectsStaleness(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := NewQuotaStore(pool)
	now := time.Date(2026, 8, 10, 8, 0, 0, 0, time.UTC)
	limit := 5000

	status, err := store.Sync(ctx, line.QuotaUsage{Limit: &limit, Consumed: 4200}, now)
	if err != nil || status.State != quota.StateReady || status.ProviderConsumed == nil || *status.ProviderConsumed != 4200 || status.ProviderLimit == nil || *status.ProviderLimit != 5000 {
		t.Fatalf("Sync() = %+v, %v", status, err)
	}
	status, err = store.Get(ctx, now.Add(16*time.Minute))
	if err != nil || status.State != quota.StateStale {
		t.Fatalf("Get() stale = %+v, %v", status, err)
	}
}
