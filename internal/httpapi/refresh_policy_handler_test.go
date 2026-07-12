package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type refreshPolicyAPIFake struct {
	policy report.RefreshPolicy
	input  report.RefreshPolicyInput
	err    error
}

func (fake *refreshPolicyAPIFake) Get(context.Context, uuid.UUID) (report.RefreshPolicy, error) {
	return fake.policy, fake.err
}

func (fake *refreshPolicyAPIFake) Put(_ context.Context, _ []byte, _ string, _ uuid.UUID, input report.RefreshPolicyInput) (report.RefreshPolicy, error) {
	fake.input = input
	return fake.policy, fake.err
}

func TestAdminUpdatesDashboardRefreshPolicyWithCSRF(t *testing.T) {
	tenantID := uuid.New()
	fast, standard, heavy := 10, 30, 60
	api := &refreshPolicyAPIFake{policy: report.RefreshPolicy{TenantID: tenantID, FastIntervalMinutes: &fast, StandardIntervalMinutes: &standard, HeavyIntervalMinutes: &heavy, Version: 3}}
	handler := NewHandler(Dependencies{
		Readiness:       readinessFunc(func(context.Context) error { return nil }),
		AdminAuth:       &fakeAdminAuth{admin: auth.AuthenticatedAdmin{TokenHash: []byte("actor")}},
		RefreshPolicies: api,
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/tenants/"+tenantID.String()+"/dashboard-refresh-policy", strings.NewReader(`{"fastIntervalMinutes":10,"standardIntervalMinutes":30,"heavyIntervalMinutes":60,"version":2}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "csrf-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || api.input.Version != 2 || !strings.Contains(response.Body.String(), `"version":3`) {
		t.Fatalf("status=%d input=%+v body=%s", response.Code, api.input, response.Body.String())
	}
}

func TestAdminRefreshPolicyMapsVersionConflict(t *testing.T) {
	tenantID := uuid.New()
	handler := NewHandler(Dependencies{
		Readiness:       readinessFunc(func(context.Context) error { return nil }),
		AdminAuth:       &fakeAdminAuth{admin: auth.AuthenticatedAdmin{TokenHash: []byte("actor")}},
		RefreshPolicies: &refreshPolicyAPIFake{err: report.ErrRefreshPolicyConflict},
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/tenants/"+tenantID.String()+"/dashboard-refresh-policy", strings.NewReader(`{"fastIntervalMinutes":5,"standardIntervalMinutes":15,"heavyIntervalMinutes":30,"version":1}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "admin-session"})
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", "csrf-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"VERSION_CONFLICT"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
