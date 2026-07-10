package database

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestSMLConnectionStoreUsesOptimisticVersionAndAuditsSafeMetadata(t *testing.T) {
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
	tenantID := uuid.New()
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, access_ends_at) values ($1, 'sml-store', 'SML Store', 'Asia/Bangkok', $2)`, tenantID, now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	store := NewSMLConnectionStore(pool)
	connection := sml.StoredConnection{
		TenantID:       tenantID,
		EndpointURL:    "http://10.0.0.8/service",
		ConfigFileName: "SMLConfigDATA.xml",
		DatabaseName:   "demo",
		Username:       secret.Sealed{KeyID: "key-1", Nonce: []byte("username1234"), Ciphertext: []byte("encrypted-user")},
		Password:       secret.Sealed{KeyID: "key-1", Nonce: []byte("password1234"), Ciphertext: []byte("encrypted-password")},
	}
	created, err := store.Put(ctx, []byte("actor"), "request-1", connection, 0, now)
	if err != nil || created.Version != 1 || created.Readiness != sml.ReadinessUntested {
		t.Fatalf("Put() create = %+v, %v", created, err)
	}
	if _, err := store.Put(ctx, []byte("actor"), "request-2", connection, 0, now); !errors.Is(err, sml.ErrConnectionVersionConflict) {
		t.Fatalf("stale Put() error = %v", err)
	}
	connection.EndpointURL = "http://10.0.0.9/service"
	updated, err := store.Put(ctx, []byte("actor"), "request-3", connection, 1, now.Add(time.Second))
	if err != nil || updated.Version != 2 {
		t.Fatalf("Put() update = %+v, %v", updated, err)
	}
	if err := store.MarkTested(ctx, []byte("actor"), "request-4", tenantID, sml.ReadinessReady, "", now.Add(2*time.Second)); err != nil {
		t.Fatalf("MarkTested() error = %v", err)
	}
	loaded, err := store.Get(ctx, tenantID)
	if err != nil || loaded.Readiness != sml.ReadinessReady || loaded.EndpointURL != connection.EndpointURL {
		t.Fatalf("Get() = %+v, %v", loaded, err)
	}
	var auditPayload string
	if err := pool.QueryRow(ctx, `select coalesce(string_agg(after_json::text, ''), '') from audit_logs where tenant_id = $1`, tenantID).Scan(&auditPayload); err != nil {
		t.Fatal(err)
	}
	if containsAny(auditPayload, "encrypted-user", "encrypted-password") {
		t.Fatalf("audit payload contains encrypted secret material: %s", auditPayload)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
