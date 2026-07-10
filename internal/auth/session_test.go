package auth

import (
	"bytes"
	"testing"
	"time"
)

func TestSessionManagerIssuesOpaqueIndependentTokens(t *testing.T) {
	manager, err := NewSessionManager(
		[]byte("01234567890123456789012345678901"),
		bytes.NewReader(append(bytes.Repeat([]byte{0x7a}, 32), bytes.Repeat([]byte{0x7b}, 32)...)),
		func() time.Time { return time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC) },
	)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}

	session, err := manager.Issue(12 * time.Hour)
	if err != nil {
		t.Fatalf("Issue() error = %v", err)
	}
	if session.RawToken == "" || session.CSRFToken == "" {
		t.Fatal("session tokens must not be empty")
	}
	if session.RawToken == session.CSRFToken {
		t.Fatal("session and CSRF tokens must be independent")
	}
	if len(session.TokenHash) != 32 || len(session.CSRFHash) != 32 {
		t.Fatalf("unexpected hash lengths: token=%d csrf=%d", len(session.TokenHash), len(session.CSRFHash))
	}
	if !manager.ValidToken(session.RawToken, session.TokenHash) {
		t.Fatal("issued token does not verify")
	}
	if manager.ValidToken("attacker-token", session.TokenHash) {
		t.Fatal("different token verified")
	}
	if got, want := session.ExpiresAt, time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", got, want)
	}
}

func TestSessionManagerRejectsWeakKeyAndInvalidTTL(t *testing.T) {
	if _, err := NewSessionManager([]byte("short"), bytes.NewReader(nil), time.Now); err == nil {
		t.Fatal("expected weak HMAC key to fail")
	}
	manager, err := NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(nil), time.Now)
	if err != nil {
		t.Fatalf("NewSessionManager() error = %v", err)
	}
	if _, err := manager.Issue(0); err == nil {
		t.Fatal("expected zero TTL to fail")
	}
}
