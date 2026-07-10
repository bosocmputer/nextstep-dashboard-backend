package auth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

type memoryAdminStore struct {
	user         AdminUser
	lockedUntil  *time.Time
	failures     int
	sessions     map[string]AdminSessionRecord
	createdCount int
}

func (store *memoryAdminStore) FindAdminUser(_ context.Context, username string) (AdminUser, error) {
	if username != store.user.Username {
		return AdminUser{}, ErrAdminUserNotFound
	}
	return store.user, nil
}

func (store *memoryAdminStore) LoginLockedUntil(_ context.Context, _ []byte, _ time.Time) (*time.Time, error) {
	return store.lockedUntil, nil
}

func (store *memoryAdminStore) RecordLoginFailure(_ context.Context, _ []byte, now time.Time, _ time.Duration, maximum int, lockout time.Duration) (*time.Time, error) {
	store.failures++
	if store.failures >= maximum {
		until := now.Add(lockout)
		store.lockedUntil = &until
	}
	return store.lockedUntil, nil
}

func (store *memoryAdminStore) ClearLoginFailures(_ context.Context, _ []byte) error {
	store.failures = 0
	store.lockedUntil = nil
	return nil
}

func (store *memoryAdminStore) CreateAdminSession(_ context.Context, session AdminSessionRecord) error {
	if store.sessions == nil {
		store.sessions = make(map[string]AdminSessionRecord)
	}
	store.sessions[string(session.TokenHash)] = session
	store.createdCount++
	return nil
}

func (store *memoryAdminStore) FindAdminSession(_ context.Context, tokenHash []byte, now time.Time) (AdminSessionRecord, error) {
	session, ok := store.sessions[string(tokenHash)]
	if !ok || session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return AdminSessionRecord{}, ErrAdminSessionNotFound
	}
	return session, nil
}

func (store *memoryAdminStore) RevokeAdminSession(_ context.Context, tokenHash []byte, now time.Time) error {
	session, ok := store.sessions[string(tokenHash)]
	if !ok {
		return nil
	}
	session.RevokedAt = &now
	store.sessions[string(tokenHash)] = session
	return nil
}

func (store *memoryAdminStore) RotateAdminPassword(_ context.Context, username string, currentSessionHash []byte, passwordHash string, now time.Time) error {
	if username != store.user.Username {
		return ErrAdminUserNotFound
	}
	store.user.PasswordHash = passwordHash
	store.user.MustRotatePassword = false
	for key, session := range store.sessions {
		if key == string(currentSessionHash) {
			continue
		}
		session.RevokedAt = &now
		store.sessions[key] = session
	}
	return nil
}

func TestAdminServiceLoginCreatesOpaqueSession(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	passwordHash := encodedHash("correct horse battery staple", 64*1024, 3, 2)
	store := &memoryAdminStore{user: AdminUser{Username: "superadmin", PasswordHash: passwordHash, MustRotatePassword: true}}
	manager, _ := NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	service := NewAdminService(store, manager, passwordHash, bytes.NewReader(bytes.Repeat([]byte{3}, 32)), func() time.Time { return now })

	result, err := service.Login(context.Background(), "superadmin", "correct horse battery staple", "198.51.100.1")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if result.Username != "superadmin" || !result.MustRotatePassword {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.RawToken == "" || result.CSRFToken == "" || store.createdCount != 1 {
		t.Fatalf("session was not created: %+v count=%d", result, store.createdCount)
	}

	authenticated, err := service.Authenticate(context.Background(), result.RawToken)
	if err != nil || authenticated.Username != "superadmin" {
		t.Fatalf("Authenticate() = %+v, %v", authenticated, err)
	}
	if err := service.ValidateCSRF(authenticated, result.CSRFToken); err != nil {
		t.Fatalf("ValidateCSRF() error = %v", err)
	}
	if err := service.ValidateCSRF(authenticated, "attacker-token"); !errors.Is(err, ErrInvalidCSRF) {
		t.Fatalf("invalid CSRF error = %v", err)
	}
}

func TestAdminServiceLocksRepeatedInvalidLogin(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	passwordHash := encodedHash("correct horse battery staple", 64*1024, 3, 2)
	store := &memoryAdminStore{user: AdminUser{Username: "superadmin", PasswordHash: passwordHash}}
	manager, _ := NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	service := NewAdminService(store, manager, passwordHash, bytes.NewReader(bytes.Repeat([]byte{3}, 32)), func() time.Time { return now })

	for attempt := 1; attempt <= 4; attempt++ {
		_, err := service.Login(context.Background(), "superadmin", "wrong password value", "198.51.100.1")
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
	}
	_, err := service.Login(context.Background(), "superadmin", "wrong password value", "198.51.100.1")
	if !errors.Is(err, ErrLoginLocked) {
		t.Fatalf("fifth attempt error = %v", err)
	}
	_, err = service.Login(context.Background(), "superadmin", "correct horse battery staple", "198.51.100.1")
	if !errors.Is(err, ErrLoginLocked) {
		t.Fatalf("correct password during lockout error = %v", err)
	}
}

func TestAdminServiceUsesFallbackHashForUnknownUser(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	passwordHash := encodedHash("correct horse battery staple", 64*1024, 3, 2)
	store := &memoryAdminStore{user: AdminUser{Username: "superadmin", PasswordHash: passwordHash}}
	manager, _ := NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	service := NewAdminService(store, manager, passwordHash, bytes.NewReader(bytes.Repeat([]byte{3}, 32)), func() time.Time { return now })

	_, err := service.Login(context.Background(), "unknown", "correct horse battery staple", "198.51.100.1")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user error = %v", err)
	}
	if store.createdCount != 0 {
		t.Fatal("unknown user created a session")
	}
}
