package sentinel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTelegramClientUsesPlainTextAndSafeFailureClassification(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/sendMessage") {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	defer server.Close()
	client, err := NewTelegramClient(testTelegramToken(), "123456789", server.URL, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	remoteID, err := client.Send(context.Background(), Alert{Kind: "OPEN", Incident: Incident{AlertRef: "NST-ABC123DEF456", Severity: SeverityP1, Status: StatusOpen, RootCause: RootPlatform, IncidentType: "TEST", FirstSeenAt: time.Now(), OccurrenceCount: 1, AffectedCount: 1}}, "https://example.test/admin/operational-incidents")
	if err != nil || remoteID != "42" || requests != 1 {
		t.Fatalf("Send() = %q, %v; requests=%d", remoteID, err, requests)
	}
}

func TestTelegramClientTreats429And5xxAsRetryable(t *testing.T) {
	for status, permanent := range map[int]bool{http.StatusTooManyRequests: false, http.StatusBadGateway: false, http.StatusBadRequest: true} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(status) }))
		client, err := NewTelegramClient(testTelegramToken(), "123456789", server.URL, &http.Client{Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.Send(context.Background(), Alert{Kind: "OPEN", Incident: Incident{AlertRef: "NST-ABC123DEF456", Severity: SeverityP1, Status: StatusOpen, RootCause: RootPlatform, IncidentType: "TEST", FirstSeenAt: time.Now(), OccurrenceCount: 1, AffectedCount: 1}}, "https://example.test/admin/operational-incidents")
		server.Close()
		if err == nil || IsPermanentSendError(err) != permanent {
			t.Fatalf("status %d error=%v permanent=%v", status, err, IsPermanentSendError(err))
		}
	}
}

func TestTelegramClientRetriesTransientFailureWithinBound(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		attempts++
		response.Header().Set("Content-Type", "application/json")
		if attempts < 3 {
			response.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = response.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
	}))
	defer server.Close()
	client, err := NewTelegramClient(testTelegramToken(), "123456789", server.URL, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	remoteID, err := client.Send(context.Background(), Alert{Kind: "OPEN", Incident: Incident{AlertRef: "NST-ABC123DEF456", Severity: SeverityP1, Status: StatusOpen, RootCause: RootPlatform, IncidentType: "TEST", FirstSeenAt: time.Now(), OccurrenceCount: 1, AffectedCount: 1}}, "https://example.test/admin/operational-incidents")
	if err != nil || remoteID != "99" || attempts != 3 {
		t.Fatalf("id=%s err=%v attempts=%d", remoteID, err, attempts)
	}
}

func TestTelegramConfigurationRejectsExposedOrMalformedValues(t *testing.T) {
	for _, token := range []string{"", "short", "123456:contains space"} {
		if _, err := NewTelegramClient(token, "123", "https://api.telegram.org", nil); err == nil {
			t.Fatalf("token %q was accepted", token)
		}
	}
	if _, err := NewTelegramClient(testTelegramToken(), "not-a-chat", "https://api.telegram.org", nil); err == nil {
		t.Fatal("invalid chat id was accepted")
	}
}

func TestTelegramPreflightMessageIsFixedAndContainsNoIncidentData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/sendMessage") {
			t.Fatalf("path = %s", request.URL.Path)
		}
		body, _ := io.ReadAll(request.Body)
		text := string(body)
		for _, forbidden := range []string{"tenant", "alert_ref", "KPI", "SQL"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("preflight body contains %q: %s", forbidden, text)
			}
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client, err := NewTelegramClient(testTelegramToken(), "123456789", server.URL, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendPreflightMessage(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramTenantContextRequiresVerifiedPrivateChat(t *testing.T) {
	for chatType, expected := range map[string]TelegramTenantContextStatus{
		"private": TelegramTenantContextPrivateVerified,
		"group":   TelegramTenantContextRedactedChatNotPrivate,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			switch {
			case strings.HasSuffix(request.URL.Path, "/getMe"):
				_, _ = response.Write([]byte(`{"ok":true}`))
			case strings.HasSuffix(request.URL.Path, "/getChat"):
				_, _ = response.Write([]byte(`{"ok":true,"result":{"id":123456789,"type":"` + chatType + `"}}`))
			default:
				t.Fatalf("unexpected Telegram path %s", request.URL.Path)
			}
		}))
		client, err := NewTelegramClient(testTelegramToken(), "123456789", server.URL, &http.Client{Timeout: time.Second})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		client.ConfigureTenantContext(TelegramTenantContextPrivateChat)
		err = client.Preflight(context.Background())
		status := client.TenantContextStatus()
		server.Close()
		if status != expected {
			t.Fatalf("chat type %s: status=%q err=%v", chatType, status, err)
		}
		if chatType == "private" && err != nil {
			t.Fatalf("private chat preflight failed: %v", err)
		}
		if chatType != "private" && err == nil {
			t.Fatal("non-private chat enabled tenant context")
		}
	}
}

func TestTelegramTenantContextRedactsWhenPreflightCannotVerifyChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := NewTelegramClient(testTelegramToken(), "123456789", server.URL, &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	client.ConfigureTenantContext(TelegramTenantContextPrivateChat)
	if err := client.Preflight(context.Background()); err == nil {
		t.Fatal("unavailable Telegram API passed preflight")
	}
	if client.TenantContextStatus() != TelegramTenantContextRedactedVerificationFailed || client.TenantContextAllowed() {
		t.Fatalf("status=%q allowed=%v", client.TenantContextStatus(), client.TenantContextAllowed())
	}
}

func testTelegramToken() string { return "123456:" + strings.Repeat("x", 32) }
