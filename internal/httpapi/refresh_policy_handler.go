package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type RefreshPolicyAPI interface {
	Get(context.Context, uuid.UUID) (report.RefreshPolicy, error)
	Put(context.Context, []byte, string, uuid.UUID, report.RefreshPolicyInput) (report.RefreshPolicy, error)
}

func registerRefreshPolicyRoutes(router chi.Router, adminAuth AdminAuthenticator, policies RefreshPolicyAPI) {
	router.Get("/api/v1/admin/tenants/{tenantId}/dashboard-refresh-policy", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		policy, err := policies.Get(request.Context(), tenantID)
		if handleRefreshPolicyError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, policy)
	})

	router.Put("/api/v1/admin/tenants/{tenantId}/dashboard-refresh-policy", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		if !isJSONRequest(request) {
			writeProblem(response, request, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.", false)
			return
		}
		var input report.RefreshPolicyInput
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Dashboard refresh policy is invalid.", false)
			return
		}
		policy, err := policies.Put(request.Context(), admin.TokenHash, requestID(request), tenantID, input)
		if handleRefreshPolicyError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, policy)
	})
}

func handleRefreshPolicyError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, report.ErrRefreshPolicyInvalid):
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Dashboard refresh intervals are invalid.", false)
	case errors.Is(err, report.ErrRefreshPolicyConflict):
		writeProblem(response, request, http.StatusConflict, "VERSION_CONFLICT", "Dashboard refresh policy changed in another session.", false)
	case errors.Is(err, tenant.ErrNotFound):
		writeProblem(response, request, http.StatusNotFound, "TENANT_NOT_FOUND", "Tenant was not found.", false)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to manage dashboard refresh policy.", false)
	}
	return true
}
