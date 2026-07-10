package auth

import (
	"context"
	"crypto/hmac"
	"errors"
	"io"
	"strings"
	"time"
)

const (
	adminSessionTTL   = 12 * time.Hour
	loginWindow       = 15 * time.Minute
	loginLockDuration = 15 * time.Minute
	maximumFailures   = 5
)

var (
	ErrAdminUserNotFound    = errors.New("admin user not found")
	ErrAdminSessionNotFound = errors.New("admin session not found")
	ErrInvalidCredentials   = errors.New("invalid credentials")
	ErrLoginLocked          = errors.New("login temporarily locked")
	ErrInvalidSession       = errors.New("invalid admin session")
	ErrInvalidCSRF          = errors.New("invalid CSRF token")
	ErrPasswordUnchanged    = errors.New("new password must differ from current password")
)

type AdminUser struct {
	Username           string
	PasswordHash       string
	MustRotatePassword bool
}

type AdminSessionRecord struct {
	TokenHash          []byte
	Username           string
	CSRFHash           []byte
	ExpiresAt          time.Time
	MustRotatePassword bool
	RevokedAt          *time.Time
}

type AdminStore interface {
	FindAdminUser(context.Context, string) (AdminUser, error)
	LoginLockedUntil(context.Context, []byte, time.Time) (*time.Time, error)
	RecordLoginFailure(context.Context, []byte, time.Time, time.Duration, int, time.Duration) (*time.Time, error)
	ClearLoginFailures(context.Context, []byte) error
	CreateAdminSession(context.Context, AdminSessionRecord) error
	FindAdminSession(context.Context, []byte, time.Time) (AdminSessionRecord, error)
	RevokeAdminSession(context.Context, []byte, time.Time) error
	RotateAdminPassword(context.Context, string, []byte, string, time.Time) error
}

type AdminService struct {
	store        AdminStore
	sessions     *SessionManager
	fallbackHash string
	entropy      io.Reader
	now          func() time.Time
}

type LoginResult struct {
	RawToken           string
	CSRFToken          string
	Username           string
	ExpiresAt          time.Time
	MustRotatePassword bool
}

type AuthenticatedAdmin struct {
	TokenHash          []byte
	Username           string
	CSRFHash           []byte
	ExpiresAt          time.Time
	MustRotatePassword bool
}

func NewAdminService(store AdminStore, sessions *SessionManager, fallbackHash string, entropy io.Reader, now func() time.Time) *AdminService {
	return &AdminService{store: store, sessions: sessions, fallbackHash: fallbackHash, entropy: entropy, now: now}
}

func (service *AdminService) Login(ctx context.Context, username, password, sourceIdentity string) (LoginResult, error) {
	now := service.now().UTC()
	identityHash := service.sessions.HashToken("admin-login:" + strings.ToLower(username) + ":" + sourceIdentity)
	lockedUntil, err := service.store.LoginLockedUntil(ctx, identityHash, now)
	if err != nil {
		return LoginResult{}, err
	}
	if lockedUntil != nil && lockedUntil.After(now) {
		return LoginResult{}, ErrLoginLocked
	}

	user, userErr := service.store.FindAdminUser(ctx, username)
	passwordHash := service.fallbackHash
	if userErr == nil {
		passwordHash = user.PasswordHash
	} else if !errors.Is(userErr, ErrAdminUserNotFound) {
		return LoginResult{}, userErr
	}
	valid, err := VerifyArgon2ID(passwordHash, password)
	if err != nil {
		return LoginResult{}, errors.New("configured admin password hash is invalid")
	}
	if userErr != nil || !valid {
		lockedUntil, recordErr := service.store.RecordLoginFailure(ctx, identityHash, now, loginWindow, maximumFailures, loginLockDuration)
		if recordErr != nil {
			return LoginResult{}, recordErr
		}
		if lockedUntil != nil && lockedUntil.After(now) {
			return LoginResult{}, ErrLoginLocked
		}
		return LoginResult{}, ErrInvalidCredentials
	}

	issued, err := service.sessions.Issue(adminSessionTTL)
	if err != nil {
		return LoginResult{}, err
	}
	if err := service.store.CreateAdminSession(ctx, AdminSessionRecord{
		TokenHash:          issued.TokenHash,
		Username:           user.Username,
		CSRFHash:           issued.CSRFHash,
		ExpiresAt:          issued.ExpiresAt,
		MustRotatePassword: user.MustRotatePassword,
	}); err != nil {
		return LoginResult{}, err
	}
	if err := service.store.ClearLoginFailures(ctx, identityHash); err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		RawToken:           issued.RawToken,
		CSRFToken:          issued.CSRFToken,
		Username:           user.Username,
		ExpiresAt:          issued.ExpiresAt,
		MustRotatePassword: user.MustRotatePassword,
	}, nil
}

func (service *AdminService) Authenticate(ctx context.Context, rawToken string) (AuthenticatedAdmin, error) {
	if rawToken == "" {
		return AuthenticatedAdmin{}, ErrInvalidSession
	}
	tokenHash := service.sessions.HashToken(rawToken)
	session, err := service.store.FindAdminSession(ctx, tokenHash, service.now().UTC())
	if errors.Is(err, ErrAdminSessionNotFound) {
		return AuthenticatedAdmin{}, ErrInvalidSession
	}
	if err != nil {
		return AuthenticatedAdmin{}, err
	}
	return AuthenticatedAdmin{
		TokenHash:          tokenHash,
		Username:           session.Username,
		CSRFHash:           session.CSRFHash,
		ExpiresAt:          session.ExpiresAt,
		MustRotatePassword: session.MustRotatePassword,
	}, nil
}

func (service *AdminService) ValidateCSRF(admin AuthenticatedAdmin, csrfToken string) error {
	if csrfToken == "" || !hmac.Equal(service.sessions.HashToken(csrfToken), admin.CSRFHash) {
		return ErrInvalidCSRF
	}
	return nil
}

func (service *AdminService) Logout(ctx context.Context, admin AuthenticatedAdmin) error {
	return service.store.RevokeAdminSession(ctx, admin.TokenHash, service.now().UTC())
}

func (service *AdminService) RotatePassword(ctx context.Context, admin AuthenticatedAdmin, currentPassword, newPassword string) error {
	user, err := service.store.FindAdminUser(ctx, admin.Username)
	if err != nil {
		return ErrInvalidCredentials
	}
	valid, err := VerifyArgon2ID(user.PasswordHash, currentPassword)
	if err != nil || !valid {
		return ErrInvalidCredentials
	}
	unchanged, err := VerifyArgon2ID(user.PasswordHash, newPassword)
	if err != nil {
		return err
	}
	if unchanged {
		return ErrPasswordUnchanged
	}
	newHash, err := HashArgon2ID(newPassword, service.entropy)
	if err != nil {
		return err
	}
	return service.store.RotateAdminPassword(ctx, admin.Username, admin.TokenHash, newHash, service.now().UTC())
}
