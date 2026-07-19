package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type RecipientAPI interface {
	CreateInvitation(context.Context, []byte, string, string, uuid.UUID, string) (recipient.Recipient, error)
	ReissueInvitation(context.Context, []byte, string, string, uuid.UUID, uuid.UUID) (recipient.Recipient, error)
	List(context.Context, uuid.UUID, int, string) (recipient.RecipientPage, error)
	GetForTenant(context.Context, uuid.UUID, uuid.UUID) (recipient.Recipient, error)
	PermissionDependencies(context.Context, uuid.UUID, uuid.UUID) (recipient.PermissionDependencies, error)
	ScheduleRecipientOptions(context.Context, uuid.UUID, recipient.ScheduleRecipientOptionsInput) (recipient.ScheduleRecipientOptions, error)
	Query(context.Context, uuid.UUID, recipient.QueryInput) (recipient.QueryResult, error)
	ReplacePermissions(context.Context, []byte, string, uuid.UUID, uuid.UUID, []report.Key, int) (recipient.Recipient, error)
	Revoke(context.Context, []byte, string, uuid.UUID, uuid.UUID) error
}

func registerRecipientRoutes(router chi.Router, adminAuth AdminAuthenticator, recipients RecipientAPI) {
	router.Post("/api/v1/admin/tenants/{tenantId}/recipients/query", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, true); !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		var input recipient.QueryInput
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient query is invalid.", false)
			return
		}
		result, err := recipients.Query(request.Context(), tenantID, input)
		if handleRecipientError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, result)
	})

	router.Post("/api/v1/admin/tenants/{tenantId}/schedule-recipient-options/query", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		var input recipient.ScheduleRecipientOptionsInput
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Schedule recipient query is invalid.", false)
			return
		}
		options, err := recipients.ScheduleRecipientOptions(request.Context(), tenantID, input)
		if handleRecipientError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, options)
	})

	router.Get("/api/v1/admin/tenants/{tenantId}/recipients", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		pageSize := 25
		if raw := request.URL.Query().Get("pageSize"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 100 {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient page size must be between 1 and 100.", false)
				return
			}
			pageSize = value
		}
		page, err := recipients.List(request.Context(), tenantID, pageSize, request.URL.Query().Get("cursor"))
		if handleRecipientError(response, request, err) {
			return
		}
		var nextCursor any
		if page.NextCursor != "" {
			nextCursor = page.NextCursor
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": page.Data, "page": map[string]any{"nextCursor": nextCursor, "hasMore": page.HasMore}})
	})

	router.Post("/api/v1/admin/tenants/{tenantId}/recipients", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok || !validIdempotencyHeader(response, request) {
			return
		}
		var input struct {
			InvitationLabel string `json:"invitationLabel"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient invitation input is invalid.", false)
			return
		}
		created, err := recipients.CreateInvitation(request.Context(), admin.TokenHash, requestID(request), request.Header.Get("Idempotency-Key"), tenantID, input.InvitationLabel)
		if handleRecipientError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusCreated, created)
	})

	router.Get("/api/v1/admin/tenants/{tenantId}/recipients/{recipientId}", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		recipientID, err := uuid.Parse(chi.URLParam(request, "recipientId"))
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient ID must be a UUID.", false)
			return
		}
		item, err := recipients.GetForTenant(request.Context(), tenantID, recipientID)
		if handleRecipientError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, item)
	})

	router.Post("/api/v1/admin/tenants/{tenantId}/recipients/{recipientId}/invitation", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok || !validIdempotencyHeader(response, request) {
			return
		}
		recipientID, err := uuid.Parse(chi.URLParam(request, "recipientId"))
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient ID must be a UUID.", false)
			return
		}
		updated, err := recipients.ReissueInvitation(request.Context(), admin.TokenHash, requestID(request), request.Header.Get("Idempotency-Key"), tenantID, recipientID)
		if handleRecipientError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, updated)
	})

	router.Delete("/api/v1/admin/tenants/{tenantId}/recipients/{recipientId}", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		recipientID, err := uuid.Parse(chi.URLParam(request, "recipientId"))
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient ID must be a UUID.", false)
			return
		}
		if err := recipients.Revoke(request.Context(), admin.TokenHash, requestID(request), tenantID, recipientID); handleRecipientError(response, request, err) {
			return
		}
		response.WriteHeader(http.StatusNoContent)
	})

	router.Put("/api/v1/admin/tenants/{tenantId}/recipients/{recipientId}/permissions", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		recipientID, err := uuid.Parse(chi.URLParam(request, "recipientId"))
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient ID must be a UUID.", false)
			return
		}
		var input struct {
			ReportKeys []report.Key `json:"reportKeys"`
			Version    int          `json:"version"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Permission input is invalid.", false)
			return
		}
		updated, err := recipients.ReplacePermissions(request.Context(), admin.TokenHash, requestID(request), tenantID, recipientID, input.ReportKeys, input.Version)
		if handleRecipientError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, updated)
	})

	router.Get("/api/v1/admin/tenants/{tenantId}/recipients/{recipientId}/permission-dependencies", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		recipientID, err := uuid.Parse(chi.URLParam(request, "recipientId"))
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient ID must be a UUID.", false)
			return
		}
		dependencies, err := recipients.PermissionDependencies(request.Context(), tenantID, recipientID)
		if handleRecipientError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, dependencies)
	})
}

func handleRecipientError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, recipient.ErrRecipientNotFound):
		writeProblem(response, request, http.StatusNotFound, "RECIPIENT_NOT_FOUND", "Recipient was not found for this tenant.", false)
	case errors.Is(err, recipient.ErrPermissionInvalid):
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report permissions are invalid.", false)
	case errors.Is(err, recipient.ErrInvalidInput):
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient input is invalid.", false)
	case errors.Is(err, recipient.ErrIdempotencyConflict):
		writeProblem(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency key was already used with different recipient input.", false)
	case errors.Is(err, recipient.ErrVersionConflict):
		writeProblem(response, request, http.StatusConflict, "VERSION_CONFLICT", "Report permissions changed in another session. Reload before saving again.", false)
	case errors.Is(err, recipient.ErrInvitationNotPending):
		writeProblem(response, request, http.StatusConflict, "INVITATION_NOT_PENDING", "Only a pending recipient invitation can be reissued.", false)
	default:
		var recipientInUse *recipient.RecipientInUseError
		if errors.As(err, &recipientInUse) {
			fieldErrors := make([]fieldError, 0, min(5, len(recipientInUse.ScheduleNames))+1)
			for _, name := range recipientInUse.ScheduleNames[:min(5, len(recipientInUse.ScheduleNames))] {
				fieldErrors = append(fieldErrors, fieldError{Field: "recipientId", Code: "ACTIVE_SCHEDULE_DEPENDENCY", Message: name})
			}
			if len(recipientInUse.ScheduleNames) > 5 {
				fieldErrors = append(fieldErrors, fieldError{Field: "recipientId", Code: "ADDITIONAL_DEPENDENCIES", Message: "มีตารางอื่นที่กำลังใช้งานเพิ่มเติม"})
			}
			writeJSON(response, http.StatusConflict, problemEnvelope{Error: problem{Code: "RECIPIENT_IN_USE", Message: "Pause or edit active schedules before removing this recipient.", RequestID: requestID(request), FieldErrors: fieldErrors}})
			break
		}
		var inUse *recipient.PermissionInUseError
		if errors.As(err, &inUse) {
			fieldErrors := make([]fieldError, 0, min(5, len(inUse.ScheduleNames))+1)
			for _, name := range inUse.ScheduleNames[:min(5, len(inUse.ScheduleNames))] {
				fieldErrors = append(fieldErrors, fieldError{Field: "reportKeys", Code: "ACTIVE_SCHEDULE_DEPENDENCY", Message: name})
			}
			if len(inUse.ScheduleNames) > 5 {
				fieldErrors = append(fieldErrors, fieldError{Field: "reportKeys", Code: "ADDITIONAL_DEPENDENCIES", Message: "มีตารางอื่นที่กำลังใช้งานเพิ่มเติม"})
			}
			writeJSON(response, http.StatusConflict, problemEnvelope{Error: problem{Code: "PERMISSION_IN_USE", Message: "Pause or edit active schedules before removing these permissions.", RequestID: requestID(request), FieldErrors: fieldErrors}})
			break
		}
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to process the recipient request.", false)
	}
	return true
}
