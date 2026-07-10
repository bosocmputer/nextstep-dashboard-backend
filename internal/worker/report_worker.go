package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
)

const (
	reportLeaseDuration     = 3 * time.Minute
	reportHeartbeatInterval = time.Minute
	maximumReportAttempts   = 3
)

type ReportRunStore interface {
	Claim(context.Context, string, time.Duration, time.Time) (report.Run, error)
	MarkRunning(context.Context, uuid.UUID, string, time.Duration, time.Time) error
	ExtendLease(context.Context, uuid.UUID, string, time.Duration, time.Time) error
	Complete(context.Context, uuid.UUID, string, report.SummaryResult, bool, time.Time) error
	Retry(context.Context, uuid.UUID, string, string, time.Time, time.Time) error
	Fail(context.Context, uuid.UUID, string, string, string, time.Time) error
}

type ConnectionProvider interface {
	Open(context.Context, uuid.UUID) (sml.Connection, error)
}

type ReportQueryClient interface {
	Query(context.Context, sml.Connection, string) ([]map[string]string, error)
}

type ReportWorker struct {
	store       ReportRunStore
	connections ConnectionProvider
	client      ReportQueryClient
	workerID    string
	now         func() time.Time
}

type executionFailure struct {
	Code      string
	Retryable bool
}

func NewReportWorker(store ReportRunStore, connections ConnectionProvider, client ReportQueryClient, workerID string, now func() time.Time) *ReportWorker {
	return &ReportWorker{store: store, connections: connections, client: client, workerID: workerID, now: now}
}

func (worker *ReportWorker) ProcessOne(ctx context.Context) error {
	now := worker.now().UTC()
	run, err := worker.store.Claim(ctx, worker.workerID, reportLeaseDuration, now)
	if err != nil {
		return err
	}
	if err := worker.store.MarkRunning(ctx, run.ID, worker.workerID, reportLeaseDuration, worker.now().UTC()); err != nil {
		return err
	}

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go worker.keepLease(heartbeatCtx, run.ID)

	summary, failure := worker.execute(ctx, run)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failure != nil {
		now = worker.now().UTC()
		if failure.Retryable && run.Attempt < maximumReportAttempts {
			backoff := 30 * time.Second * time.Duration(1<<(run.Attempt-1))
			return worker.store.Retry(ctx, run.ID, worker.workerID, failure.Code, now.Add(backoff), now)
		}
		return worker.store.Fail(ctx, run.ID, worker.workerID, failure.Code, safeFailureMessage(failure.Code), now)
	}
	return worker.store.Complete(ctx, run.ID, worker.workerID, summary, run.Source == report.SourceDashboard, worker.now().UTC())
}

func (worker *ReportWorker) execute(ctx context.Context, run report.Run) (report.SummaryResult, *executionFailure) {
	definition, ok := report.DefinitionFor(run.ReportKey)
	if !ok {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
	}
	connection, err := worker.connections.Open(ctx, run.TenantID)
	if err != nil {
		var connectionError *sml.ConnectionTestError
		if errors.As(err, &connectionError) {
			return report.SummaryResult{}, &executionFailure{Code: connectionError.SafeCode, Retryable: connectionError.Retryable}
		}
		if errors.Is(err, sml.ErrConnectionNotConfigured) {
			return report.SummaryResult{}, &executionFailure{Code: "SML_NOT_CONFIGURED"}
		}
		return report.SummaryResult{}, &executionFailure{Code: "SML_CONNECTION_LOAD_FAILED", Retryable: true}
	}
	plan, err := report.BuildQueryPlan(run.ReportKey, run.Period)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
	}
	stepRows, failure := worker.executePlan(ctx, run, definition, connection, plan)
	if failure != nil {
		return report.SummaryResult{}, failure
	}
	summary, err := report.Summarize(run.ReportKey, stepRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID"}
	}

	comparisonPeriod, err := report.ResolveComparisonPeriod(run.Period)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
	}
	comparisonPlan, err := report.BuildQueryPlan(run.ReportKey, comparisonPeriod)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
	}
	comparisonRows, comparisonFailure := worker.executePlan(ctx, run, definition, connection, comparisonPlan)
	if comparisonFailure != nil {
		comparisonRows = emptyReportSteps(run.ReportKey)
	}
	dashboard, err := report.BuildDashboard(run.ReportKey, run.Period, comparisonPeriod, stepRows, comparisonRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID"}
	}
	if comparisonFailure != nil {
		dashboard.Quality.Status = "WARNING"
		dashboard.Quality.Warnings = append(dashboard.Quality.Warnings, "COMPARISON_QUERY_FAILED")
		for index := range dashboard.KPIs {
			dashboard.KPIs[index].Comparison = report.MetricComparison{Availability: report.ComparisonUnavailable}
		}
	}
	summary.Dashboard = &dashboard
	return summary, nil
}

func (worker *ReportWorker) executePlan(ctx context.Context, run report.Run, definition report.Definition, connection sml.Connection, plan report.QueryPlan) (map[string][]map[string]string, *executionFailure) {
	stepRows := make(map[string][]map[string]string, len(plan.Steps))
	totalRows := 0
	for _, step := range plan.Steps {
		rendered, err := report.RenderSQL(step.Query)
		if err != nil {
			return nil, &executionFailure{Code: "REPORT_QUERY_RENDER_FAILED"}
		}
		queryTimeout := definition.DetailTimeout
		if run.Source == report.SourceSchedule {
			queryTimeout = definition.SummaryTimeout
		}
		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		rows, queryErr := worker.client.Query(queryCtx, connection, rendered)
		cancel()
		if queryErr != nil {
			var safeError *sml.SafeError
			if errors.As(queryErr, &safeError) {
				return nil, &executionFailure{Code: safeError.Code, Retryable: safeError.Retryable}
			}
			if errors.Is(queryErr, context.DeadlineExceeded) {
				return nil, &executionFailure{Code: "SML_TIMEOUT", Retryable: true}
			}
			return nil, &executionFailure{Code: "SML_QUERY_FAILED", Retryable: true}
		}
		totalRows += len(rows)
		if totalRows > definition.MaxRows {
			return nil, &executionFailure{Code: "REPORT_ROW_LIMIT_EXCEEDED"}
		}
		stepRows[step.Name] = rows
	}
	return stepRows, nil
}

func emptyReportSteps(key report.Key) map[string][]map[string]string {
	if key == report.SalesGoodsServices || key == report.PurchaseGoodsPayables {
		return map[string][]map[string]string{"headers": {}, "details": {}}
	}
	return map[string][]map[string]string{"rows": {}}
}

func (worker *ReportWorker) keepLease(ctx context.Context, runID uuid.UUID) {
	ticker := time.NewTicker(reportHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = worker.store.ExtendLease(ctx, runID, worker.workerID, reportLeaseDuration, worker.now().UTC())
		}
	}
}

func safeFailureMessage(code string) string {
	return fmt.Sprintf("Report run failed safely (%s).", code)
}
