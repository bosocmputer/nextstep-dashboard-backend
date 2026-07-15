package viewer

import (
	"errors"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type DashboardSnapshot struct {
	RunID                   uuid.UUID                `json:"runId"`
	Dashboard               report.Dashboard         `json:"dashboard"`
	PeriodFrom              string                   `json:"periodFrom,omitempty"`
	PeriodTo                string                   `json:"periodTo,omitempty"`
	SourceStartedAt         *time.Time               `json:"sourceStartedAt,omitempty"`
	SourceFinishedAt        *time.Time               `json:"sourceFinishedAt,omitempty"`
	FreshUntil              *time.Time               `json:"freshUntil,omitempty"`
	StaleUntil              *time.Time               `json:"staleUntil,omitempty"`
	FreshnessStatus         FreshnessStatus          `json:"freshnessStatus,omitempty"`
	ReportDefinitionVersion string                   `json:"reportDefinitionVersion,omitempty"`
	DataSourceVersion       int                      `json:"dataSourceVersion,omitempty"`
	QueryPlanFingerprint    string                   `json:"queryPlanFingerprint,omitempty"`
	SourceConsistency       report.SourceConsistency `json:"sourceConsistency,omitempty"`
	DetailsAvailable        bool                     `json:"detailsAvailable"`
	DetailsExpiresAt        *time.Time               `json:"detailsExpiresAt,omitempty"`
}

type FreshnessStatus string

const (
	FreshnessFresh      FreshnessStatus = "FRESH"
	FreshnessStale      FreshnessStatus = "STALE"
	FreshnessExpired    FreshnessStatus = "EXPIRED"
	FreshnessMissing    FreshnessStatus = "MISSING"
	FreshnessRefreshing FreshnessStatus = "REFRESHING"
	FreshnessFailed     FreshnessStatus = "REFRESH_FAILED"
)

type RevalidationDisposition string

const (
	RevalidationFreshCache        RevalidationDisposition = "FRESH_CACHE"
	RevalidationStaleRefreshing   RevalidationDisposition = "STALE_REFRESHING"
	RevalidationMissingRefreshing RevalidationDisposition = "MISSING_REFRESHING"
	RevalidationJoined            RevalidationDisposition = "JOINED"
	RevalidationDisabled          RevalidationDisposition = "DISABLED"
	RevalidationCircuitOpen       RevalidationDisposition = "CIRCUIT_OPEN"
)

type ReportRevalidation struct {
	Disposition    RevalidationDisposition `json:"disposition"`
	Snapshot       *DashboardSnapshot      `json:"snapshot,omitempty"`
	Run            *report.Run             `json:"run,omitempty"`
	RetryAfter     int                     `json:"retryAfter,omitempty"`
	LegacyFallback bool                    `json:"legacyFallback,omitempty"`
}

type OverviewRevalidation struct {
	Disposition    RevalidationDisposition `json:"disposition"`
	Overview       ExecutiveOverview       `json:"overview"`
	Runs           []report.Run            `json:"runs,omitempty"`
	RetryAfter     int                     `json:"retryAfter,omitempty"`
	LegacyFallback bool                    `json:"legacyFallback,omitempty"`
}

type ExecutiveOverview struct {
	TenantID          uuid.UUID                `json:"tenantId"`
	Timezone          string                   `json:"timezone"`
	GenerationID      *uuid.UUID               `json:"generationId,omitempty"`
	GenerationKey     string                   `json:"generationKey,omitempty"`
	RequestedPeriod   *report.Period           `json:"requestedPeriod,omitempty"`
	DataStatus        FreshnessStatus          `json:"dataStatus,omitempty"`
	SourceConsistency report.SourceConsistency `json:"sourceConsistency,omitempty"`
	SourceStartedAt   *time.Time               `json:"sourceStartedAt,omitempty"`
	SourceFinishedAt  *time.Time               `json:"sourceFinishedAt,omitempty"`
	PublishedAt       *time.Time               `json:"publishedAt,omitempty"`
	Items             []DashboardSnapshot      `json:"items"`
	ActiveRefresh     *DashboardRefresh        `json:"activeRefresh,omitempty"`
}

// SnapshotPeriodRequest identifies one exact report period for a batch snapshot lookup.
// The caller remains responsible for authorizing every report key before calling the store.
type SnapshotPeriodRequest struct {
	ReportKey report.Key
	Period    report.Period
}

type DashboardRefreshStatus string

const (
	DashboardRefreshQueued    DashboardRefreshStatus = "QUEUED"
	DashboardRefreshRunning   DashboardRefreshStatus = "RUNNING"
	DashboardRefreshPartial   DashboardRefreshStatus = "PARTIAL"
	DashboardRefreshSucceeded DashboardRefreshStatus = "SUCCEEDED"
	DashboardRefreshFailed    DashboardRefreshStatus = "FAILED"
)

type DashboardRefreshRun struct {
	ReportKey           report.Key               `json:"reportKey"`
	RunID               uuid.UUID                `json:"runId"`
	Status              report.RunStatus         `json:"status"`
	ProgressPhase       report.ProgressPhase     `json:"progressPhase,omitempty"`
	ProgressUpdatedAt   *time.Time               `json:"progressUpdatedAt,omitempty"`
	ExpectedP50MS       int64                    `json:"expectedP50Ms,omitempty"`
	ExpectedP90MS       int64                    `json:"expectedP90Ms,omitempty"`
	ExpectedSampleCount int                      `json:"expectedSampleCount"`
	ExecutionStrategy   report.ExecutionStrategy `json:"executionStrategy,omitempty"`
	SourceConsistency   report.SourceConsistency `json:"sourceConsistency,omitempty"`
	CompletedChunks     int                      `json:"completedChunks,omitempty"`
	TotalChunks         int                      `json:"totalChunks,omitempty"`
}

type DashboardRefresh struct {
	ID           uuid.UUID              `json:"id"`
	TenantID     uuid.UUID              `json:"tenantId"`
	GenerationID *uuid.UUID             `json:"generationId,omitempty"`
	Status       DashboardRefreshStatus `json:"status"`
	Total        int                    `json:"total"`
	Completed    int                    `json:"completed"`
	Failed       int                    `json:"failed"`
	Runs         []DashboardRefreshRun  `json:"runs"`
	CreatedAt    time.Time              `json:"createdAt"`
	FinishedAt   *time.Time             `json:"finishedAt,omitempty"`
}

type DashboardRefreshInput struct {
	PeriodPreset report.Preset
	DateFrom     *string
	DateTo       *string
	ReportKeys   []report.Key
}

type DashboardRefreshFailure struct {
	ReportKey     report.Key       `json:"reportKey"`
	Status        report.RunStatus `json:"status"`
	SafeErrorCode string           `json:"safeErrorCode,omitempty"`
}

type DashboardRefreshResult struct {
	RefreshID         uuid.UUID                 `json:"refreshId"`
	TenantID          uuid.UUID                 `json:"tenantId"`
	GenerationID      *uuid.UUID                `json:"generationId,omitempty"`
	GenerationKey     string                    `json:"generationKey,omitempty"`
	SourceConsistency report.SourceConsistency  `json:"sourceConsistency,omitempty"`
	SourceStartedAt   *time.Time                `json:"sourceStartedAt,omitempty"`
	SourceFinishedAt  *time.Time                `json:"sourceFinishedAt,omitempty"`
	PublishedAt       *time.Time                `json:"publishedAt,omitempty"`
	Status            DashboardRefreshStatus    `json:"status"`
	Items             []DashboardSnapshot       `json:"items"`
	Failures          []DashboardRefreshFailure `json:"failures"`
}

var ErrDashboardRefreshNotReady = errors.New("dashboard refresh result is not ready")
