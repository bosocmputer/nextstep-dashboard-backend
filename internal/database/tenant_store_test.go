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
	"github.com/google/uuid"
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
	pendingRecipientID := uuid.New()
	expiresAt := now.Add(time.Hour)
	seedStatements := []struct {
		query string
		args  []any
	}{
		{`insert into line_recipients (id, encryption_key_id, status, created_at, updated_at) values ($1, 'test-key', 'PENDING', $2, $2)`, []any{pendingRecipientID, now}},
		{`insert into tenant_memberships (tenant_id, recipient_id, status, created_at, updated_at) values ($1, $2, 'PENDING', $3, $3)`, []any{created.ID, pendingRecipientID, now}},
		{`insert into recipient_invitations (tenant_id, pending_recipient_id, reference_hash, created_at, expires_at) values ($1, $2, decode('01020304', 'hex'), $3, $4)`, []any{created.ID, pendingRecipientID, now, expiresAt}},
		{`insert into notification_schedules (tenant_id, name, status, local_time, timezone, period_preset, next_run_at, created_at, updated_at) values ($1, 'active-before-archive', 'ACTIVE', '08:00', 'Asia/Bangkok', 'YESTERDAY', $2, $3, $3)`, []any{created.ID, expiresAt, now}},
		{`insert into report_runs (tenant_id, report_key, source, idempotency_key, status, period_preset, queued_at, expires_at, created_at, updated_at) values ($1, 'sales_goods_services', 'DASHBOARD', 'tenant-archive-run', 'QUEUED', 'YESTERDAY', $2, $3, $2, $2)`, []any{created.ID, now, expiresAt}},
		{`insert into dashboard_refreshes (tenant_id, requested_by_recipient_id, idempotency_key, status, total, created_at, updated_at) values ($1, $2, 'tenant-archive-refresh', 'QUEUED', 1, $3, $3)`, []any{created.ID, pendingRecipientID, now}},
	}
	for _, statement := range seedStatements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed tenant archive dependency: %v", err)
		}
	}

	if err := service.Archive(ctx, []byte("admin-hash"), "request-7", created.ID, updated.Version); err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	if _, err := service.Get(ctx, created.ID); !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("archived tenant Get() error = %v", err)
	}
	page, err := service.List(ctx, tenant.ListFilter{PageSize: 25, Search: name})
	if err != nil || len(page.Data) != 0 {
		t.Fatalf("archived tenant still listed: page=%+v err=%v", page, err)
	}
	if err := service.Archive(ctx, []byte("admin-hash"), "request-8", created.ID, updated.Version); !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("archive replay error = %v", err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from audit_logs where tenant_id = $1`, created.ID).Scan(&auditCount); err != nil {
		t.Fatalf("count archived audit logs: %v", err)
	}
	if auditCount != 3 {
		t.Fatalf("audit count = %d, want 3 (create + update + archive)", auditCount)
	}
	var scheduleStatus, membershipStatus, recipientStatus, runStatus, runSafeCode, refreshStatus string
	var invitationUsedAt *time.Time
	if err := pool.QueryRow(ctx, `select status from notification_schedules where tenant_id = $1 and name = 'active-before-archive'`, created.ID).Scan(&scheduleStatus); err != nil {
		t.Fatalf("read archived schedule status: %v", err)
	}
	if err := pool.QueryRow(ctx, `select status from tenant_memberships where tenant_id = $1 and recipient_id = $2`, created.ID, pendingRecipientID).Scan(&membershipStatus); err != nil {
		t.Fatalf("read archived membership status: %v", err)
	}
	if err := pool.QueryRow(ctx, `select status from line_recipients where id = $1`, pendingRecipientID).Scan(&recipientStatus); err != nil {
		t.Fatalf("read archived pending recipient status: %v", err)
	}
	if err := pool.QueryRow(ctx, `select used_at from recipient_invitations where tenant_id = $1`, created.ID).Scan(&invitationUsedAt); err != nil {
		t.Fatalf("read archived invitation status: %v", err)
	}
	if err := pool.QueryRow(ctx, `select status, safe_error_code from report_runs where tenant_id = $1 and idempotency_key = 'tenant-archive-run'`, created.ID).Scan(&runStatus, &runSafeCode); err != nil {
		t.Fatalf("read archived report status: %v", err)
	}
	if err := pool.QueryRow(ctx, `select status from dashboard_refreshes where tenant_id = $1 and idempotency_key = 'tenant-archive-refresh'`, created.ID).Scan(&refreshStatus); err != nil {
		t.Fatalf("read archived refresh status: %v", err)
	}
	if scheduleStatus != "PAUSED" || membershipStatus != "REVOKED" || recipientStatus != "REVOKED" || invitationUsedAt == nil || runStatus != "CANCELLED" || runSafeCode != "TENANT_ARCHIVED" || refreshStatus != "FAILED" {
		t.Fatalf("archive dependencies not stopped: schedule=%s membership=%s recipient=%s invitation=%v run=%s/%s refresh=%s", scheduleStatus, membershipStatus, recipientStatus, invitationUsedAt, runStatus, runSafeCode, refreshStatus)
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
