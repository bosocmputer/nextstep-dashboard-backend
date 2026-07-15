package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type OperationsAPI interface {
	GetLineQuota(context.Context, time.Time) (operations.LineQuotaStatus, error)
	ListReportRuns(context.Context, operations.ReportRunFilter) (operations.ReportRunPage, error)
	ListDeliveries(context.Context, operations.DeliveryFilter) (operations.DeliveryPage, error)
	ListAudit(context.Context, operations.AuditFilter) (operations.AuditPage, error)
}

func registerOperationsRoutes(router interface {
	Get(string, http.HandlerFunc)
}, adminAuth AdminAuthenticator, operationsAPI OperationsAPI) {
	router.Get("/api/v1/admin/line-quota", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		status, err := operationsAPI.GetLineQuota(request.Context(), time.Now().UTC())
		if handleOperationsError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, status)
	})

	router.Get("/api/v1/admin/report-runs", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		pageSize, tenantID, ok := parseOperationsFilter(response, request)
		if !ok {
			return
		}
		var status *report.RunStatus
		if raw := request.URL.Query().Get("status"); raw != "" {
			value := report.RunStatus(raw)
			switch value {
			case report.StatusQueued, report.StatusClaimed, report.StatusRunning, report.StatusSucceeded, report.StatusFailed, report.StatusCancelled, report.StatusExpired:
				status = &value
			default:
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report run status is invalid.", false)
				return
			}
		}
		page, err := operationsAPI.ListReportRuns(request.Context(), operations.ReportRunFilter{
			TenantID: tenantID, Status: status, Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize, Now: time.Now().UTC(),
		})
		if handleOperationsError(response, request, err) {
			return
		}
		data := make([]map[string]any, 0, len(page.Data))
		for _, item := range page.Data {
			responseItem := reportRunResponse(item.Run)
			responseItem["tenantName"] = item.TenantName
			responseItem["runtimeStatus"] = item.RuntimeStatus
			responseItem["retryAvailableAt"] = nullableTime(item.RetryAvailableAt)
			if item.WaitReason != nil {
				responseItem["waitReason"] = *item.WaitReason
			}
			data = append(data, responseItem)
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": data, "page": operationsPage(page.NextCursor, page.HasMore)})
	})

	router.Get("/api/v1/admin/line-deliveries", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		pageSize, tenantID, ok := parseOperationsFilter(response, request)
		if !ok {
			return
		}
		page, err := operationsAPI.ListDeliveries(request.Context(), operations.DeliveryFilter{
			TenantID: tenantID, Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize,
		})
		if handleOperationsError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": page.Data, "page": operationsPage(page.NextCursor, page.HasMore)})
	})

	router.Get("/api/v1/admin/audit-logs", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		pageSize, tenantID, ok := parseOperationsFilter(response, request)
		if !ok {
			return
		}
		page, err := operationsAPI.ListAudit(request.Context(), operations.AuditFilter{
			TenantID: tenantID, Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize,
		})
		if handleOperationsError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": page.Data, "page": operationsPage(page.NextCursor, page.HasMore)})
	})
}

func parseOperationsFilter(response http.ResponseWriter, request *http.Request) (int, *uuid.UUID, bool) {
	pageSize := 25
	if raw := request.URL.Query().Get("pageSize"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Page size must be between 1 and 100.", false)
			return 0, nil, false
		}
		pageSize = value
	}
	var tenantID *uuid.UUID
	if raw := request.URL.Query().Get("tenantId"); raw != "" {
		value, err := uuid.Parse(raw)
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Tenant filter must be a UUID.", false)
			return 0, nil, false
		}
		tenantID = &value
	}
	return pageSize, tenantID, true
}

func operationsPage(next string, hasMore bool) map[string]any {
	var nextCursor any
	if next != "" {
		nextCursor = next
	}
	return map[string]any{"nextCursor": nextCursor, "hasMore": hasMore}
}

func handleOperationsError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, operations.ErrInvalidCursor) {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Cursor is invalid.", false)
	} else {
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to load operations history.", false)
	}
	return true
}
