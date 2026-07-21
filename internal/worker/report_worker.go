package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
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
	UpdateProgress(context.Context, uuid.UUID, string, report.ProgressPhase, int, int, time.Time) error
	ExtendLease(context.Context, uuid.UUID, string, time.Duration, time.Time) error
	Complete(context.Context, uuid.UUID, string, report.SummaryResult, bool, time.Time) error
	Retry(context.Context, uuid.UUID, string, string, time.Time, time.Time) error
	RetryPreRequestFailure(context.Context, uuid.UUID, string, string, time.Time, time.Time) error
	Fail(context.Context, uuid.UUID, string, failure.Evidence, time.Time) error
	FailPreRequestFailure(context.Context, uuid.UUID, string, failure.Evidence, time.Time) error
	FailRemoteUnknown(context.Context, uuid.UUID, string, failure.Evidence, time.Time, time.Time) error
}

type HeavyChunkStore interface {
	PrepareChunks(context.Context, uuid.UUID, string, []report.ChunkManifest, time.Time) error
	StartChunk(context.Context, uuid.UUID, string, int, time.Time) error
	CompleteChunk(context.Context, uuid.UUID, string, int, any, int, time.Time) error
	FailChunk(context.Context, uuid.UUID, string, int, string, time.Time) error
}

type SourceConsistencyStore interface {
	SetSourceConsistency(context.Context, uuid.UUID, string, report.SourceConsistency, time.Time) error
}

type QueryConcurrencyEvidenceStore interface {
	QueryConcurrencyEvidence(context.Context, uuid.UUID, time.Time) (int, int, error)
}

type ConnectionProvider interface {
	Open(context.Context, uuid.UUID) (sml.Connection, error)
}

type ReportQueryClient interface {
	Query(context.Context, sml.Connection, string) ([]map[string]string, error)
}

type ReportWorker struct {
	store                 ReportRunStore
	connections           ConnectionProvider
	client                ReportQueryClient
	workerID              string
	now                   func() time.Time
	summaryQueriesEnabled bool
	heavyChunkEnabled     bool
	scheduleChunkEnabled  bool
	heavyChunkTargets     map[string]struct{}
	heartbeatInterval     time.Duration
}

type executionFailure struct {
	Code               string
	Stage              failure.Stage
	TransportPhase     failure.TransportPhase
	Retryable          bool
	RemoteStateUnknown bool
	PreRequestFailure  bool
	ProtocolEvidence   *failure.JavaWSProtocolEvidence
}

func NewReportWorker(store ReportRunStore, connections ConnectionProvider, client ReportQueryClient, workerID string, now func() time.Time) *ReportWorker {
	return &ReportWorker{
		store: store, connections: connections, client: client, workerID: workerID, now: now,
		summaryQueriesEnabled: true, heartbeatInterval: reportHeartbeatInterval,
	}
}

func (worker *ReportWorker) ConfigureSummaryQueries(enabled bool) *ReportWorker {
	worker.summaryQueriesEnabled = enabled
	return worker
}

func (worker *ReportWorker) ConfigureHeavyChunks(enabled, scheduleEnabled bool, targets []string) *ReportWorker {
	worker.heavyChunkEnabled = enabled
	worker.scheduleChunkEnabled = scheduleEnabled
	worker.heavyChunkTargets = make(map[string]struct{}, len(targets))
	for _, target := range targets {
		worker.heavyChunkTargets[target] = struct{}{}
	}
	return worker
}

func (worker *ReportWorker) ProcessOne(ctx context.Context) error {
	now := worker.now().UTC()
	run, err := worker.store.Claim(ctx, worker.workerID, reportLeaseDuration, now)
	if err != nil {
		return err
	}
	startedAt := worker.now().UTC()
	if err := worker.store.MarkRunning(ctx, run.ID, worker.workerID, reportLeaseDuration, startedAt); err != nil {
		return err
	}
	totalProgressSteps := 5
	if report.ComparisonSupported(run.ReportKey, run.Period) {
		totalProgressSteps = 6
	}
	if err := worker.store.UpdateProgress(ctx, run.ID, worker.workerID, report.ProgressConnecting, 1, totalProgressSteps, worker.now().UTC()); err != nil {
		return err
	}

	executionCtx, stopExecution := context.WithCancel(ctx)
	defer stopExecution()
	leaseErrors := make(chan error, 1)
	go worker.keepLease(executionCtx, run.ID, stopExecution, leaseErrors)

	recorder, evidenceCtx, recorderErr := sml.NewProtocolRecorder(executionCtx)
	var summary report.SummaryResult
	var executionErr *executionFailure
	if recorderErr != nil {
		executionErr = &executionFailure{Code: "SML_REQUEST_INVALID", Stage: failure.StageSendRequest}
	} else {
		if evidenceStore, ok := worker.store.(QueryConcurrencyEvidenceStore); ok {
			if tenantConcurrent, hostConcurrent, evidenceErr := evidenceStore.QueryConcurrencyEvidence(evidenceCtx, run.ID, worker.now().UTC()); evidenceErr == nil {
				recorder.SetConcurrency(tenantConcurrent, hostConcurrent)
			}
		}
		summary, executionErr = worker.execute(evidenceCtx, run)
		if executionErr != nil && executionErr.ProtocolEvidence == nil {
			protocol := recorder.Snapshot()
			// A generated reference or admission count alone is not transport
			// evidence. Attach protocol metadata only after the HTTP request was
			// actually written, including when JavaWS succeeded and a later
			// Nextstep report-build stage failed.
			if protocol.RequestCount > 0 {
				converted := convertProtocolEvidence(protocol)
				executionErr.ProtocolEvidence = &converted
			}
		}
	}
	stopExecution()
	select {
	case leaseErr := <-leaseErrors:
		return fmt.Errorf("extend report lease: %w", leaseErr)
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if executionErr != nil {
		now = worker.now().UTC()
		evidence := buildFailureEvidence(run, *executionErr, startedAt, now)
		if executionErr.RemoteStateUnknown {
			// The remote PostgreSQL query may still be running after JavaWS has
			// timed out. Failing the run and opening the tenant cooldown must be a
			// single transaction so a crash cannot leave either invariant half-set.
			return worker.store.FailRemoteUnknown(ctx, run.ID, worker.workerID, evidence, now, now.Add(10*time.Minute))
		}
		if executionErr.PreRequestFailure {
			if run.Source == report.SourceDashboard && run.Attempt < 2 {
				return worker.store.RetryPreRequestFailure(ctx, run.ID, worker.workerID, executionErr.Code, now.Add(30*time.Second), now)
			}
			return worker.store.FailPreRequestFailure(ctx, run.ID, worker.workerID, evidence, now)
		}
		if executionErr.Retryable && executionErr.Code != "SML_TIMEOUT" && run.Source != report.SourceBackground && run.Attempt < maximumReportAttempts {
			backoff := 30 * time.Second * time.Duration(1<<(run.Attempt-1))
			return worker.store.Retry(ctx, run.ID, worker.workerID, executionErr.Code, now.Add(backoff), now)
		}
		return worker.store.Fail(ctx, run.ID, worker.workerID, evidence, now)
	}
	completedSteps := totalProgressSteps - 1
	if err := worker.store.UpdateProgress(ctx, run.ID, worker.workerID, report.ProgressSavingResult, completedSteps, totalProgressSteps, worker.now().UTC()); err != nil {
		return err
	}
	persistRows := run.Source == report.SourceDashboard && run.ResultKind != report.ResultSummary
	return worker.store.Complete(ctx, run.ID, worker.workerID, summary, persistRows, worker.now().UTC())
}

func (worker *ReportWorker) execute(ctx context.Context, run report.Run) (report.SummaryResult, *executionFailure) {
	definition, ok := report.DefinitionFor(run.ReportKey)
	if !ok {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
	}
	usesSummaryBudget := run.Source == report.SourceSchedule || run.Source == report.SourceBackground || run.ResultKind == report.ResultSummary
	totalTimeout := definition.DetailTotalTimeout
	if usesSummaryBudget {
		totalTimeout = definition.SummaryTotalTimeout
	}
	if totalTimeout <= 0 {
		totalTimeout = definition.DetailTimeout
		if usesSummaryBudget {
			totalTimeout = definition.SummaryTimeout
		}
	}
	executionCtx, cancelExecution := context.WithTimeout(ctx, totalTimeout)
	defer cancelExecution()
	connection, err := worker.connections.Open(executionCtx, run.TenantID)
	if err != nil {
		var connectionError *sml.ConnectionTestError
		if errors.As(err, &connectionError) {
			return report.SummaryResult{}, &executionFailure{Code: connectionError.SafeCode, Stage: failure.StageLoadConnection, Retryable: connectionError.Retryable}
		}
		if errors.Is(err, sml.ErrConnectionNotConfigured) {
			return report.SummaryResult{}, &executionFailure{Code: "SML_NOT_CONFIGURED", Stage: failure.StageLoadConnection}
		}
		return report.SummaryResult{}, &executionFailure{Code: "SML_CONNECTION_LOAD_FAILED", Stage: failure.StageLoadConnection, Retryable: true}
	}
	projection := run.ResultKind
	if projection == "" { // Compatibility for pre-projection runs already queued during rollout.
		projection = report.ResultDetail
	}
	if projection == report.ResultSummary && !worker.summaryQueriesEnabled {
		projection = report.ResultDetail
	}
	if worker.shouldUseChunks(run, definition, projection) {
		return worker.executeChunked(executionCtx, run, definition, connection, projection)
	}
	plan, err := report.BuildQueryPlanForProjection(run.ReportKey, run.Period, projection)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
	}
	consistency := report.ConsistencyStatement
	if len(plan.Steps) > 1 || report.ComparisonSupported(run.ReportKey, run.Period) {
		consistency = report.ConsistencySerialWindow
	}
	if metadataStore, ok := worker.store.(SourceConsistencyStore); ok {
		if err := metadataStore.SetSourceConsistency(executionCtx, run.ID, worker.workerID, consistency, worker.now().UTC()); err != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Stage: failure.StageQueueExecution, Retryable: true}
		}
	}
	totalProgressSteps := 5
	if report.ComparisonSupported(run.ReportKey, run.Period) {
		totalProgressSteps = 6
	}
	if err := worker.store.UpdateProgress(executionCtx, run.ID, worker.workerID, report.ProgressQueryingCurrent, 2, totalProgressSteps, worker.now().UTC()); err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Stage: failure.StageQueueExecution, Retryable: true}
	}
	stepRows, planFailure := worker.executePlan(executionCtx, run, definition, connection, plan)
	if planFailure != nil {
		return report.SummaryResult{}, planFailure
	}
	summary, err := report.Summarize(run.ReportKey, stepRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID", Stage: failure.StageBuildReport}
	}

	comparisonPeriod, err := report.ResolveComparisonPeriod(run.Period)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
	}
	comparisonRows := emptyReportSteps(run.ReportKey)
	comparisonWarning := ""
	if report.ComparisonSupported(run.ReportKey, run.Period) {
		if err := worker.store.UpdateProgress(executionCtx, run.ID, worker.workerID, report.ProgressQueryingComparison, 3, totalProgressSteps, worker.now().UTC()); err != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Stage: failure.StageQueueExecution, Retryable: true}
		}
		comparisonPlan, planErr := report.BuildQueryPlanForProjection(run.ReportKey, comparisonPeriod, projection)
		if planErr != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
		}
		var comparisonFailure *executionFailure
		comparisonRows, comparisonFailure = worker.executePlan(executionCtx, run, definition, connection, comparisonPlan)
		if comparisonFailure != nil {
			if comparisonFailure.RemoteStateUnknown {
				return report.SummaryResult{}, comparisonFailure
			}
			comparisonRows = emptyReportSteps(run.ReportKey)
			comparisonWarning = "COMPARISON_QUERY_FAILED"
		}
	}
	buildingStep := 3
	if totalProgressSteps == 6 {
		buildingStep = 4
	}
	if err := worker.store.UpdateProgress(executionCtx, run.ID, worker.workerID, report.ProgressBuildingDashboard, buildingStep, totalProgressSteps, worker.now().UTC()); err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Stage: failure.StageQueueExecution, Retryable: true}
	}
	dashboard, err := report.BuildDashboard(run.ReportKey, run.Period, comparisonPeriod, stepRows, comparisonRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID", Stage: failure.StageBuildReport}
	}
	if comparisonWarning != "" {
		report.SetComparisonUnavailable(&dashboard, comparisonWarning)
	}
	summary.Dashboard = &dashboard
	return summary, nil
}

func (worker *ReportWorker) shouldUseChunks(run report.Run, definition report.Definition, projection report.ResultKind) bool {
	if !worker.heavyChunkEnabled || !definition.ChunkSafe || len(worker.heavyChunkTargets) == 0 {
		return false
	}
	if run.Source == report.SourceSchedule && !worker.scheduleChunkEnabled {
		return false
	}
	if projection != report.ResultSummary && projection != report.ResultDetail {
		return false
	}
	_, enabled := worker.heavyChunkTargets[run.TenantID.String()+"/"+string(run.ReportKey)]
	return enabled
}

type persistedChunkResult struct {
	Current    map[string][]map[string]string `json:"current"`
	Comparison map[string][]map[string]string `json:"comparison,omitempty"`
}

func (worker *ReportWorker) executeChunked(ctx context.Context, run report.Run, definition report.Definition, connection sml.Connection, projection report.ResultKind) (report.SummaryResult, *executionFailure) {
	chunkStore, ok := worker.store.(HeavyChunkStore)
	if !ok {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_STORE_UNAVAILABLE", Stage: failure.StageQueueExecution}
	}
	chunkCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	totalProgressSteps := 5
	if report.ComparisonSupported(run.ReportKey, run.Period) {
		totalProgressSteps = 6
	}
	if err := worker.store.UpdateProgress(chunkCtx, run.ID, worker.workerID, report.ProgressQueryingCurrent, 2, totalProgressSteps, worker.now().UTC()); err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Stage: failure.StageQueueExecution, Retryable: true}
	}
	manifestQuery, chunkSize, err := report.BuildChunkManifestQuery(run.ReportKey, run.Period)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
	}
	manifestRows, queryFailure := worker.query(chunkCtx, definition.SummaryTimeout, connection, manifestQuery)
	if queryFailure != nil {
		return report.SummaryResult{}, queryFailure
	}
	unitKeys, err := report.ChunkKeys(manifestRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID", Stage: failure.StageBuildReport}
	}
	if chunkSize < report.MinimumChunkSize {
		chunkSize = report.MinimumChunkSize
	}
	manifests := make([]report.ChunkManifest, 0, (len(unitKeys)+chunkSize-1)/chunkSize)
	for start := 0; start < len(unitKeys); start += chunkSize {
		end := start + chunkSize
		if end > len(unitKeys) {
			end = len(unitKeys)
		}
		keys := append([]string(nil), unitKeys[start:end]...)
		manifests = append(manifests, report.ChunkManifest{
			Number: len(manifests) + 1, Key: report.ChunkKey(len(manifests), keys),
			CursorFrom: keys[0], CursorTo: keys[len(keys)-1], UnitKeys: keys,
		})
	}
	if err := chunkStore.PrepareChunks(chunkCtx, run.ID, worker.workerID, manifests, worker.now().UTC()); err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_MANIFEST_PERSIST_FAILED", Stage: failure.StageSaveReport}
	}

	comparisonPeriod, err := report.ResolveComparisonPeriod(run.Period)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
	}
	comparisonSupported := report.ComparisonSupported(run.ReportKey, run.Period)
	currentChunks := make([]map[string][]map[string]string, 0, len(manifests))
	comparisonChunks := make([]map[string][]map[string]string, 0, len(manifests))
	comparisonWarning := ""
	for _, manifest := range manifests {
		if err := chunkStore.StartChunk(chunkCtx, run.ID, worker.workerID, manifest.Number, worker.now().UTC()); err != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_PROGRESS_FAILED", Stage: failure.StageQueueExecution}
		}
		plan, planErr := report.BuildChunkQueryPlan(run.ReportKey, run.Period, projection, manifest.UnitKeys)
		if planErr != nil {
			_ = chunkStore.FailChunk(chunkCtx, run.ID, worker.workerID, manifest.Number, "REPORT_CONTRACT_INVALID", worker.now().UTC())
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
		}
		current, currentFailure := worker.executePlan(chunkCtx, run, definition, connection, plan)
		if currentFailure != nil {
			_ = chunkStore.FailChunk(context.WithoutCancel(ctx), run.ID, worker.workerID, manifest.Number, currentFailure.Code, worker.now().UTC())
			return report.SummaryResult{}, currentFailure
		}
		comparison := map[string][]map[string]string{}
		if comparisonSupported {
			comparisonPlan, planErr := report.BuildChunkQueryPlan(run.ReportKey, comparisonPeriod, projection, manifest.UnitKeys)
			if planErr != nil {
				_ = chunkStore.FailChunk(chunkCtx, run.ID, worker.workerID, manifest.Number, "REPORT_CONTRACT_INVALID", worker.now().UTC())
				return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID", Stage: failure.StageBuildReport}
			}
			comparison, currentFailure = worker.executePlan(chunkCtx, run, definition, connection, comparisonPlan)
			if currentFailure != nil {
				if currentFailure.RemoteStateUnknown {
					_ = chunkStore.FailChunk(context.WithoutCancel(ctx), run.ID, worker.workerID, manifest.Number, currentFailure.Code, worker.now().UTC())
					return report.SummaryResult{}, currentFailure
				}
				// Comparison remains optional, matching the direct runner contract.
				comparison = emptyReportSteps(run.ReportKey)
				comparisonWarning = "COMPARISON_QUERY_FAILED"
			}
		}
		rowCount := 0
		for _, rows := range current {
			rowCount += len(rows)
		}
		payload := persistedChunkResult{Current: current, Comparison: comparison}
		if err := chunkStore.CompleteChunk(chunkCtx, run.ID, worker.workerID, manifest.Number, payload, rowCount, worker.now().UTC()); err != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_PROGRESS_FAILED", Stage: failure.StageQueueExecution}
		}
		currentChunks = append(currentChunks, current)
		if comparisonSupported {
			comparisonChunks = append(comparisonChunks, comparison)
		}
	}
	currentRows, err := report.MergeChunkedSteps(run.ReportKey, projection, currentChunks)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID", Stage: failure.StageBuildReport}
	}
	if len(flattenStepRows(currentRows)) > definition.MaxRows {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_ROW_LIMIT_EXCEEDED", Stage: failure.StageBuildReport}
	}
	comparisonRows := emptyReportSteps(run.ReportKey)
	if comparisonSupported {
		comparisonRows, err = report.MergeChunkedSteps(run.ReportKey, projection, comparisonChunks)
		if err != nil {
			comparisonRows = emptyReportSteps(run.ReportKey)
			comparisonWarning = "COMPARISON_QUERY_FAILED"
		}
	}
	buildingStep := 3
	if comparisonSupported {
		buildingStep = 4
	}
	if err := worker.store.UpdateProgress(chunkCtx, run.ID, worker.workerID, report.ProgressBuildingDashboard, buildingStep, totalProgressSteps, worker.now().UTC()); err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Stage: failure.StageQueueExecution, Retryable: true}
	}
	summary, err := report.Summarize(run.ReportKey, currentRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID", Stage: failure.StageBuildReport}
	}
	dashboard, err := report.BuildDashboard(run.ReportKey, run.Period, comparisonPeriod, currentRows, comparisonRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID", Stage: failure.StageBuildReport}
	}
	if comparisonWarning != "" {
		report.SetComparisonUnavailable(&dashboard, comparisonWarning)
	}
	summary.Dashboard = &dashboard
	return summary, nil
}

func (worker *ReportWorker) query(ctx context.Context, timeout time.Duration, connection sml.Connection, query report.Query) ([]map[string]string, *executionFailure) {
	rendered, err := report.RenderSQL(query)
	if err != nil {
		return nil, &executionFailure{Code: "REPORT_QUERY_RENDER_FAILED", Stage: failure.StageBuildReport}
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := worker.client.Query(queryCtx, connection, rendered)
	if err == nil {
		return rows, nil
	}
	var safeError *sml.SafeError
	if errors.As(err, &safeError) {
		return nil, failureFromSafeError(safeError)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, &executionFailure{Code: "SML_TIMEOUT", Stage: failure.StageWaitResponse, TransportPhase: failure.PhaseRequestSentResultUnknown, Retryable: true, RemoteStateUnknown: true}
	}
	return nil, &executionFailure{Code: "SML_QUERY_FAILED", Stage: failure.StageWaitResponse, TransportPhase: failure.PhaseRequestSentResultUnknown, Retryable: true, RemoteStateUnknown: true}
}

func flattenStepRows(steps map[string][]map[string]string) []map[string]string {
	rows := make([]map[string]string, 0)
	for _, step := range steps {
		rows = append(rows, step...)
	}
	return rows
}

func (worker *ReportWorker) executePlan(ctx context.Context, run report.Run, definition report.Definition, connection sml.Connection, plan report.QueryPlan) (map[string][]map[string]string, *executionFailure) {
	stepRows := make(map[string][]map[string]string, len(plan.Steps))
	totalRows := 0
	for _, step := range plan.Steps {
		rendered, err := report.RenderSQL(step.Query)
		if err != nil {
			return nil, &executionFailure{Code: "REPORT_QUERY_RENDER_FAILED", Stage: failure.StageBuildReport}
		}
		queryTimeout := definition.DetailTimeout
		if run.Source == report.SourceSchedule || run.Source == report.SourceBackground || run.ResultKind == report.ResultSummary {
			queryTimeout = definition.SummaryTimeout
		}
		queryCtx, cancel := context.WithTimeout(ctx, queryTimeout)
		rows, queryErr := worker.client.Query(queryCtx, connection, rendered)
		cancel()
		if queryErr != nil {
			var safeError *sml.SafeError
			if errors.As(queryErr, &safeError) {
				return nil, failureFromSafeError(safeError)
			}
			if errors.Is(queryErr, context.DeadlineExceeded) {
				return nil, &executionFailure{Code: "SML_TIMEOUT", Stage: failure.StageWaitResponse, TransportPhase: failure.PhaseRequestSentResultUnknown, Retryable: true, RemoteStateUnknown: true}
			}
			return nil, &executionFailure{Code: "SML_QUERY_FAILED", Stage: failure.StageWaitResponse, TransportPhase: failure.PhaseRequestSentResultUnknown, Retryable: true, RemoteStateUnknown: true}
		}
		totalRows += len(rows)
		if totalRows > definition.MaxRows {
			return nil, &executionFailure{Code: "REPORT_ROW_LIMIT_EXCEEDED", Stage: failure.StageBuildReport}
		}
		stepRows[step.Name] = rows
	}
	return stepRows, nil
}

func failureFromSafeError(safeError *sml.SafeError) *executionFailure {
	preRequest := safeError.Retryable && safeError.Phase == sml.BeforeRequestSent &&
		(safeError.Code == "SML_TIMEOUT" || safeError.Code == "SML_UNREACHABLE")
	result := &executionFailure{
		Code: safeError.Code, Retryable: safeError.Retryable,
		Stage:              failure.EvidenceForCode(safeError.Code).Stage,
		TransportPhase:     failure.TransportPhase(safeError.Phase),
		RemoteStateUnknown: safeError.Phase == sml.RequestSentResultUnknown,
		PreRequestFailure:  preRequest,
	}
	if safeError.ProtocolEvidence != nil {
		converted := convertProtocolEvidence(*safeError.ProtocolEvidence)
		result.ProtocolEvidence = &converted
	}
	return result
}

func convertProtocolEvidence(evidence sml.ProtocolEvidence) failure.JavaWSProtocolEvidence {
	return failure.JavaWSProtocolEvidence{
		RequestRef: evidence.RequestRef, RequestCount: evidence.RequestCount, RetryCount: evidence.RetryCount,
		RequestSentAt: evidence.RequestSentAt, FirstResponseByteAt: evidence.FirstResponseByteAt,
		ResponseCompletedAt: evidence.ResponseCompletedAt, HTTPStatus: evidence.HTTPStatus,
		ResponseContentType: evidence.ResponseContentType, ResponseBodyBytes: evidence.ResponseBodyBytes,
		SOAPValid: evidence.SOAPValid, SOAPReturnCharacters: evidence.SOAPReturnCharacters,
		Base64Valid: evidence.Base64Valid, DecodedPayloadBytes: evidence.DecodedPayloadBytes,
		ZIPSignatureValid: evidence.ZIPSignatureValid, ResponseSHA256: evidence.ResponseSHA256,
		TenantConcurrentQueries: evidence.TenantConcurrentQueries, HostConcurrentQueries: evidence.HostConcurrentQueries,
	}
}

func buildFailureEvidence(run report.Run, execution executionFailure, startedAt, occurredAt time.Time) failure.Evidence {
	duration := occurredAt.Sub(startedAt).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	attempt := run.Attempt
	evidence := failure.EvidenceForCode(execution.Code)
	evidence.Stage = execution.Stage
	evidence.TransportPhase = execution.TransportPhase
	evidence.OccurredAt = occurredAt
	evidence.StartedAt = &startedAt
	evidence.FinishedAt = &occurredAt
	evidence.DurationMS = &duration
	evidence.Attempt = &attempt
	evidence.Retryable = execution.Retryable
	evidence.RemoteStateUnknown = execution.RemoteStateUnknown
	evidence.ProtocolEvidence = execution.ProtocolEvidence
	if run.DataSourceVersion > 0 {
		connectionVersion := run.DataSourceVersion
		evidence.ConnectionVersion = &connectionVersion
	}
	return failure.Complete(evidence)
}

func emptyReportSteps(key report.Key) map[string][]map[string]string {
	if key == report.SalesGoodsServices || key == report.PurchaseGoodsPayables {
		return map[string][]map[string]string{"headers": {}, "details": {}}
	}
	return map[string][]map[string]string{"rows": {}}
}

func (worker *ReportWorker) keepLease(ctx context.Context, runID uuid.UUID, cancelExecution context.CancelFunc, leaseErrors chan<- error) {
	interval := worker.heartbeatInterval
	if interval <= 0 {
		interval = reportHeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := worker.store.ExtendLease(ctx, runID, worker.workerID, reportLeaseDuration, worker.now().UTC()); err != nil {
				select {
				case leaseErrors <- err:
				default:
				}
				cancelExecution()
				return
			}
		}
	}
}
