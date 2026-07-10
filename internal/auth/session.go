package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"time"
)

const sessionTokenBytes = 32

type SessionManager struct {
	hmacKey []byte
	entropy io.Reader
	now     func() time.Time
}

type IssuedSession struct {
	RawToken  string
	TokenHash []byte
	CSRFToken string
	CSRFHash  []byte
	ExpiresAt time.Time
}

func NewSessionManager(hmacKey []byte, entropy io.Reader, now func() time.Time) (*SessionManager, error) {
	if len(hmacKey) < 32 {
		return nil, errors.New("session HMAC key must contain at least 32 bytes")
	}
	if entropy == nil || now == nil {
		return nil, errors.New("session manager dependencies are required")
	}
	return &SessionManager{hmacKey: append([]byte(nil), hmacKey...), entropy: entropy, now: now}, nil
}

func (manager *SessionManager) Issue(ttl time.Duration) (IssuedSession, error) {
	if ttl <= 0 || ttl > 7*24*time.Hour {
		return IssuedSession{}, errors.New("session TTL must be between zero and seven days")
	}
	token, err := manager.randomToken()
	if err != nil {
		return IssuedSession{}, err
	}
	csrf, err := manager.randomToken()
	if err != nil {
		return IssuedSession{}, err
	}
	return IssuedSession{
		RawToken:  token,
		TokenHash: manager.HashToken(token),
		CSRFToken: csrf,
		CSRFHash:  manager.HashToken(csrf),
		ExpiresAt: manager.now().UTC().Add(ttl),
	}, nil
}

func (manager *SessionManager) HashToken(token string) []byte {
	mac := hmac.New(sha256.New, manager.hmacKey)
	_, _ = mac.Write([]byte(token))
	return mac.Sum(nil)
}

func (manager *SessionManager) ValidToken(token string, expectedHash []byte) bool {
	return hmac.Equal(manager.HashToken(token), expectedHash)
}

func (manager *SessionManager) randomToken() (string, error) {
	random := make([]byte, sessionTokenBytes)
	if _, err := io.ReadFull(manager.entropy, random); err != nil {
		return "", errors.New("generate secure session token")
	}
	return base64.RawURLEncoding.EncodeToString(random), nil
}
