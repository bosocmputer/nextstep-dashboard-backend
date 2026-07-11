package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
)

func TestAdminReportCatalogReturnsSafeDefinitionsAndServerLimits(t *testing.T) {
	handler := NewHandler(Dependencies{
		Readiness: readinessFunc(func(context.Context) error { return nil }),
		AdminAuth: &fakeAdminAuth{admin: auth.AuthenticatedAdmin{Username: "superadmin"}},
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/reports", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: "session"})
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"maxScheduleReports":10`) || !strings.Contains(body, `"maxFlexPayloadBytes":30720`) {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
	for _, forbidden := range []string{"contractJson", "query", "sql", "isSensitive"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("catalog leaked %q: %s", forbidden, body)
		}
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}
