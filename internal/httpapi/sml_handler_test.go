package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
)

type fakeSMLAPI struct {
	status  sml.ConnectionStatus
	putErr  error
	testErr error
}

func (fake *fakeSMLAPI) Get(context.Context, uuid.UUID) (sml.ConnectionStatus, error) {
	return fake.status, nil
}

func (fake *fakeSMLAPI) Replace(context.Context, []byte, string, uuid.UUID, sml.ConnectionInput) (sml.ConnectionStatus, error) {
	return fake.status, fake.putErr
}

func (fake *fakeSMLAPI) Test(context.Context, []byte, string, uuid.UUID) (sml.ConnectionTestResult, error) {
	return sml.ConnectionTestResult{}, fake.testErr
}

func TestReplaceSMLConnectionNeverEchoesCredentials(t *testing.T) {
	adminAuth := &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin"}}
	smlAPI := &fakeSMLAPI{status: sml.ConnectionStatus{IsConfigured: true, EndpointURL: "http://10.0.0.8:8080", EndpointHost: "10.0.0.8:8080", DatabaseName: "demo", ConfigFileName: "SMLConfigDATA.xml", Version: 1}}
	handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: adminAuth, SMLConnections: smlAPI})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/tenants/4a06e1c2-29cd-4b5a-81d4-b2a26c2e11ec/sml-connection", strings.NewReader(`{"endpointUrl":"http://10.0.0.8/service","configFileName":"SMLConfigDATA.xml","databaseName":"demo","username":"sml-user","password":"sml-password","version":0}`))
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	request.Header.Set("X-CSRF-Token", "csrf")
	request.Header.Set("Idempotency-Key", "replace-sml-connection")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"endpointUrl":"http://10.0.0.8:8080"`) || strings.Contains(body, "sml-user") || strings.Contains(body, "sml-password") {
		t.Fatalf("status = %d, body = %s", response.Code, body)
	}
}

func TestSMLConnectionMapsVersionAndDependencyFailures(t *testing.T) {
	adminAuth := &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin"}}
	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		api        *fakeSMLAPI
		wantStatus int
		wantCode   string
	}{
		{name: "version", method: http.MethodPut, path: "/api/v1/admin/tenants/4a06e1c2-29cd-4b5a-81d4-b2a26c2e11ec/sml-connection", body: `{"endpointUrl":"http://10.0.0.8/service","configFileName":"SMLConfigDATA.xml","databaseName":"demo","username":"user","password":"password","version":1}`, api: &fakeSMLAPI{putErr: sml.ErrConnectionVersionConflict}, wantStatus: 409, wantCode: "VERSION_CONFLICT"},
		{name: "dependency", method: http.MethodPost, path: "/api/v1/admin/tenants/4a06e1c2-29cd-4b5a-81d4-b2a26c2e11ec/sml-connection/test", api: &fakeSMLAPI{testErr: &sml.ConnectionTestError{SafeCode: "SML_TIMEOUT", Retryable: true}}, wantStatus: 424, wantCode: "SML_TIMEOUT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHandler(Dependencies{Readiness: readinessFunc(func(context.Context) error { return nil }), AdminAuth: adminAuth, SMLConnections: test.api})
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
			request.Header.Set("X-CSRF-Token", "csrf")
			request.Header.Set("Idempotency-Key", "sml-operation")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || !strings.Contains(response.Body.String(), `"code":"`+test.wantCode+`"`) {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}
