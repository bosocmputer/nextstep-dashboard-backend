package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
)

type fakeAdminAuth struct {
	loginResult auth.LoginResult
	loginErr    error
	admin       auth.AuthenticatedAdmin
	authErr     error
	csrfErr     error
	logoutCount int
}

func (fake *fakeAdminAuth) Login(context.Context, string, string, string) (auth.LoginResult, error) {
	return fake.loginResult, fake.loginErr
}

func (fake *fakeAdminAuth) Authenticate(context.Context, string) (auth.AuthenticatedAdmin, error) {
	return fake.admin, fake.authErr
}

func (fake *fakeAdminAuth) ValidateCSRF(auth.AuthenticatedAdmin, string) error {
	return fake.csrfErr
}

func (fake *fakeAdminAuth) Logout(context.Context, auth.AuthenticatedAdmin) error {
	fake.logoutCount++
	return nil
}

func (fake *fakeAdminAuth) RotatePassword(context.Context, auth.AuthenticatedAdmin, string, string) error {
	return nil
}

func TestAdminLoginSetsHardenedCookieAndReturnsCSRFToken(t *testing.T) {
	expiresAt := time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	adminAuth := &fakeAdminAuth{loginResult: auth.LoginResult{
		RawToken:           "opaque-session-token",
		CSRFToken:          "opaque-csrf-token",
		Username:           "superadmin",
		ExpiresAt:          expiresAt,
		MustRotatePassword: true,
	}}
	handler := NewHandler(Dependencies{
		Readiness:     readinessFunc(func(context.Context) error { return nil }),
		AdminAuth:     adminAuth,
		SecureCookies: true,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/admin/login", strings.NewReader(`{"username":"superadmin","password":"a-secure-password"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("cookie count = %d", len(cookies))
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range cookies {
		switch cookie.Name {
		case adminSessionCookie:
			sessionCookie = cookie
		case adminCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie is not hardened: %+v", sessionCookie)
	}
	if csrfCookie == nil || csrfCookie.HttpOnly || !csrfCookie.Secure || csrfCookie.Value != "opaque-csrf-token" {
		t.Fatalf("CSRF cookie is not usable and hardened: %+v", csrfCookie)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"csrfToken":"opaque-csrf-token"`) || strings.Contains(body, "a-secure-password") {
		t.Fatalf("unexpected response body: %s", body)
	}
	for header, expected := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := response.Header().Get(header); got != expected {
			t.Errorf("%s = %q, want %q", header, got, expected)
		}
	}
}

func TestAdminLoginRejectsUnknownJSONField(t *testing.T) {
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }),
		AdminAuth: &fakeAdminAuth{},
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/admin/login", strings.NewReader(`{"username":"superadmin","password":"a-secure-password","role":"owner"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestAdminLoginMapsInvalidCredentialsAndLockout(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{name: "invalid", err: auth.ErrInvalidCredentials, wantStatus: http.StatusUnauthorized},
		{name: "locked", err: auth.ErrLoginLocked, wantStatus: http.StatusTooManyRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: &fakeAdminAuth{loginErr: test.err}})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/admin/login", strings.NewReader(`{"username":"superadmin","password":"a-secure-password"}`))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), test.err.Error()) {
				t.Fatalf("internal auth error leaked: %s", response.Body.String())
			}
		})
	}
}

func TestAdminLogoutRequiresSessionAndCSRF(t *testing.T) {
	adminAuth := &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin"}, csrfErr: auth.ErrInvalidCSRF}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: adminAuth})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/auth/admin/logout", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if adminAuth.logoutCount != 0 {
		t.Fatal("logout executed without valid CSRF")
	}
}

func TestAdminSessionRejectsInvalidCookie(t *testing.T) {
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }),
		AdminAuth: &fakeAdminAuth{authErr: errors.New("database details")},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/auth/admin/session", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "invalid"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "database details") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
