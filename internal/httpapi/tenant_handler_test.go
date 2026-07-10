package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/google/uuid"
)

type fakeTenantAPI struct {
	created     tenant.Tenant
	createErr   error
	createCount int
	updated     tenant.Tenant
	updateErr   error
}

func (fake *fakeTenantAPI) Create(context.Context, []byte, string, string, tenant.CreateInput) (tenant.Tenant, error) {
	fake.createCount++
	return fake.created, fake.createErr
}

func (fake *fakeTenantAPI) List(context.Context, tenant.ListFilter) (tenant.Page, error) {
	return tenant.Page{}, nil
}

func (fake *fakeTenantAPI) Get(context.Context, uuid.UUID) (tenant.Tenant, error) {
	return tenant.Tenant{}, tenant.ErrNotFound
}

func (fake *fakeTenantAPI) Update(context.Context, []byte, string, uuid.UUID, tenant.PatchInput) (tenant.Tenant, error) {
	return fake.updated, fake.updateErr
}

func TestCreateTenantRequiresBootstrapPasswordRotation(t *testing.T) {
	adminAuth := &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin", MustRotatePassword: true}}
	tenantAPI := &fakeTenantAPI{}
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }),
		AdminAuth: adminAuth,
		Tenants:   tenantAPI,
	})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants", strings.NewReader(`{"slug":"shop-one","name":"Shop","timezone":"Asia/Bangkok","accessEndsAt":"2027-07-10T00:00:00Z"}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	request.Header.Set("X-CSRF-Token", "csrf")
	request.Header.Set("Idempotency-Key", "create-shop-one")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if tenantAPI.createCount != 0 {
		t.Fatal("tenant was created before bootstrap password rotation")
	}
}

func TestCreateTenantMapsValidationAndConflict(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{name: "field validation", err: &tenant.ValidationError{Field: "slug", Code: "INVALID_SLUG", Message: "invalid"}, wantStatus: 422, wantCode: "VALIDATION_ERROR"},
		{name: "duplicate slug", err: tenant.ErrConflict, wantStatus: 409, wantCode: "CONFLICT"},
		{name: "idempotency mismatch", err: tenant.ErrIdempotencyConflict, wantStatus: 409, wantCode: "IDEMPOTENCY_CONFLICT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			adminAuth := &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin"}}
			tenantAPI := &fakeTenantAPI{createErr: test.err}
			handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: adminAuth, Tenants: tenantAPI})
			request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/tenants", strings.NewReader(`{"slug":"shop-one","name":"Shop","timezone":"Asia/Bangkok","accessEndsAt":"2027-07-10T00:00:00Z"}`))
			request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
			request.Header.Set("X-CSRF-Token", "csrf")
			request.Header.Set("Idempotency-Key", "create-shop-one")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestPatchTenantReturnsOptimisticConflict(t *testing.T) {
	adminAuth := &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin"}}
	tenantAPI := &fakeTenantAPI{updateErr: tenant.ErrConflict}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: adminAuth, Tenants: tenantAPI})
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/admin/tenants/4a06e1c2-29cd-4b5a-81d4-b2a26c2e11ec", strings.NewReader(`{"version":1,"name":"New name"}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	request.Header.Set("X-CSRF-Token", "csrf")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"VERSION_CONFLICT"`) {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestTenantRoutesRejectInvalidIdentifierAndPageSize(t *testing.T) {
	adminAuth := &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin", ExpiresAt: time.Now().Add(time.Hour)}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: adminAuth, Tenants: &fakeTenantAPI{}})
	for _, path := range []string{
		"/api/v1/admin/tenants/not-a-uuid",
		"/api/v1/admin/tenants?pageSize=1000",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s status = %d, body = %s", path, response.Code, response.Body.String())
		}
	}
}
