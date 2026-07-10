package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdminStoreLifecycle(t *testing.T) {
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

	created, err := BootstrapAdmin(ctx, pool, "superadmin", "$argon2id$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA")
	if err != nil || !created {
		t.Fatalf("BootstrapAdmin() first = created %v, error %v", created, err)
	}
	created, err = BootstrapAdmin(ctx, pool, "superadmin", "must-not-overwrite")
	if err != nil || created {
		t.Fatalf("BootstrapAdmin() second = created %v, error %v", created, err)
	}

	store := NewAdminStore(pool)
	user, err := store.FindAdminUser(ctx, "superadmin")
	if err != nil {
		t.Fatalf("FindAdminUser() error = %v", err)
	}
	if user.PasswordHash == "must-not-overwrite" || !user.MustRotatePassword {
		t.Fatalf("bootstrap user was overwritten: %+v", user)
	}

	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	identityHash := []byte("identity-hash")
	for attempt := 1; attempt <= 5; attempt++ {
		lockedUntil, err := store.RecordLoginFailure(ctx, identityHash, now, 15*time.Minute, 5, 15*time.Minute)
		if err != nil {
			t.Fatalf("RecordLoginFailure() attempt %d error = %v", attempt, err)
		}
		if attempt < 5 && lockedUntil != nil {
			t.Fatalf("attempt %d locked too early at %s", attempt, lockedUntil)
		}
		if attempt == 5 && (lockedUntil == nil || !lockedUntil.After(now)) {
			t.Fatalf("fifth attempt did not lock: %v", lockedUntil)
		}
	}

	session := auth.AdminSessionRecord{
		TokenHash: []byte("token-hash"),
		Username:  "superadmin",
		CSRFHash:  []byte("csrf-hash"),
		ExpiresAt: now.Add(time.Hour),
	}
	if err := store.CreateAdminSession(ctx, session); err != nil {
		t.Fatalf("CreateAdminSession() error = %v", err)
	}
	loaded, err := store.FindAdminSession(ctx, session.TokenHash, now)
	if err != nil || loaded.Username != session.Username {
		t.Fatalf("FindAdminSession() = %+v, %v", loaded, err)
	}
	if err := store.RotateAdminPassword(ctx, "superadmin", session.TokenHash, "new-encoded-hash", now); err != nil {
		t.Fatalf("RotateAdminPassword() error = %v", err)
	}
	user, err = store.FindAdminUser(ctx, "superadmin")
	if err != nil || user.PasswordHash != "new-encoded-hash" || user.MustRotatePassword {
		t.Fatalf("rotated user = %+v, %v", user, err)
	}
}
