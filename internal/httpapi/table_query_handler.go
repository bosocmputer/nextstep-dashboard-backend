package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tablequery"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type TenantsTableQueryAPI interface {
	QueryTenants(context.Context, tablequery.TenantsInput) (tablequery.TenantsResult, error)
}
type SchedulesTableQueryAPI interface {
	QuerySchedules(context.Context, uuid.UUID, tablequery.SchedulesInput) (tablequery.SchedulesResult, error)
}
type ReportRunsTableQueryAPI interface {
	QueryReportRuns(context.Context, tablequery.ReportRunsInput) (tablequery.ReportRunsResult, error)
}
type DeliveriesTableQueryAPI interface {
	QueryDeliveries(context.Context, tablequery.DeliveriesInput) (tablequery.DeliveriesResult, error)
}
type AuditTableQueryAPI interface {
	QueryAudit(context.Context, tablequery.AuditInput) (tablequery.AuditResult, error)
}
type IncidentsTableQueryAPI interface {
	QueryIncidents(context.Context, tablequery.IncidentsInput) (tablequery.IncidentsResult, error)
}
type OccurrencesTableQueryAPI interface {
	QueryOccurrences(context.Context, uuid.UUID, tablequery.OccurrencesInput) (tablequery.OccurrencesResult, error)
}

func registerTableQueryRoutes(router chi.Router, adminAuth AdminAuthenticator, api any) {
	if service, ok := api.(TenantsTableQueryAPI); ok {
		router.Post("/api/v1/admin/tenants/query", tableQueryHandler(adminAuth, func(ctx context.Context, input tablequery.TenantsInput, _ *http.Request) (any, error) {
			return service.QueryTenants(ctx, input)
		}))
	}
	if service, ok := api.(SchedulesTableQueryAPI); ok {
		router.Post("/api/v1/admin/tenants/{tenantId}/schedules/query", tableQueryHandler(adminAuth, func(ctx context.Context, input tablequery.SchedulesInput, request *http.Request) (any, error) {
			tenantID, err := uuid.Parse(chi.URLParam(request, "tenantId"))
			if err != nil {
				return nil, tablequery.ErrInvalidInput
			}
			return service.QuerySchedules(ctx, tenantID, input)
		}))
	}
	if service, ok := api.(ReportRunsTableQueryAPI); ok {
		router.Post("/api/v1/admin/report-runs/query", tableQueryHandler(adminAuth, func(ctx context.Context, input tablequery.ReportRunsInput, _ *http.Request) (any, error) {
			result, err := service.QueryReportRuns(ctx, input)
			if err != nil {
				return nil, err
			}
			data := make([]map[string]any, 0, len(result.Data))
			for _, item := range result.Data {
				data = append(data, tableReportRunResponse(item))
			}
			return map[string]any{"data": data, "page": result.Page, "pageSize": result.PageSize, "total": result.Total, "totalPages": result.TotalPages}, nil
		}))
	}
	if service, ok := api.(DeliveriesTableQueryAPI); ok {
		router.Post("/api/v1/admin/line-deliveries/query", tableQueryHandler(adminAuth, func(ctx context.Context, input tablequery.DeliveriesInput, _ *http.Request) (any, error) {
			return service.QueryDeliveries(ctx, input)
		}))
	}
	if service, ok := api.(AuditTableQueryAPI); ok {
		router.Post("/api/v1/admin/audit-logs/query", tableQueryHandler(adminAuth, func(ctx context.Context, input tablequery.AuditInput, _ *http.Request) (any, error) {
			return service.QueryAudit(ctx, input)
		}))
	}
	if service, ok := api.(IncidentsTableQueryAPI); ok {
		router.Post("/api/v1/admin/operational-incidents/query", tableQueryHandler(adminAuth, func(ctx context.Context, input tablequery.IncidentsInput, _ *http.Request) (any, error) {
			return service.QueryIncidents(ctx, input)
		}))
	}
	if service, ok := api.(OccurrencesTableQueryAPI); ok {
		router.Post("/api/v1/admin/operational-incidents/{incidentId}/occurrences/query", tableQueryHandler(adminAuth, func(ctx context.Context, input tablequery.OccurrencesInput, request *http.Request) (any, error) {
			incidentID, err := uuid.Parse(chi.URLParam(request, "incidentId"))
			if err != nil {
				return nil, tablequery.ErrInvalidInput
			}
			return service.QueryOccurrences(ctx, incidentID, input)
		}))
	}
}

func tableQueryHandler[T any](adminAuth AdminAuthenticator, invoke func(context.Context, T, *http.Request) (any, error)) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, true); !ok {
			return
		}
		if !requireJSONRequest(response, request) {
			return
		}
		var input T
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Table query is invalid.", false)
			return
		}
		if !tablequery.ValidateEnvelope(&input) {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Table query is invalid.", false)
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		result, err := invoke(ctx, input, request)
		if err != nil {
			handleTableQueryError(response, request, err)
			return
		}
		response.Header().Set("Cache-Control", "no-store")
		writeJSON(response, http.StatusOK, result)
	}
}

func tableReportRunResponse(item operations.ReportRun) map[string]any {
	result := reportRunResponse(item.Run)
	result["tenantName"] = item.TenantName
	result["runtimeStatus"] = item.RuntimeStatus
	result["retryAvailableAt"] = nullableTime(item.RetryAvailableAt)
	if item.WaitReason != nil {
		result["waitReason"] = *item.WaitReason
	}
	if item.FailureSummary != nil {
		result["failureSummary"] = item.FailureSummary
	}
	return result
}

func handleTableQueryError(response http.ResponseWriter, request *http.Request, err error) {
	switch {
	case errors.Is(err, tablequery.ErrInvalidInput):
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Table query is invalid.", false)
	case errors.Is(err, sentinel.ErrNotFound):
		writeProblem(response, request, http.StatusNotFound, "INCIDENT_NOT_FOUND", "Operational incident was not found.", false)
	case errors.Is(err, context.DeadlineExceeded):
		writeProblem(response, request, http.StatusServiceUnavailable, "TABLE_QUERY_TIMEOUT", "Table query took too long.", true)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to query table data.", false)
	}
}
