package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func registerViewerReportRoutes(router chi.Router, viewerAuth ViewerAPI, viewerReports ViewerReportAPI) {
	router.Get("/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/snapshots/latest", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, reportKey, ok := parseViewerReportPath(response, request)
		if !ok {
			return
		}
		input, ok := parseViewerPeriodQuery(response, request)
		if !ok {
			return
		}
		snapshot, err := viewerReports.ExactSnapshot(request.Context(), authenticated.RecipientID, tenantID, reportKey, input)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, snapshot)
	})

	router.Post("/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/revalidations", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewerMutation(response, request, viewerAuth)
		if !ok || !isJSONRequest(request) {
			if ok {
				writeProblem(response, request, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.", false)
			}
			return
		}
		tenantID, reportKey, ok := parseViewerReportPath(response, request)
		if !ok {
			return
		}
		input, ok := decodeViewerPeriodBody(response, request)
		if !ok {
			return
		}
		result, err := viewerReports.Revalidate(request.Context(), authenticated.RecipientID, tenantID, reportKey, input)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, reportRevalidationResponse(result))
	})

	router.Post("/api/v1/viewer/tenants/{tenantId}/executive-overview/revalidations", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewerMutation(response, request, viewerAuth)
		if !ok || !isJSONRequest(request) {
			if ok {
				writeProblem(response, request, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.", false)
			}
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		var body struct {
			PeriodPreset report.Preset `json:"periodPreset"`
			DateFrom     *string       `json:"dateFrom"`
			DateTo       *string       `json:"dateTo"`
			ReportKeys   []report.Key  `json:"reportKeys"`
		}
		if err := decodeJSON(response, request, &body); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Dashboard revalidation input is invalid.", false)
			return
		}
		result, err := viewerReports.RevalidateOverview(request.Context(), authenticated.RecipientID, tenantID, viewer.DashboardRefreshInput{
			PeriodPreset: body.PeriodPreset, DateFrom: body.DateFrom, DateTo: body.DateTo, ReportKeys: body.ReportKeys,
		})
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, overviewRevalidationResponse(result))
	})

	router.Get("/api/v1/viewer/tenants/{tenantId}/executive-overview", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		var overview viewer.ExecutiveOverview
		var err error
		query := request.URL.Query()
		hasExactPeriodQuery := query.Has("periodPreset") || query.Has("dateFrom") || query.Has("dateTo") || query.Has("reportKey")
		if hasExactPeriodQuery {
			input, valid := parseViewerOverviewPeriodQuery(response, request)
			if !valid {
				return
			}
			overview, err = viewerReports.ExactOverview(request.Context(), authenticated.RecipientID, tenantID, input)
		} else {
			overview, err = viewerReports.ExecutiveOverview(request.Context(), authenticated.RecipientID, tenantID)
		}
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, overview)
	})

	router.Post("/api/v1/viewer/tenants/{tenantId}/executive-overview/refreshes", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewerMutation(response, request, viewerAuth)
		if !ok || !validIdempotencyHeader(response, request) {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		var input *viewer.DashboardRefreshInput
		if request.ContentLength != 0 {
			if !isJSONRequest(request) {
				writeProblem(response, request, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.", false)
				return
			}
			var body struct {
				PeriodPreset report.Preset `json:"periodPreset"`
				DateFrom     *string       `json:"dateFrom"`
				DateTo       *string       `json:"dateTo"`
				ReportKeys   []report.Key  `json:"reportKeys"`
			}
			if err := decodeJSON(response, request, &body); err != nil {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Dashboard refresh input is invalid.", false)
				return
			}
			input = &viewer.DashboardRefreshInput{PeriodPreset: body.PeriodPreset, DateFrom: body.DateFrom, DateTo: body.DateTo, ReportKeys: body.ReportKeys}
		}
		refresh, err := viewerReports.CreateDashboardRefresh(request.Context(), authenticated.RecipientID, tenantID, request.Header.Get("Idempotency-Key"), input)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusAccepted, refresh)
	})

	router.Get("/api/v1/viewer/tenants/{tenantId}/executive-overview/refreshes/{refreshId}/result", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		refreshID, err := uuid.Parse(chi.URLParam(request, "refreshId"))
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Dashboard refresh ID must be a UUID.", false)
			return
		}
		result, err := viewerReports.GetDashboardRefreshResult(request.Context(), authenticated.RecipientID, tenantID, refreshID)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, result)
	})

	router.Get("/api/v1/viewer/tenants/{tenantId}/executive-overview/refreshes/{refreshId}", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, ok := parseTenantID(response, request)
		if !ok {
			return
		}
		refreshID, err := uuid.Parse(chi.URLParam(request, "refreshId"))
		if err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Dashboard refresh ID must be a UUID.", false)
			return
		}
		refresh, err := viewerReports.GetDashboardRefresh(request.Context(), authenticated.RecipientID, tenantID, refreshID)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, refresh)
	})

	router.Post("/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewerMutation(response, request, viewerAuth)
		if !ok || !validIdempotencyHeader(response, request) {
			return
		}
		if !isJSONRequest(request) {
			writeProblem(response, request, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Content-Type must be application/json.", false)
			return
		}
		tenantID, reportKey, ok := parseViewerReportPath(response, request)
		if !ok {
			return
		}
		var input struct {
			PeriodPreset report.Preset `json:"periodPreset"`
			DateFrom     *string       `json:"dateFrom"`
			DateTo       *string       `json:"dateTo"`
		}
		if err := decodeJSON(response, request, &input); err != nil {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report period input is invalid.", false)
			return
		}
		run, err := viewerReports.Create(request.Context(), authenticated.RecipientID, tenantID, reportKey, request.Header.Get("Idempotency-Key"), viewer.CreateReportRunInput{
			PeriodPreset: input.PeriodPreset, DateFrom: input.DateFrom, DateTo: input.DateTo,
		})
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusAccepted, reportRunResponse(run))
	})

	router.Get("/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs/{runId}", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, reportKey, runID, ok := parseViewerRunPath(response, request)
		if !ok {
			return
		}
		run, err := viewerReports.Get(request.Context(), authenticated.RecipientID, tenantID, reportKey, runID)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, reportRunResponse(run))
	})

	router.Get("/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs/{runId}/dashboard", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, reportKey, runID, ok := parseViewerRunPath(response, request)
		if !ok {
			return
		}
		dashboard, err := viewerReports.GetDashboard(request.Context(), authenticated.RecipientID, tenantID, reportKey, runID)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, dashboard)
	})

	router.Get("/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs/{runId}/rows", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewer(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, reportKey, runID, ok := parseViewerRunPath(response, request)
		if !ok {
			return
		}
		pageSize := 25
		if raw := request.URL.Query().Get("pageSize"); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil || value < 1 || value > 100 {
				writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report page size must be between 1 and 100.", false)
				return
			}
			pageSize = value
		}
		page, err := viewerReports.ListRows(request.Context(), authenticated.RecipientID, tenantID, reportKey, runID, request.URL.Query().Get("cursor"), pageSize)
		if handleViewerReportError(response, request, err) {
			return
		}
		var nextCursor any
		if page.NextCursor != "" {
			nextCursor = page.NextCursor
		}
		writeJSON(response, http.StatusOK, map[string]any{
			"runId": page.RunID, "columns": page.Columns, "data": page.Rows,
			"page": map[string]any{"nextCursor": nextCursor, "hasMore": page.HasMore},
		})
	})

	router.Post("/api/v1/viewer/tenants/{tenantId}/reports/{reportKey}/runs/{runId}/cancel", func(response http.ResponseWriter, request *http.Request) {
		authenticated, ok := authenticateViewerMutation(response, request, viewerAuth)
		if !ok {
			return
		}
		tenantID, reportKey, runID, ok := parseViewerRunPath(response, request)
		if !ok {
			return
		}
		run, err := viewerReports.Cancel(request.Context(), authenticated.RecipientID, tenantID, reportKey, runID)
		if handleViewerReportError(response, request, err) {
			return
		}
		writeJSON(response, http.StatusOK, reportRunResponse(run))
	})
}

func decodeViewerPeriodBody(response http.ResponseWriter, request *http.Request) (viewer.CreateReportRunInput, bool) {
	var body struct {
		PeriodPreset report.Preset `json:"periodPreset"`
		DateFrom     *string       `json:"dateFrom"`
		DateTo       *string       `json:"dateTo"`
	}
	if err := decodeJSON(response, request, &body); err != nil {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report period input is invalid.", false)
		return viewer.CreateReportRunInput{}, false
	}
	return viewer.CreateReportRunInput{PeriodPreset: body.PeriodPreset, DateFrom: body.DateFrom, DateTo: body.DateTo}, true
}

func parseViewerPeriodQuery(response http.ResponseWriter, request *http.Request) (viewer.CreateReportRunInput, bool) {
	input := viewer.CreateReportRunInput{PeriodPreset: report.Preset(request.URL.Query().Get("periodPreset"))}
	if value := request.URL.Query().Get("dateFrom"); value != "" {
		input.DateFrom = &value
	}
	if value := request.URL.Query().Get("dateTo"); value != "" {
		input.DateTo = &value
	}
	if input.PeriodPreset == "" {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "periodPreset is required.", false)
		return viewer.CreateReportRunInput{}, false
	}
	return input, true
}

func parseViewerOverviewPeriodQuery(response http.ResponseWriter, request *http.Request) (viewer.DashboardRefreshInput, bool) {
	query := request.URL.Query()
	input := viewer.DashboardRefreshInput{
		PeriodPreset: report.Preset(query.Get("periodPreset")),
		ReportKeys:   make([]report.Key, 0, len(query["reportKey"])),
	}
	for _, value := range query["reportKey"] {
		if value == "" {
			writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "reportKey is invalid.", false)
			return viewer.DashboardRefreshInput{}, false
		}
		input.ReportKeys = append(input.ReportKeys, report.Key(value))
	}
	if value := query.Get("dateFrom"); value != "" {
		input.DateFrom = &value
	}
	if value := query.Get("dateTo"); value != "" {
		input.DateTo = &value
	}
	if input.PeriodPreset == "" || len(input.ReportKeys) == 0 {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "periodPreset and reportKey are required.", false)
		return viewer.DashboardRefreshInput{}, false
	}
	return input, true
}

func reportRevalidationResponse(result viewer.ReportRevalidation) map[string]any {
	response := map[string]any{"disposition": result.Disposition, "retryAfter": result.RetryAfter, "legacyFallback": result.LegacyFallback}
	if result.Snapshot != nil {
		response["snapshot"] = result.Snapshot
	}
	if result.Run != nil {
		response["run"] = reportRunResponse(*result.Run)
	}
	return response
}

func overviewRevalidationResponse(result viewer.OverviewRevalidation) map[string]any {
	overview := result.Overview
	if overview.Items == nil {
		overview.Items = make([]viewer.DashboardSnapshot, 0)
	}
	runs := make([]map[string]any, 0, len(result.Runs))
	for _, run := range result.Runs {
		runs = append(runs, reportRunResponse(run))
	}
	return map[string]any{
		"disposition": result.Disposition, "overview": overview,
		"runs": runs, "retryAfter": result.RetryAfter, "legacyFallback": result.LegacyFallback,
	}
}

func authenticateViewerMutation(response http.ResponseWriter, request *http.Request, viewerAuth ViewerAPI) (viewer.AuthenticatedViewer, bool) {
	authenticated, ok := authenticateViewer(response, request, viewerAuth)
	if !ok || !authorizeViewerCSRF(response, request, viewerAuth, authenticated) {
		return viewer.AuthenticatedViewer{}, false
	}
	return authenticated, true
}

func parseViewerReportPath(response http.ResponseWriter, request *http.Request) (uuid.UUID, report.Key, bool) {
	tenantID, ok := parseTenantID(response, request)
	if !ok {
		return uuid.Nil, "", false
	}
	reportKey := report.Key(chi.URLParam(request, "reportKey"))
	if _, ok := report.DefinitionFor(reportKey); !ok {
		writeProblem(response, request, http.StatusNotFound, "REPORT_NOT_FOUND", "Report was not found.", false)
		return uuid.Nil, "", false
	}
	return tenantID, reportKey, true
}

func parseViewerRunPath(response http.ResponseWriter, request *http.Request) (uuid.UUID, report.Key, uuid.UUID, bool) {
	tenantID, reportKey, ok := parseViewerReportPath(response, request)
	if !ok {
		return uuid.Nil, "", uuid.Nil, false
	}
	runID, err := uuid.Parse(chi.URLParam(request, "runId"))
	if err != nil {
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report run ID must be a UUID.", false)
		return uuid.Nil, "", uuid.Nil, false
	}
	return tenantID, reportKey, runID, true
}

func handleViewerReportError(response http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, viewer.ErrReportForbidden), errors.Is(err, report.ErrRunForbidden):
		writeProblem(response, request, http.StatusForbidden, "REPORT_ACCESS_FORBIDDEN", "This report is not available to the verified LINE identity.", false)
	case errors.Is(err, viewer.ErrReportInputInvalid):
		writeProblem(response, request, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Report request input is invalid.", false)
	case errors.Is(err, viewer.ErrViewerContextChanged):
		writeProblem(response, request, http.StatusConflict, "VIEWER_CONTEXT_CHANGED", "Viewer store or report access changed before the refresh started.", false)
	case errors.Is(err, viewer.ErrDashboardRefreshNotReady):
		response.Header().Set("Retry-After", "2")
		writeProblem(response, request, http.StatusConflict, "DASHBOARD_REFRESH_NOT_READY", "Dashboard refresh result is not ready yet.", true)
	case errors.Is(err, report.ErrRunIdempotencyConflict):
		writeProblem(response, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "Idempotency key was already used with different report input.", false)
	case errors.Is(err, report.ErrRunConcurrencyLimit):
		response.Header().Set("Retry-After", "5")
		writeProblem(response, request, http.StatusTooManyRequests, "REPORT_CONCURRENCY_LIMIT", "This store already has the maximum number of active report runs.", true)
	case errors.Is(err, report.ErrRunCircuitOpen):
		response.Header().Set("Retry-After", "60")
		writeProblem(response, request, http.StatusTooManyRequests, "SML_CIRCUIT_OPEN", "This store is temporarily protected after repeated SML failures. Try again later.", true)
	case errors.Is(err, report.ErrRunRowsExpired):
		writeProblem(response, request, http.StatusGone, "REPORT_ROWS_EXPIRED", "Temporary report rows have expired. Run the report again.", false)
	case errors.Is(err, report.ErrRunNotCancellable):
		writeProblem(response, request, http.StatusConflict, "REPORT_NOT_CANCELLABLE", "This report run has already finished and cannot be cancelled.", false)
	case errors.Is(err, report.ErrRunNotFound):
		writeProblem(response, request, http.StatusNotFound, "REPORT_RUN_NOT_FOUND", "Report run was not found.", false)
	default:
		writeProblem(response, request, http.StatusInternalServerError, "INTERNAL_ERROR", "Unable to process the report run.", false)
	}
	return true
}

func reportRunResponse(run report.Run) map[string]any {
	summary := run.Summary
	if summary == nil {
		summary = map[string]string{}
	}
	reconciliation := run.Reconciliation
	if reconciliation == nil {
		reconciliation = map[string]any{}
	}
	result := map[string]any{
		"id": run.ID, "tenantId": run.TenantID, "reportKey": run.ReportKey, "status": run.Status,
		"periodPreset": run.Period.Preset, "rowCount": run.RowCount, "isTruncated": run.IsTruncated,
		"summary": summary, "reconciliation": reconciliation,
		"queuedAt": run.QueuedAt, "expiresAt": run.ExpiresAt,
	}
	result["resultKind"] = run.ResultKind
	progressEnd := time.Now().UTC()
	if run.FinishedAt != nil {
		progressEnd = *run.FinishedAt
	}
	elapsedMS := max(int64(0), progressEnd.Sub(run.QueuedAt).Milliseconds())
	result["progress"] = map[string]any{
		"phase": run.ProgressPhase, "phaseSequence": run.ProgressSequence,
		"completedSteps": run.ProgressCompletedSteps, "totalSteps": run.ProgressTotalSteps,
		"attempt": run.Attempt, "updatedAt": nullableTime(run.ProgressUpdatedAt),
		"expectedP50Ms": nullablePositive(run.ExpectedP50MS), "expectedP90Ms": nullablePositive(run.ExpectedP90MS),
		"sampleCount": run.ExpectedSampleCount,
		"elapsedMs":   elapsedMS,
	}
	result["dateFrom"] = nullableString(run.Period.DateFrom)
	result["dateTo"] = nullableString(run.Period.DateTo)
	result["safeErrorCode"] = nullableString(run.SafeErrorCode)
	result["safeErrorMessage"] = nullableString(run.SafeErrorMessage)
	if run.QueuePosition > 0 {
		result["queuePosition"] = run.QueuePosition
	}
	result["startedAt"] = nullableTime(run.StartedAt)
	result["finishedAt"] = nullableTime(run.FinishedAt)
	return result
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}
