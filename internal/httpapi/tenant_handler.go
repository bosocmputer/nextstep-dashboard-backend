package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type TenantAPI interface {
	Create(context.Context, []byte, string, string, tenant.CreateInput) (tenant.Tenant, error)
	List(context.Context, tenant.ListFilter) (tenant.Page, error)
	Get(context.Context, uuid.UUID) (tenant.Tenant, error)
	Update(context.Context, []byte, string, uuid.UUID, tenant.PatchInput) (tenant.Tenant, error)
}

func registerTenantRoutes(router chi.Router, adminAuth AdminAuthenticator, tenants TenantAPI) {
	router.Get("/api/v1/admin/tenants", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		pageSize := 25
		if raw := request.URL.Query().Get("pageSize"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 100 {
				writeValidationProblem(response, request, &tenant.ValidationError{Field: "pageSize", Code: "INVALID_PAGE_SIZE", Message: "Page size must be between 1 and 100."})
				return
			}
			pageSize = value
		}
		var status *tenant.Status
		if raw := request.URL.Query().Get("status"); raw != "" {
			value := tenant.Status(raw)
			status = &value
		}
		page, err := tenants.List(request.Context(), tenant.ListFilter{
			Cursor:   request.URL.Query().Get("cursor"),
			PageSize: pageSize,
			Status:   status,
			Search:   request.URL.Query().Get("search"),
		})
		if handleTenantError(response, request, err, "Unable to list tenants.") {
			return
		}
		var nextCursor any
		if page.NextCursor != "" {
			nextCursor = page.NextCursor
		}
		writeJSON(response, http.StatusOK, map[string]any{
			"data": page.Data,
			"page": map[string]any{"nextCursor": nextCursor, "hasMore": page.HasMore},
		})
	})

	router.Post("/api/v1/admin/tenants", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		var input struct {
			Slug         string    `json:"slug"`
			Name         string    `json:"name"`
			Timezone     string    `json:"timezone"`
			AccessEndsAt time.Time `json:"accessEndsAt"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Tenant input is invalid.", false)
			return
		}
		created, err := tenants.Create(request.Context(), admin.TokenHash, requestID(request), request.Header.Get("Idempotency-Key"), tenant.CreateInput{
			Slug: input.Slug, Name: input.Name, Timezone: input.Timezone, AccessEndsAt: input.AccessEndsAt,
		})
		if handleTenantError(response, request, err, "Unable to create tenant.") {
			return
		}
		writeJSON(response, http.StatusCreated, created)
	})

	router.Get("/api/v1/admin/tenants/{tenantId}", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		id, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		item, err := tenants.Get(request.Context(), id)
		if handleTenantError(response, request, err, "Unable to get tenant.") {
			return
		}
		writeJSON(response, http.StatusOK, item)
	})

	router.Patch("/api/v1/admin/tenants/{tenantId}", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		id, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		var input struct {
			Name         *string        `json:"name"`
			Timezone     *string        `json:"timezone"`
			Status       *tenant.Status `json:"status"`
			AccessEndsAt *time.Time     `json:"accessEndsAt"`
			Version      int            `json:"version"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Tenant patch is invalid.", false)
			return
		}
		updated, err := tenants.Update(request.Context(), admin.TokenHash, requestID(request), id, tenant.PatchInput{
			Name: input.Name, Timezone: input.Timezone, Status: input.Status, AccessEndsAt: input.AccessEndsAt, Version: input.Version,
		})
		if handleTenantError(response, request, err, "Unable to update tenant.") {
			return
		}
		writeJSON(response, http.StatusOK, updated)
	})
}

func operationalAdmin(response http.ResponseWriter, request *http.Request, adminAuth AdminAuthenticator, requireCSRF bool) (auth.AuthenticatedAdmin, bool) {
	admin, ok := authenticateAdmin(response, request, adminAuth)
	if !ok {
		return auth.AuthenticatedAdmin{}, false
	}
	if admin.MustRotatePassword {
		writeProblem(response, request, http.StatusForbidden, "PASSWORD_ROTATION_REQUIRED", "Rotate the bootstrap password before continuing.", false)
		return auth.AuthenticatedAdmin{}, false
	}
	if requireCSRF && !authorizeCSRF(response, request, adminAuth, admin) {
		return auth.AuthenticatedAdmin{}, false
	}
	return admin, true
}

func parseTenantID(response http.ResponseWriter, request *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(request, "tenantId"))
	if err != nil {
		writeValidationProblem(response, request, &tenant.ValidationError{Field: "tenantId", Code: "INVALID_ID", Message: "Tenant ID must be a UUID."})
		return uuid.Nil, false
	}
	return id, true
}

func handleTenantError(response http.ResponseWriter, request *http.Request, err error, internalMessage string) bool {
	if err == nil {
		return false
	}
	var validationError *tenant.ValidationError
	switch {
	case errors.As(err, &validationError):
		writeValidationProblem(response, request, validationError)
	case errors.Is(err, tenant.ErrNotFound):
		writeProblem(response, request, http.StatusNotFound, "NOT_FOUND", "Tenant was not found.", false)
	case errors.Is(err, tenant.ErrIdempotencyConflict):
		writeProblem(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency key was already used with different input.", false)
	case errors.Is(err, tenant.ErrConflict):
		code := "CONFLICT"
		message := "Tenant already exists."
		if request.Method == http.MethodPatch {
			code = "VERSION_CONFLICT"
			message = "Tenant changed since it was loaded. Reload before saving again."
		}
		writeProblem(response, request, http.StatusConflict, code, message, false)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", internalMessage, false)
	}
	return true
}

func requestID(request *http.Request) string {
	value, _ := request.Context().Value(requestIDContextKey{}).(string)
	return value
}
