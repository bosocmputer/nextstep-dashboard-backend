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
	UpdateProgress(context.Context, uuid.UUID, string, report.ProgressPhase, int, int, time.Time) error
	ExtendLease(context.Context, uuid.UUID, string, time.Duration, time.Time) error
	Complete(context.Context, uuid.UUID, string, report.SummaryResult, bool, time.Time) error
	Retry(context.Context, uuid.UUID, string, string, time.Time, time.Time) error
	RetryPreRequestFailure(context.Context, uuid.UUID, string, string, time.Time, time.Time) error
	Fail(context.Context, uuid.UUID, string, string, string, time.Time) error
	FailPreRequestFailure(context.Context, uuid.UUID, string, string, string, time.Time) error
	FailRemoteUnknown(context.Context, uuid.UUID, string, string, string, time.Time, time.Time) error
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
	Retryable          bool
	RemoteStateUnknown bool
	PreRequestFailure  bool
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
	if err := worker.store.MarkRunning(ctx, run.ID, worker.workerID, reportLeaseDuration, worker.now().UTC()); err != nil {
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

	summary, failure := worker.execute(executionCtx, run)
	stopExecution()
	select {
	case leaseErr := <-leaseErrors:
		return fmt.Errorf("extend report lease: %w", leaseErr)
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failure != nil {
		now = worker.now().UTC()
		if failure.RemoteStateUnknown {
			// The remote PostgreSQL query may still be running after JavaWS has
			// timed out. Failing the run and opening the tenant cooldown must be a
			// single transaction so a crash cannot leave either invariant half-set.
			return worker.store.FailRemoteUnknown(ctx, run.ID, worker.workerID, failure.Code, safeFailureMessage(failure.Code), now, now.Add(10*time.Minute))
		}
		if failure.PreRequestFailure {
			if run.Source == report.SourceDashboard && run.Attempt < 2 {
				return worker.store.RetryPreRequestFailure(ctx, run.ID, worker.workerID, failure.Code, now.Add(30*time.Second), now)
			}
			return worker.store.FailPreRequestFailure(ctx, run.ID, worker.workerID, failure.Code, safeFailureMessage(failure.Code), now)
		}
		if failure.Retryable && failure.Code != "SML_TIMEOUT" && run.Source != report.SourceBackground && run.Attempt < maximumReportAttempts {
			backoff := 30 * time.Second * time.Duration(1<<(run.Attempt-1))
			return worker.store.Retry(ctx, run.ID, worker.workerID, failure.Code, now.Add(backoff), now)
		}
		return worker.store.Fail(ctx, run.ID, worker.workerID, failure.Code, safeFailureMessage(failure.Code), now)
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
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
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
			return report.SummaryResult{}, &executionFailure{Code: connectionError.SafeCode, Retryable: connectionError.Retryable}
		}
		if errors.Is(err, sml.ErrConnectionNotConfigured) {
			return report.SummaryResult{}, &executionFailure{Code: "SML_NOT_CONFIGURED"}
		}
		return report.SummaryResult{}, &executionFailure{Code: "SML_CONNECTION_LOAD_FAILED", Retryable: true}
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
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
	}
	consistency := report.ConsistencyStatement
	if len(plan.Steps) > 1 || report.ComparisonSupported(run.ReportKey, run.Period) {
		consistency = report.ConsistencySerialWindow
	}
	if metadataStore, ok := worker.store.(SourceConsistencyStore); ok {
		if err := metadataStore.SetSourceConsistency(executionCtx, run.ID, worker.workerID, consistency, worker.now().UTC()); err != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Retryable: true}
		}
	}
	totalProgressSteps := 5
	if report.ComparisonSupported(run.ReportKey, run.Period) {
		totalProgressSteps = 6
	}
	if err := worker.store.UpdateProgress(executionCtx, run.ID, worker.workerID, report.ProgressQueryingCurrent, 2, totalProgressSteps, worker.now().UTC()); err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Retryable: true}
	}
	stepRows, failure := worker.executePlan(executionCtx, run, definition, connection, plan)
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
	comparisonRows := emptyReportSteps(run.ReportKey)
	comparisonWarning := ""
	if report.ComparisonSupported(run.ReportKey, run.Period) {
		if err := worker.store.UpdateProgress(executionCtx, run.ID, worker.workerID, report.ProgressQueryingComparison, 3, totalProgressSteps, worker.now().UTC()); err != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Retryable: true}
		}
		comparisonPlan, planErr := report.BuildQueryPlanForProjection(run.ReportKey, comparisonPeriod, projection)
		if planErr != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
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
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Retryable: true}
	}
	dashboard, err := report.BuildDashboard(run.ReportKey, run.Period, comparisonPeriod, stepRows, comparisonRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID"}
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
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_STORE_UNAVAILABLE"}
	}
	chunkCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	totalProgressSteps := 5
	if report.ComparisonSupported(run.ReportKey, run.Period) {
		totalProgressSteps = 6
	}
	if err := worker.store.UpdateProgress(chunkCtx, run.ID, worker.workerID, report.ProgressQueryingCurrent, 2, totalProgressSteps, worker.now().UTC()); err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Retryable: true}
	}
	manifestQuery, chunkSize, err := report.BuildChunkManifestQuery(run.ReportKey, run.Period)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
	}
	manifestRows, failure := worker.query(chunkCtx, definition.SummaryTimeout, connection, manifestQuery)
	if failure != nil {
		return report.SummaryResult{}, failure
	}
	unitKeys, err := report.ChunkKeys(manifestRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID"}
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
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_MANIFEST_PERSIST_FAILED"}
	}

	comparisonPeriod, err := report.ResolveComparisonPeriod(run.Period)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
	}
	comparisonSupported := report.ComparisonSupported(run.ReportKey, run.Period)
	currentChunks := make([]map[string][]map[string]string, 0, len(manifests))
	comparisonChunks := make([]map[string][]map[string]string, 0, len(manifests))
	comparisonWarning := ""
	for _, manifest := range manifests {
		if err := chunkStore.StartChunk(chunkCtx, run.ID, worker.workerID, manifest.Number, worker.now().UTC()); err != nil {
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_PROGRESS_FAILED"}
		}
		plan, planErr := report.BuildChunkQueryPlan(run.ReportKey, run.Period, projection, manifest.UnitKeys)
		if planErr != nil {
			_ = chunkStore.FailChunk(chunkCtx, run.ID, worker.workerID, manifest.Number, "REPORT_CONTRACT_INVALID", worker.now().UTC())
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
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
				return report.SummaryResult{}, &executionFailure{Code: "REPORT_CONTRACT_INVALID"}
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
			return report.SummaryResult{}, &executionFailure{Code: "REPORT_CHUNK_PROGRESS_FAILED"}
		}
		currentChunks = append(currentChunks, current)
		if comparisonSupported {
			comparisonChunks = append(comparisonChunks, comparison)
		}
	}
	currentRows, err := report.MergeChunkedSteps(run.ReportKey, projection, currentChunks)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID"}
	}
	if len(flattenStepRows(currentRows)) > definition.MaxRows {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_ROW_LIMIT_EXCEEDED"}
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
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_PROGRESS_FAILED", Retryable: true}
	}
	summary, err := report.Summarize(run.ReportKey, currentRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID"}
	}
	dashboard, err := report.BuildDashboard(run.ReportKey, run.Period, comparisonPeriod, currentRows, comparisonRows)
	if err != nil {
		return report.SummaryResult{}, &executionFailure{Code: "REPORT_OUTPUT_INVALID"}
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
		return nil, &executionFailure{Code: "REPORT_QUERY_RENDER_FAILED"}
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
		return nil, &executionFailure{Code: "SML_TIMEOUT", Retryable: true, RemoteStateUnknown: true}
	}
	return nil, &executionFailure{Code: "SML_QUERY_FAILED", Retryable: true, RemoteStateUnknown: true}
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
			return nil, &executionFailure{Code: "REPORT_QUERY_RENDER_FAILED"}
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
				return nil, &executionFailure{Code: "SML_TIMEOUT", Retryable: true, RemoteStateUnknown: true}
			}
			return nil, &executionFailure{Code: "SML_QUERY_FAILED", Retryable: true, RemoteStateUnknown: true}
		}
		totalRows += len(rows)
		if totalRows > definition.MaxRows {
			return nil, &executionFailure{Code: "REPORT_ROW_LIMIT_EXCEEDED"}
		}
		stepRows[step.Name] = rows
	}
	return stepRows, nil
}

func failureFromSafeError(safeError *sml.SafeError) *executionFailure {
	preRequest := safeError.Retryable && safeError.Phase == sml.BeforeRequestSent &&
		(safeError.Code == "SML_TIMEOUT" || safeError.Code == "SML_UNREACHABLE")
	return &executionFailure{
		Code: safeError.Code, Retryable: safeError.Retryable,
		RemoteStateUnknown: safeError.Phase == sml.RequestSentResultUnknown || safeError.Phase == sml.ResponseStarted,
		PreRequestFailure:  preRequest,
	}
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

func safeFailureMessage(code string) string {
	switch code {
	case "REPORT_OUTPUT_INVALID":
		return "ข้อมูลตัวเลขจาก SML อยู่ในรูปแบบที่ระบบไม่รองรับ"
	case "REPORT_SET_INCOMPLETE":
		return "สร้างรายงานในรอบนี้ไม่ครบ ระบบจึงไม่ส่ง LINE"
	case "SML_ZIP_FORMAT_INVALID":
		return "Server ลูกค้าส่งผลลัพธ์กลับมาในรูปแบบ ZIP ที่ไม่ถูกต้อง"
	case "SML_ZIP_EMPTY":
		return "Server ลูกค้าส่งผลลัพธ์ ZIP ที่ไม่มีข้อมูลกลับมา"
	case "SML_ZIP_TOO_LARGE":
		return "ผลลัพธ์จาก Server ลูกค้ามีขนาดใหญ่เกินขอบเขตที่ปลอดภัย"
	case "SML_ZIP_READ_FAILED":
		return "ระบบอ่านผลลัพธ์ ZIP จาก Server ลูกค้าไม่สำเร็จ"
	case "SML_ZIP_INVALID":
		return "ผลลัพธ์ ZIP จาก Server ลูกค้าไม่สมบูรณ์"
	}
	return fmt.Sprintf("Report run failed safely (%s).", code)
}
