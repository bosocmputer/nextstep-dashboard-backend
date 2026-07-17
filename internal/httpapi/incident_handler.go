package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type IncidentAPI interface {
	List(context.Context, sentinel.IncidentFilter) (sentinel.IncidentPage, error)
	Get(context.Context, uuid.UUID) (sentinel.IncidentDetail, error)
	Occurrences(context.Context, uuid.UUID, sentinel.OccurrenceFilter) (sentinel.OccurrencePage, error)
	Acknowledge(context.Context, uuid.UUID, int) (sentinel.Incident, error)
	AcceptRisk(context.Context, uuid.UUID, int, string) (sentinel.Incident, error)
}

func registerIncidentRoutes(router chi.Router, adminAuth AdminAuthenticator, incidents IncidentAPI) {
	router.Get("/api/v1/admin/operational-incidents", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		pageSize := 25
		if raw := request.URL.Query().Get("pageSize"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 100 {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Page size is invalid.", false)
				return
			}
			pageSize = value
		}
		filter := sentinel.IncidentFilter{Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize, ActiveOnly: request.URL.Query().Get("scope") != "ALL"}
		if raw := request.URL.Query().Get("status"); raw != "" {
			value := sentinel.Status(raw)
			if value != sentinel.StatusOpen && value != sentinel.StatusAcknowledged && value != sentinel.StatusResolved && value != sentinel.StatusClosedAccepted {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Incident status is invalid.", false)
				return
			}
			filter.Status = &value
		}
		if raw := request.URL.Query().Get("severity"); raw != "" {
			value := sentinel.Severity(raw)
			if value != sentinel.SeverityP1 && value != sentinel.SeverityP2 {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Incident severity is invalid.", false)
				return
			}
			filter.Severity = &value
		}
		page, err := incidents.List(request.Context(), filter)
		if handleIncidentError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": page.Data, "page": operationsPage(page.NextCursor, page.HasMore)})
	})

	router.Get("/api/v1/admin/operational-incidents/{incidentId}/occurrences", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		id, ok := parseIncidentID(response, request)
		if !ok {
			return
		}
		pageSize := 50
		if raw := request.URL.Query().Get("pageSize"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 100 {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Page size is invalid.", false)
				return
			}
			pageSize = value
		}
		page, err := incidents.Occurrences(request.Context(), id, sentinel.OccurrenceFilter{Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize})
		if handleIncidentError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": page.Data, "page": operationsPage(page.NextCursor, page.HasMore)})
	})

	router.Get("/api/v1/admin/operational-incidents/{incidentId}", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		id, ok := parseIncidentID(response, request)
		if !ok {
			return
		}
		detail, err := incidents.Get(request.Context(), id)
		if handleIncidentError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, detail)
	})

	router.Post("/api/v1/admin/operational-incidents/{incidentId}/acknowledge", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, true); !ok {
			return
		}
		id, ok := parseIncidentID(response, request)
		if !ok {
			return
		}
		var input struct {
			Version int `json:"version"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Incident input is invalid.", false)
			return
		}
		item, err := incidents.Acknowledge(request.Context(), id, input.Version)
		if handleIncidentError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, item)
	})

	router.Post("/api/v1/admin/operational-incidents/{incidentId}/accept-risk", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, true); !ok {
			return
		}
		id, ok := parseIncidentID(response, request)
		if !ok {
			return
		}
		var input struct {
			Version int    `json:"version"`
			Reason  string `json:"reason"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Incident input is invalid.", false)
			return
		}
		item, err := incidents.AcceptRisk(request.Context(), id, input.Version, input.Reason)
		if handleIncidentError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, item)
	})
}

func parseIncidentID(response http.ResponseWriter, request *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(request, "incidentId"))
	if err != nil {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Incident ID is invalid.", false)
		return uuid.Nil, false
	}
	return id, true
}

func handleIncidentError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, sentinel.ErrInvalidInput):
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Incident input is invalid.", false)
	case errors.Is(err, sentinel.ErrNotFound):
		writeProblem(response, request, http.StatusNotFound, "NOT_FOUND", "Incident was not found.", false)
	case errors.Is(err, sentinel.ErrVersionConflict):
		writeProblem(response, request, http.StatusConflict, "VERSION_CONFLICT", "Incident changed since it was loaded.", false)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to process operational incident.", false)
	}
	return true
}
