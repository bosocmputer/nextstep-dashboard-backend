package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
)

type fakeViewerAPI struct {
	exchangeResult viewer.ExchangeResult
	exchangeErr    error
	authenticated  viewer.AuthenticatedViewer
	authErr        error
	csrfErr        error
	tenants        []viewer.TenantAccess
	reports        []viewer.ReportAccess
	logoutCount    int
}

func (fake *fakeViewerAPI) Exchange(context.Context, string, string, string) (viewer.ExchangeResult, error) {
	return fake.exchangeResult, fake.exchangeErr
}

func (fake *fakeViewerAPI) Authenticate(context.Context, string) (viewer.AuthenticatedViewer, error) {
	return fake.authenticated, fake.authErr
}

func (fake *fakeViewerAPI) ValidateCSRF(viewer.AuthenticatedViewer, string) error {
	return fake.csrfErr
}

func (fake *fakeViewerAPI) Logout(context.Context, viewer.AuthenticatedViewer) error {
	fake.logoutCount++
	return nil
}

func (fake *fakeViewerAPI) ListTenants(context.Context, uuid.UUID) ([]viewer.TenantAccess, error) {
	return fake.tenants, nil
}

func (fake *fakeViewerAPI) ListReports(context.Context, uuid.UUID, uuid.UUID) ([]viewer.ReportAccess, error) {
	return fake.reports, nil
}

func (fake *fakeViewerAPI) CanAccessReport(context.Context, uuid.UUID, uuid.UUID, report.Key) (bool, error) {
	return true, nil
}

func TestViewerSessionExchangeSetsHardenedCookies(t *testing.T) {
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	recipientID := uuid.New()
	api := &fakeViewerAPI{exchangeResult: viewer.ExchangeResult{
		RawToken: "opaque-viewer-session", CSRFToken: "opaque-viewer-csrf", RecipientID: recipientID,
		DisplayName: "Owner", ExpiresAt: expiresAt,
	}}
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: api, SecureCookies: true,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/viewer/line/session", strings.NewReader(`{
		"idToken":"opaque-line-id-token-that-is-long-enough",
		"invitationReference":"opaque-invitation-reference-that-is-long-enough"
	}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range response.Result().Cookies() {
		switch cookie.Name {
		case viewerSessionCookie:
			sessionCookie = cookie
		case viewerCSRFCookie:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("viewer session cookie is not hardened: %+v", sessionCookie)
	}
	if csrfCookie == nil || csrfCookie.HttpOnly || !csrfCookie.Secure || csrfCookie.Value != "opaque-viewer-csrf" {
		t.Fatalf("viewer CSRF cookie is not usable and hardened: %+v", csrfCookie)
	}
	if body := response.Body.String(); !strings.Contains(body, recipientID.String()) || !strings.Contains(body, `"csrfToken":"opaque-viewer-csrf"`) || strings.Contains(body, "opaque-viewer-session") {
		t.Fatalf("unexpected body = %s", body)
	}
}

func TestViewerSessionMapsSafeLINEFailuresWithoutLeakingToken(t *testing.T) {
	api := &fakeViewerAPI{exchangeErr: &line.SafeError{Code: "LINE_VERIFY_UNAVAILABLE", Retryable: true}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: api})
	secretToken := "secret-line-id-token-that-must-not-leak"
	request := httptest.NewRequest(http.MethodPost, "/api/v1/viewer/line/session", strings.NewReader(`{"idToken":"`+secretToken+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"LINE_VERIFY_UNAVAILABLE"`) || strings.Contains(response.Body.String(), secretToken) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestViewerNavigationUsesAuthenticatedRecipient(t *testing.T) {
	recipientID, tenantID := uuid.New(), uuid.New()
	api := &fakeViewerAPI{
		authenticated: viewer.AuthenticatedViewer{RecipientID: recipientID, DisplayName: "Owner"},
		tenants:       []viewer.TenantAccess{{ID: tenantID, Name: "Shop", Timezone: "Asia/Bangkok", ReportKeys: []report.Key{report.StockBalance}}},
		reports:       []viewer.ReportAccess{{Key: report.StockBalance, Label: "ยอดคงเหลือสินค้า"}},
	}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: api})
	for _, path := range []string{"/api/v1/viewer/tenants", "/api/v1/viewer/tenants/" + tenantID.String() + "/reports"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "opaque-session"})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tenantID.String()) && path == "/api/v1/viewer/tenants" {
			t.Fatalf("GET %s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}

func TestViewerLogoutRequiresCSRF(t *testing.T) {
	api := &fakeViewerAPI{
		authenticated: viewer.AuthenticatedViewer{RecipientID: uuid.New()},
		csrfErr:       auth.ErrInvalidCSRF,
	}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: api})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/viewer/logout", nil)
	request.AddCookie(&http.Cookie{Name: viewerSessionCookie, Value: "opaque-session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || api.logoutCount != 0 {
		t.Fatalf("status = %d, logoutCount = %d, body = %s", response.Code, api.logoutCount, response.Body.String())
	}
}

func TestViewerSessionRejectsUnboundIdentity(t *testing.T) {
	api := &fakeViewerAPI{exchangeErr: viewer.ErrIdentityForbidden}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), ViewerAuth: api})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/viewer/line/session", strings.NewReader(`{"idToken":"opaque-line-id-token-that-is-long-enough"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"LINE_IDENTITY_FORBIDDEN"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
