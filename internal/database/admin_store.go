package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AdminStore struct {
	pool *pgxpool.Pool
}

func NewAdminStore(pool *pgxpool.Pool) *AdminStore {
	return &AdminStore{pool: pool}
}

func BootstrapAdmin(ctx context.Context, pool *pgxpool.Pool, username, passwordHash string) (bool, error) {
	result, err := pool.Exec(ctx, `
		insert into admin_users (username, password_hash, must_rotate_password)
		values ($1, $2, true)
		on conflict (username) do nothing`, username, passwordHash)
	if err != nil {
		return false, fmt.Errorf("bootstrap admin user: %w", err)
	}
	return result.RowsAffected() == 1, nil
}

func (store *AdminStore) FindAdminUser(ctx context.Context, username string) (auth.AdminUser, error) {
	var user auth.AdminUser
	err := store.pool.QueryRow(ctx, `
		select username, password_hash, must_rotate_password
		from admin_users
		where username = $1`, username).Scan(&user.Username, &user.PasswordHash, &user.MustRotatePassword)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.AdminUser{}, auth.ErrAdminUserNotFound
	}
	if err != nil {
		return auth.AdminUser{}, fmt.Errorf("find admin user: %w", err)
	}
	return user, nil
}

func (store *AdminStore) LoginLockedUntil(ctx context.Context, identityHash []byte, now time.Time) (*time.Time, error) {
	var lockedUntil *time.Time
	err := store.pool.QueryRow(ctx, `
		select locked_until
		from admin_login_attempts
		where identity_hash = $1`, identityHash).Scan(&lockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read admin login lock: %w", err)
	}
	if lockedUntil == nil || !lockedUntil.After(now) {
		return nil, nil
	}
	return lockedUntil, nil
}

func (store *AdminStore) RecordLoginFailure(ctx context.Context, identityHash []byte, now time.Time, window time.Duration, maximum int, lockout time.Duration) (*time.Time, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin login failure transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		insert into admin_login_attempts (identity_hash)
		values ($1)
		on conflict (identity_hash) do nothing`, identityHash); err != nil {
		return nil, fmt.Errorf("ensure login failure row: %w", err)
	}
	var failedCount int
	var firstFailedAt, lockedUntil *time.Time
	if err := tx.QueryRow(ctx, `
		select failed_count, first_failed_at, locked_until
		from admin_login_attempts
		where identity_hash = $1
		for update`, identityHash).Scan(&failedCount, &firstFailedAt, &lockedUntil); err != nil {
		return nil, fmt.Errorf("lock login failure row: %w", err)
	}
	if lockedUntil != nil && lockedUntil.After(now) {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit existing login lock: %w", err)
		}
		return lockedUntil, nil
	}
	if firstFailedAt == nil || now.Sub(*firstFailedAt) > window {
		failedCount = 1
		firstFailedAt = &now
	} else {
		failedCount++
	}
	lockedUntil = nil
	if failedCount >= maximum {
		value := now.Add(lockout)
		lockedUntil = &value
	}
	if _, err := tx.Exec(ctx, `
		update admin_login_attempts
		set failed_count = $2, first_failed_at = $3, locked_until = $4, updated_at = $5
		where identity_hash = $1`, identityHash, failedCount, firstFailedAt, lockedUntil, now); err != nil {
		return nil, fmt.Errorf("update login failure row: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit login failure: %w", err)
	}
	return lockedUntil, nil
}

func (store *AdminStore) ClearLoginFailures(ctx context.Context, identityHash []byte) error {
	if _, err := store.pool.Exec(ctx, `delete from admin_login_attempts where identity_hash = $1`, identityHash); err != nil {
		return fmt.Errorf("clear admin login failures: %w", err)
	}
	return nil
}

func (store *AdminStore) CreateAdminSession(ctx context.Context, session auth.AdminSessionRecord) error {
	_, err := store.pool.Exec(ctx, `
		insert into admin_sessions (id_hash, username, csrf_hash, expires_at)
		values ($1, $2, $3, $4)`, session.TokenHash, session.Username, session.CSRFHash, session.ExpiresAt)
	if err != nil {
		return fmt.Errorf("create admin session: %w", err)
	}
	return nil
}

func (store *AdminStore) FindAdminSession(ctx context.Context, tokenHash []byte, now time.Time) (auth.AdminSessionRecord, error) {
	var session auth.AdminSessionRecord
	err := store.pool.QueryRow(ctx, `
		select s.id_hash, s.username, s.csrf_hash, s.expires_at, u.must_rotate_password, s.revoked_at
		from admin_sessions s
		join admin_users u on u.username = s.username
		where s.id_hash = $1 and s.revoked_at is null and s.expires_at > $2`, tokenHash, now).Scan(
		&session.TokenHash,
		&session.Username,
		&session.CSRFHash,
		&session.ExpiresAt,
		&session.MustRotatePassword,
		&session.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.AdminSessionRecord{}, auth.ErrAdminSessionNotFound
	}
	if err != nil {
		return auth.AdminSessionRecord{}, fmt.Errorf("find admin session: %w", err)
	}
	return session, nil
}

func (store *AdminStore) RevokeAdminSession(ctx context.Context, tokenHash []byte, now time.Time) error {
	if _, err := store.pool.Exec(ctx, `
		update admin_sessions
		set revoked_at = coalesce(revoked_at, $2)
		where id_hash = $1`, tokenHash, now); err != nil {
		return fmt.Errorf("revoke admin session: %w", err)
	}
	return nil
}

func (store *AdminStore) RotateAdminPassword(ctx context.Context, username string, currentSessionHash []byte, passwordHash string, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin admin password rotation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	result, err := tx.Exec(ctx, `
		update admin_users
		set password_hash = $2, must_rotate_password = false, password_updated_at = $3, updated_at = $3
		where username = $1`, username, passwordHash, now)
	if err != nil {
		return fmt.Errorf("rotate admin password: %w", err)
	}
	if result.RowsAffected() != 1 {
		return auth.ErrAdminUserNotFound
	}
	if _, err := tx.Exec(ctx, `
		update admin_sessions
		set revoked_at = $3
		where username = $1 and id_hash <> $2 and revoked_at is null`, username, currentSessionHash, now); err != nil {
		return fmt.Errorf("revoke other admin sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit admin password rotation: %w", err)
	}
	return nil
}
