package api

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

func TestOpenAPIContractIsValidAndContainsCriticalFlows(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("openapi.yaml")
	if err != nil {
		t.Fatalf("load OpenAPI document: %v", err)
	}
	if err := document.Validate(context.Background()); err != nil {
		t.Fatalf("validate OpenAPI document: %v", err)
	}
	if document.Info.Version != "1.1.0" {
		t.Fatalf("contract version = %q, want 1.1.0", document.Info.Version)
	}

	criticalOperations := map[string]string{
		"/api/v1/auth/admin/login": "POST",
		"/api/v1/admin/tenants":    "POST",
		"/api/v1/admin/tenants/{tenantId}/schedules/{scheduleId}/activate":             "POST",
		"/api/v1/admin/tenants/{tenantId}/schedules/{scheduleId}":                      "DELETE",
		"/api/v1/admin/tenants/{tenantId}/schedules/{scheduleId}/restore":              "POST",
		"/api/v1/viewer/line/session":                                                  "POST",
		"/api/v1/viewer/tenants/{tenantId}/executive-overview":                         "GET",
		"/api/v1/viewer/tenants/{tenantId}/executive-overview/refreshes":               "POST",
		"/api/v1/viewer/tenants/{tenantId}/executive-overview/refreshes/{refreshId}":   "GET",
		"/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs":                   "POST",
		"/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs/{runId}/dashboard": "GET",
		"/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs/{runId}/rows":      "GET",
	}
	for path, method := range criticalOperations {
		pathItem := document.Paths.Find(path)
		if pathItem == nil || pathItem.GetOperation(method) == nil {
			t.Errorf("missing %s %s", method, path)
		}
	}
}
