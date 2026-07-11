package database

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestTenantStoreIdempotencyPaginationAndOptimisticUpdate(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	service := tenant.NewService(NewTenantStore(pool), func() time.Time { return now })
	input := tenant.CreateInput{Slug: "shop-one", Name: "ร้านหนึ่ง", Timezone: "Asia/Bangkok", AccessEndsAt: now.AddDate(1, 0, 0)}
	created, err := service.Create(ctx, []byte("admin-hash"), "request-1", "idempotency-shop-one", input)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	replayed, err := service.Create(ctx, []byte("admin-hash"), "request-2", "idempotency-shop-one", input)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("idempotent replay = %+v, %v", replayed, err)
	}
	changedInput := input
	changedInput.Name = "ชื่ออื่น"
	if _, err := service.Create(ctx, []byte("admin-hash"), "request-3", "idempotency-shop-one", changedInput); !errors.Is(err, tenant.ErrIdempotencyConflict) {
		t.Fatalf("changed idempotent request error = %v", err)
	}

	now = now.Add(time.Second)
	_, err = service.Create(ctx, []byte("admin-hash"), "request-4", "idempotency-shop-two", tenant.CreateInput{
		Slug: "shop-two", Name: "ร้านสอง", Timezone: "Asia/Bangkok", AccessEndsAt: now.AddDate(1, 0, 0),
	})
	if err != nil {
		t.Fatalf("Create() second tenant error = %v", err)
	}
	pageOne, err := service.List(ctx, tenant.ListFilter{PageSize: 1})
	if err != nil || len(pageOne.Data) != 1 || !pageOne.HasMore || pageOne.NextCursor == "" {
		t.Fatalf("page one = %+v, %v", pageOne, err)
	}
	pageTwo, err := service.List(ctx, tenant.ListFilter{PageSize: 1, Cursor: pageOne.NextCursor})
	if err != nil || len(pageTwo.Data) != 1 || pageTwo.Data[0].ID == pageOne.Data[0].ID {
		t.Fatalf("page two = %+v, %v", pageTwo, err)
	}

	name := "ร้านหนึ่งแก้ไข"
	updated, err := service.Update(ctx, []byte("admin-hash"), "request-5", created.ID, tenant.PatchInput{Version: created.Version, Name: &name})
	if err != nil || updated.Version != created.Version+1 || updated.Name != name {
		t.Fatalf("Update() = %+v, %v", updated, err)
	}
	if _, err := service.Update(ctx, []byte("admin-hash"), "request-6", created.ID, tenant.PatchInput{Version: created.Version, Name: &name}); !errors.Is(err, tenant.ErrConflict) {
		t.Fatalf("stale update error = %v", err)
	}

	var auditCount int
	if err := pool.QueryRow(ctx, `select count(*) from audit_logs where tenant_id = $1`, created.ID).Scan(&auditCount); err != nil {
		t.Fatalf("count audit logs: %v", err)
	}
	if auditCount != 2 {
		t.Fatalf("audit count = %d, want 2 (create + update)", auditCount)
	}
}

func TestTenantStoreAutoSlugCollisionReplayAndConcurrentCreate(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New() error = %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	collisionEntropy := bytes.Repeat([]byte{0}, 8)
	collisionSlug, err := generateTenantSlug(bytes.NewReader(collisionEntropy))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tenant.NewService(NewTenantStore(pool), func() time.Time { return now }).Create(
		ctx, []byte("auto-slug-admin"), "seed-collision", "auto-slug-seed-001",
		tenant.CreateInput{Slug: collisionSlug, Name: "ร้านทดสอบ collision"},
	); err != nil {
		t.Fatalf("seed collision slug: %v", err)
	}

	store := NewTenantStore(pool)
	store.slugEntropy = bytes.NewReader(append(collisionEntropy, []byte{1, 2, 3, 4, 5, 6, 7, 8}...))
	service := tenant.NewService(store, func() time.Time { return now })
	created, err := service.Create(ctx, []byte("auto-slug-admin"), "auto-create", "auto-slug-create-001", tenant.CreateInput{Name: "ร้านชื่อไทย"})
	if err != nil {
		t.Fatalf("auto slug create: %v", err)
	}
	if created.Slug == collisionSlug || created.Timezone != "Asia/Bangkok" || created.Status != tenant.StatusDisabled {
		t.Fatalf("auto slug defaults = %+v", created)
	}
	replayed, err := service.Create(ctx, []byte("auto-slug-admin"), "auto-replay", "auto-slug-create-001", tenant.CreateInput{Name: "ร้านชื่อไทย"})
	if err != nil || replayed.ID != created.ID || replayed.Slug != created.Slug {
		t.Fatalf("auto slug replay = %+v, %v", replayed, err)
	}

	concurrentService := tenant.NewService(NewTenantStore(pool), func() time.Time { return now })
	results := make([]tenant.Tenant, 2)
	errorsFound := make([]error, 2)
	idempotencyKeys := []string{"auto-slug-concurrent-001", "auto-slug-concurrent-002"}
	var wait sync.WaitGroup
	for index := range results {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			results[index], errorsFound[index] = concurrentService.Create(
				ctx, []byte("auto-slug-admin"), "concurrent-create", idempotencyKeys[index],
				tenant.CreateInput{Name: "ร้านชื่อซ้ำ"},
			)
		}(index)
	}
	wait.Wait()
	if errorsFound[0] != nil || errorsFound[1] != nil {
		t.Fatalf("concurrent create errors = %v", errorsFound)
	}
	if results[0].ID == results[1].ID || results[0].Slug == results[1].Slug {
		t.Fatalf("concurrent tenants are not unique: %+v", results)
	}
}
