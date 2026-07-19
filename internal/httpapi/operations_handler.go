package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type OperationsAPI interface {
	GetLineQuota(context.Context, time.Time) (operations.LineQuotaStatus, error)
	ListReportRuns(context.Context, operations.ReportRunFilter) (operations.ReportRunPage, error)
	GetReportRunDetail(context.Context, uuid.UUID, time.Time) (operations.ReportRunDetail, error)
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
		var reportKey *report.Key
		if raw := request.URL.Query().Get("reportKey"); raw != "" {
			value := report.Key(raw)
			if _, exists := report.DefinitionFor(value); !exists {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report filter is invalid.", false)
				return
			}
			reportKey = &value
		}
		var source *report.Source
		if raw := request.URL.Query().Get("source"); raw != "" {
			value := report.Source(raw)
			switch value {
			case report.SourceDashboard, report.SourceSchedule, report.SourceBackground:
				source = &value
			default:
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report source is invalid.", false)
				return
			}
		}
		createdFrom, createdTo, ok := parseOperationsDateRange(response, request)
		if !ok {
			return
		}
		page, err := operationsAPI.ListReportRuns(request.Context(), operations.ReportRunFilter{
			TenantID: tenantID, Status: status, ReportKey: reportKey, Source: source,
			CreatedFrom: createdFrom, CreatedTo: createdTo, Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize, Now: time.Now().UTC(),
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
			if item.FailureSummary != nil {
				responseItem["failureSummary"] = item.FailureSummary
			}
			data = append(data, responseItem)
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": data, "page": operationsPage(page.NextCursor, page.HasMore)})
	})

	router.Get("/api/v1/admin/report-runs/{runId}", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		runID, err := uuid.Parse(request.PathValue("runId"))
		if err != nil {
			writeProblem(response, request, http.StatusNotFound, "REPORT_RUN_NOT_FOUND", "Report run was not found.", false)
			return
		}
		detail, err := operationsAPI.GetReportRunDetail(request.Context(), runID, time.Now().UTC())
		if handleOperationsError(response, request, err) {
			return
		}
		responseItem := reportRunResponse(detail.Run)
		responseItem["tenantName"] = detail.TenantName
		if detail.FailureSummary != nil {
			responseItem["failureSummary"] = detail.FailureSummary
		}
		responseItem["impact"] = detail.Impact
		responseItem["triggerKind"] = detail.TriggerKind
		responseItem["connectionChangedSinceFailure"] = detail.ConnectionChangedSinceFailure
		writeJSON(response, http.StatusOK, responseItem)
	})

	router.Get("/api/v1/admin/line-deliveries", func(response http.ResponseWriter, request *http.Request) {
		if _, ok := operationalAdmin(response, request, adminAuth, false); !ok {
			return
		}
		pageSize, tenantID, ok := parseOperationsFilter(response, request)
		if !ok {
			return
		}
		var status *operations.DeliveryStatus
		if raw := request.URL.Query().Get("status"); raw != "" {
			value := operations.DeliveryStatus(raw)
			switch value {
			case "PENDING", "SENDING", "ACCEPTED", "RETRY_WAIT", "UNCERTAIN", "FAILED_PERMANENT":
				status = &value
			default:
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Delivery status is invalid.", false)
				return
			}
		}
		var recipientID *uuid.UUID
		if raw := request.URL.Query().Get("recipientId"); raw != "" {
			value, err := uuid.Parse(raw)
			if err != nil {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Recipient filter must be a UUID.", false)
				return
			}
			recipientID = &value
		}
		createdFrom, createdTo, ok := parseOperationsDateRange(response, request)
		if !ok {
			return
		}
		page, err := operationsAPI.ListDeliveries(request.Context(), operations.DeliveryFilter{
			TenantID: tenantID, Status: status, RecipientID: recipientID, CreatedFrom: createdFrom, CreatedTo: createdTo,
			Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize,
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
		actorType, ok := parseOperationsEnum(response, request, "actorType", []string{"ADMIN", "VIEWER", "WORKER", "SYSTEM"})
		if !ok {
			return
		}
		result, ok := parseOperationsEnum(response, request, "result", []string{"SUCCESS", "DENIED", "FAILED"})
		if !ok {
			return
		}
		var action *string
		if raw := request.URL.Query().Get("action"); raw != "" {
			if len(raw) > 100 || !operationsActionPattern.MatchString(raw) {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Audit action is invalid.", false)
				return
			}
			action = &raw
		}
		createdFrom, createdTo, ok := parseOperationsDateRange(response, request)
		if !ok {
			return
		}
		page, err := operationsAPI.ListAudit(request.Context(), operations.AuditFilter{
			TenantID: tenantID, ActorType: actorType, Action: action, Result: result,
			CreatedFrom: createdFrom, CreatedTo: createdTo, Cursor: request.URL.Query().Get("cursor"), PageSize: pageSize,
		})
		if handleOperationsError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, map[string]any{"data": page.Data, "page": operationsPage(page.NextCursor, page.HasMore)})
	})
}

var operationsActionPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func parseOperationsEnum(response http.ResponseWriter, request *http.Request, name string, allowed []string) (*string, bool) {
	raw := request.URL.Query().Get(name)
	if raw == "" {
		return nil, true
	}
	for _, value := range allowed {
		if raw == value {
			return &raw, true
		}
	}
	writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Operations filter is invalid.", false)
	return nil, false
}

func parseOperationsDateRange(response http.ResponseWriter, request *http.Request) (*time.Time, *time.Time, bool) {
	location, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to validate date filter.", false)
		return nil, nil, false
	}
	parse := func(name string) (*time.Time, bool) {
		raw := request.URL.Query().Get(name)
		if raw == "" {
			return nil, true
		}
		value, err := time.ParseInLocation("2006-01-02", raw, location)
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Date filter must use YYYY-MM-DD.", false)
			return nil, false
		}
		value = value.UTC()
		return &value, true
	}
	from, ok := parse("dateFrom")
	if !ok {
		return nil, nil, false
	}
	toStart, ok := parse("dateTo")
	if !ok {
		return nil, nil, false
	}
	var to *time.Time
	if toStart != nil {
		value := toStart.In(location).AddDate(0, 0, 1).UTC()
		to = &value
	}
	if from != nil && to != nil && !from.Before(*to) {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Date range is invalid.", false)
		return nil, nil, false
	}
	return from, to, true
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
	} else if errors.Is(err, report.ErrRunNotFound) {
		writeProblem(response, request, http.StatusNotFound, "REPORT_RUN_NOT_FOUND", "Report run was not found.", false)
	} else {
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to load operations history.", false)
	}
	return true
}
