package database

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestScheduleStoreLifecycleAndReadinessGates(t *testing.T) {
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
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tenantID, activeRecipientID, pendingRecipientID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at)
		values ($1, $2, 'Schedule Store', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, "schedule-"+tenantID.String(), now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_sml_connections (
		  tenant_id, endpoint_url, database_name, username_ciphertext, username_nonce,
		  password_ciphertext, password_nonce, encryption_key_id, readiness_status, last_tested_at
		) values ($1, 'http://10.0.0.8/service', 'shop', decode('01','hex'), decode('02','hex'), decode('03','hex'), decode('04','hex'), 'key', 'READY', $2)`, tenantID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into line_recipients (id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, display_name_ciphertext, display_name_nonce, encryption_key_id, status, verified_at)
		values ($1, decode('31','hex'), decode('32','hex'), decode('33','hex'), decode('34','hex'), decode('35','hex'), 'key', 'ACTIVE', $3),
		       ($2, null, null, null, decode('36','hex'), decode('37','hex'), 'key', 'PENDING', null)`, activeRecipientID, pendingRecipientID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_memberships (tenant_id, recipient_id, status)
		values ($1, $2, 'ACTIVE'), ($1, $3, 'PENDING')`, tenantID, activeRecipientID, pendingRecipientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into recipient_report_permissions (tenant_id, recipient_id, report_key)
		values ($1, $2, 'sales_goods_services'),
		       ($1, $2, 'stock_balance'),
		       ($1, $3, 'sales_goods_services'),
		       ($1, $3, 'stock_balance')`, tenantID, activeRecipientID, pendingRecipientID); err != nil {
		t.Fatal(err)
	}

	store := NewScheduleStore(pool)
	input := schedule.Input{
		Name: "Morning", DaysOfWeek: []int{1, 3, 5}, LocalTime: "09:30", Timezone: "Asia/Bangkok",
		PeriodPreset: report.Yesterday, ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance},
		RecipientIDs: []uuid.UUID{activeRecipientID, pendingRecipientID},
	}
	created, err := store.Create(ctx, []byte("admin"), "request-create", "schedule-create-001", tenantID, input, now)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	replayed, err := store.Create(ctx, []byte("admin"), "request-create-retry", "schedule-create-001", tenantID, input, now)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("Create() replay = %+v, %v", replayed, err)
	}
	changed := input
	changed.Name = "Changed"
	if _, err := store.Create(ctx, []byte("admin"), "request-conflict", "schedule-create-001", tenantID, changed, now); !errors.Is(err, schedule.ErrConflict) {
		t.Fatalf("changed idempotency input error = %v", err)
	}
	// Simulate permission drift after a valid schedule was created. Readiness must
	// still catch it, even though create/update reject incomplete permission sets.
	if _, err := pool.Exec(ctx, `
		delete from recipient_report_permissions
		where tenant_id = $1 and recipient_id = $2 and report_key = 'stock_balance'`, tenantID, activeRecipientID); err != nil {
		t.Fatal(err)
	}
	readiness, err := store.Readiness(ctx, tenantID, []uuid.UUID{created.ID}, now)
	if err != nil {
		t.Fatal(err)
	}
	wantBlockers := []string{schedule.BlockerRecipientNotActive, schedule.BlockerRecipientPermissionMismatch}
	if !reflect.DeepEqual(readiness[created.ID], wantBlockers) {
		t.Fatalf("Readiness() = %v, want %v", readiness[created.ID], wantBlockers)
	}
	if _, err := store.Activate(ctx, []byte("admin"), "request-blocked", tenantID, created.ID, now.Add(time.Hour), now); err == nil {
		t.Fatal("blocked schedule activated")
	}

	updatedInput := input
	updatedInput.RecipientIDs = []uuid.UUID{activeRecipientID}
	updatedInput.ReportKeys = []report.Key{report.SalesGoodsServices}
	updated, err := store.Update(ctx, []byte("admin"), "request-update", tenantID, created.ID, updatedInput, created.Version, now.Add(time.Second))
	if err != nil || updated.Version != created.Version+1 {
		t.Fatalf("Update() = %+v, %v", updated, err)
	}
	readiness, err = store.Readiness(ctx, tenantID, []uuid.UUID{created.ID}, now)
	if err != nil || len(readiness[created.ID]) != 0 {
		t.Fatalf("Readiness() after update = %v, %v", readiness[created.ID], err)
	}
	activated, err := store.Activate(ctx, []byte("admin"), "request-activate", tenantID, created.ID, now.Add(time.Hour), now.Add(2*time.Second))
	if err != nil || activated.Status != schedule.StatusActive {
		t.Fatalf("Activate() = %+v, %v", activated, err)
	}
	if _, err := store.Update(ctx, []byte("admin"), "request-active-update", tenantID, created.ID, updatedInput, activated.Version, now.Add(3*time.Second)); !errors.Is(err, schedule.ErrStateConflict) {
		t.Fatalf("active Update() error = %v", err)
	}
	paused, err := store.Pause(ctx, []byte("admin"), "request-pause", tenantID, created.ID, now.Add(4*time.Second))
	if err != nil || paused.Status != schedule.StatusPaused {
		t.Fatalf("Pause() = %+v, %v", paused, err)
	}
	page, err := store.List(ctx, tenantID, 25, "", false)
	if err != nil || len(page.Data) != 1 || page.Data[0].ID != created.ID {
		t.Fatalf("List() = %+v, %v", page, err)
	}
	archived, err := store.Archive(ctx, []byte("admin"), "request-archive", tenantID, created.ID, paused.Version, now.Add(5*time.Second))
	if err != nil || archived.Status != schedule.StatusArchived || archived.ArchivedAt == nil {
		t.Fatalf("Archive() = %+v, %v", archived, err)
	}
	page, err = store.List(ctx, tenantID, 25, "", false)
	if err != nil || len(page.Data) != 0 {
		t.Fatalf("default List() includes archived schedule: %+v, %v", page, err)
	}
	page, err = store.List(ctx, tenantID, 25, "", true)
	if err != nil || len(page.Data) != 1 || page.Data[0].Status != schedule.StatusArchived {
		t.Fatalf("List(includeArchived) = %+v, %v", page, err)
	}
	if _, err := store.MaterializeTest(ctx, []byte("admin"), "request-archived-test", "archived-test-send-001", tenantID, created.ID, now.Add(6*time.Second)); !errors.Is(err, schedule.ErrStateConflict) {
		t.Fatalf("archived test send error = %v", err)
	}
	restored, err := store.Restore(ctx, []byte("admin"), "request-restore", tenantID, created.ID, archived.Version, now.Add(7*time.Second))
	if err != nil || restored.Status != schedule.StatusDraft || restored.ArchivedAt != nil {
		t.Fatalf("Restore() = %+v, %v", restored, err)
	}
	var auditActions []string
	rows, err := pool.Query(ctx, `select action from audit_logs where tenant_id = $1 and entity_id = $2 order by occurred_at`, tenantID, created.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		auditActions = append(auditActions, action)
	}
	if !containsString(auditActions, "SCHEDULE_ARCHIVED") || !containsString(auditActions, "SCHEDULE_RESTORED") {
		t.Fatalf("audit actions = %v", auditActions)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
