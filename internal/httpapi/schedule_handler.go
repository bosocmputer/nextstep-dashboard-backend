package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type ScheduleAPI interface {
	Create(context.Context, []byte, string, string, uuid.UUID, schedule.Input) (schedule.Schedule, error)
	List(context.Context, uuid.UUID, int, string) (schedule.Page, error)
	Get(context.Context, uuid.UUID, uuid.UUID) (schedule.Schedule, error)
	Update(context.Context, []byte, string, uuid.UUID, uuid.UUID, schedule.Input, int) (schedule.Schedule, error)
	Activate(context.Context, []byte, string, uuid.UUID, uuid.UUID) (schedule.Schedule, error)
	Pause(context.Context, []byte, string, uuid.UUID, uuid.UUID) (schedule.Schedule, error)
}

type SchedulePreviewAPI interface {
	Preview(context.Context, uuid.UUID, line.FlexPreviewInput) (line.FlexPreview, error)
}

type ScheduleTestSendAPI interface {
	Enqueue(context.Context, []byte, string, string, uuid.UUID, uuid.UUID) (schedule.Execution, error)
}

type scheduleInputBody struct {
	Name         string        `json:"name"`
	DaysOfWeek   []int         `json:"daysOfWeek"`
	LocalTime    string        `json:"localTime"`
	Timezone     string        `json:"timezone"`
	PeriodPreset report.Preset `json:"periodPreset"`
	ReportKeys   []report.Key  `json:"reportKeys"`
	RecipientIDs []uuid.UUID   `json:"recipientIds"`
}

func (body scheduleInputBody) input() schedule.Input {
	return schedule.Input{
		Name: body.Name, DaysOfWeek: body.DaysOfWeek, LocalTime: body.LocalTime, Timezone: body.Timezone,
		PeriodPreset: body.PeriodPreset, ReportKeys: body.ReportKeys, RecipientIDs: body.RecipientIDs,
	}
}

func registerScheduleRoutes(router chi.Router, adminAuth AdminAuthenticator, schedules ScheduleAPI, previews SchedulePreviewAPI, testSends ScheduleTestSendAPI) {
	router.Get("/api/v1/admin/tenants/{tenantId}/schedules", func(response http.ResponseWriter, request *http.Request) {
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
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Schedule page size must be between 1 and 100.", false)
				return
			}
			pageSize = value
		}
		page, err := schedules.List(request.Context(), tenantID, pageSize, request.URL.Query().Get("cursor"))
		if handleScheduleError(response, request, err) {
			return
		}
		var nextCursor any
		if page.NextCursor != "" {
			nextCursor = page.NextCursor
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": page.Data, "page": map[string]any{"nextCursor": nextCursor, "hasMore": page.HasMore}})
	})

	router.Post("/api/v1/admin/tenants/{tenantId}/schedules", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok || !validIdempotencyHeader(response, request) {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok || !requireJSONRequest(response, request) {
			return
		}
		var body scheduleInputBody
		if err := decodeJSON(response, request, &body); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Schedule input is invalid.", false)
			return
		}
		created, err := schedules.Create(request.Context(), admin.TokenHash, requestID(request), request.Header.Get("Idempotency-Key"), tenantID, body.input())
		if handleScheduleError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusCreated, created)
	})

	if previews != nil {
		router.Post("/api/v1/admin/tenants/{tenantId}/schedules/preview", func(response http.ResponseWriter, request *http.Request) {
			if _, ok := operationalAdmin(response, request, adminAuth, true); !ok {
				return
			}
			tenantID, ok := parseTenantID(response, request)
			if !ok || !requireJSONRequest(response, request) {
				return
			}
			var body line.FlexPreviewInput
			if err := decodeJSON(response, request, &body); err != nil {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Flex preview input is invalid.", false)
				return
			}
			preview, err := previews.Preview(request.Context(), tenantID, body)
			if handleFlexPreviewError(response, request, err) {
				return
			}
			writeJSON(response, http.StatusOK, preview)
		})
	}

	if testSends != nil {
		router.Post("/api/v1/admin/tenants/{tenantId}/schedules/{scheduleId}/test-send", func(response http.ResponseWriter, request *http.Request) {
			admin, ok := operationalAdmin(response, request, adminAuth, true)
			if !ok || !validIdempotencyHeader(response, request) {
				return
			}
			tenantID, scheduleID, ok := parseSchedulePath(response, request)
			if !ok {
				return
			}
			execution, err := testSends.Enqueue(
				request.Context(), admin.TokenHash, requestID(request), request.Header.Get("Idempotency-Key"), tenantID, scheduleID,
			)
			if handleScheduleError(response, request, err) {
				return
			}
			writeJSON(response, http.StatusAccepted, execution)
		})
	}

	router.Get("/api/v1/admin/tenants/{tenantId}/schedules/{scheduleId}", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		tenantID, scheduleID, ok := parseSchedulePath(response, request)
		if !ok {
			return
		}
		item, err := schedules.Get(request.Context(), tenantID, scheduleID)
		if handleScheduleError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, item)
	})

	router.Patch("/api/v1/admin/tenants/{tenantId}/schedules/{scheduleId}", func(response http.ResponseWriter, request *http.Request) {
		admin, ok := operationalAdmin(response, request, adminAuth, true)
		if !ok {
			return
		}
		tenantID, scheduleID, ok := parseSchedulePath(response, request)
		if !ok || !requireJSONRequest(response, request) {
			return
		}
		var body struct {
			scheduleInputBody
			Version int `json:"version"`
		}
		if err := decodeJSON(response, request, &body); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Schedule patch is invalid.", false)
			return
		}
		updated, err := schedules.Update(request.Context(), admin.TokenHash, requestID(request), tenantID, scheduleID, body.scheduleInputBody.input(), body.Version)
		if handleScheduleError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, updated)
	})

	for _, operation := range []struct {
		path   string
		invoke func(context.Context, []byte, string, uuid.UUID, uuid.UUID) (schedule.Schedule, error)
	}{
		{path: "activate", invoke: schedules.Activate},
		{path: "pause", invoke: schedules.Pause},
	} {
		operation := operation
		router.Post("/api/v1/admin/tenants/{tenantId}/schedules/{scheduleId}/"+operation.path, func(response http.ResponseWriter, request *http.Request) {
			admin, ok := operationalAdmin(response, request, adminAuth, true)
			if !ok {
				return
			}
			tenantID, scheduleID, ok := parseSchedulePath(response, request)
			if !ok {
				return
			}
			item, err := operation.invoke(request.Context(), admin.TokenHash, requestID(request), tenantID, scheduleID)
			if handleScheduleError(response, request, err) {
				return
			}
			writeJSON(response, http.StatusOK, item)
		})
	}
}

func handleFlexPreviewError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var validationError *line.FlexPreviewValidationError
	switch {
	case errors.As(err, &validationError):
		writeJSON(response, http.StatusUnprocessableEntity, problemEnvelope{Error: problem{
			Code: "VALIDATION_ERROR", Message: "Flex preview input is invalid.", RequestID: requestID(request),
			FieldErrors: []fieldError{{Field: validationError.Field, Code: validationError.Code, Message: "Preview field is invalid."}},
		}})
	case errors.Is(err, tenant.ErrNotFound):
		writeProblem(response, request, http.StatusNotFound, "TENANT_NOT_FOUND", "Tenant was not found.", false)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to render the Flex preview.", false)
	}
	return true
}

func parseSchedulePath(response http.ResponseWriter, request *http.Request) (uuid.UUID, uuid.UUID, bool) {
	tenantID, ok := parseTenantID(response, request)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	scheduleID, err := uuid.Parse(chi.URLParam(request, "scheduleId"))
	if err != nil {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Schedule ID must be a UUID.", false)
		return uuid.Nil, uuid.Nil, false
	}
	return tenantID, scheduleID, true
}

func requireJSONRequest(response http.ResponseWriter, request *http.Request) bool {
	if !isJSONRequest(request) {
		writeProblem(response, request, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.", false)
		return false
	}
	return true
}

func handleScheduleError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	var validationError *schedule.ValidationError
	var readinessError *schedule.ReadinessError
	switch {
	case errors.As(err, &validationError):
		writeJSON(response, http.StatusUnprocessableEntity, problemEnvelope{Error: problem{
			Code: "VALIDATION_ERROR", Message: "Schedule input is invalid.", RequestID: requestID(request),
			FieldErrors: []fieldError{{Field: validationError.Field, Code: validationError.Code, Message: "Schedule field is invalid."}},
		}})
	case errors.As(err, &readinessError):
		fieldErrors := make([]fieldError, 0, len(readinessError.Blockers))
		for _, blocker := range readinessError.Blockers {
			fieldErrors = append(fieldErrors, fieldError{Field: "readiness", Code: blocker, Message: "Resolve this readiness blocker before activation."})
		}
		writeJSON(response, http.StatusUnprocessableEntity, problemEnvelope{Error: problem{
			Code: "SCHEDULE_NOT_READY", Message: "Schedule readiness checks failed.", RequestID: requestID(request), FieldErrors: fieldErrors,
		}})
	case errors.Is(err, schedule.ErrNotFound):
		writeProblem(response, request, http.StatusNotFound, "SCHEDULE_NOT_FOUND", "Schedule was not found.", false)
	case errors.Is(err, schedule.ErrVersionConflict):
		writeProblem(response, request, http.StatusConflict, "VERSION_CONFLICT", "Schedule changed since it was loaded. Reload before saving again.", false)
	case errors.Is(err, schedule.ErrStateConflict):
		writeProblem(response, request, http.StatusConflict, "SCHEDULE_STATE_CONFLICT", "Schedule state does not allow this operation.", false)
	case errors.Is(err, schedule.ErrConflict):
		writeProblem(response, request, http.StatusConflict, "SCHEDULE_CONFLICT", "Schedule name or idempotency input conflicts with existing data.", false)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to process the schedule.", false)
	}
	return true
}
