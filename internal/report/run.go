package report

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type Source string

const (
	SourceDashboard  Source = "DASHBOARD"
	SourceSchedule   Source = "SCHEDULE"
	SourceBackground Source = "BACKGROUND"
)

type ResultKind string

const (
	ResultDetail  ResultKind = "DETAIL"
	ResultSummary ResultKind = "SUMMARY"
)

type ProgressPhase string

const (
	ProgressQueued             ProgressPhase = "QUEUED"
	ProgressConnecting         ProgressPhase = "CONNECTING"
	ProgressQueryingCurrent    ProgressPhase = "QUERYING_CURRENT"
	ProgressQueryingComparison ProgressPhase = "QUERYING_COMPARISON"
	ProgressBuildingDashboard  ProgressPhase = "BUILDING_DASHBOARD"
	ProgressSavingResult       ProgressPhase = "SAVING_RESULT"
	ProgressWaitingRetry       ProgressPhase = "WAITING_RETRY"
	ProgressCompleted          ProgressPhase = "COMPLETED"
)

type RunStatus string

const (
	StatusQueued    RunStatus = "QUEUED"
	StatusClaimed   RunStatus = "CLAIMED"
	StatusRunning   RunStatus = "RUNNING"
	StatusSucceeded RunStatus = "SUCCEEDED"
	StatusFailed    RunStatus = "FAILED"
	StatusCancelled RunStatus = "CANCELLED"
	StatusExpired   RunStatus = "EXPIRED"
)

var (
	ErrRunNotFound            = errors.New("report run not found")
	ErrNoQueuedRun            = errors.New("no report run is available")
	ErrRunLeaseLost           = errors.New("report run lease was lost")
	ErrRunConcurrencyLimit    = errors.New("tenant report run concurrency limit reached")
	ErrRunCircuitOpen         = errors.New("tenant SML circuit is open")
	ErrRunIdempotencyConflict = errors.New("report run idempotency conflict")
	ErrRunRowsExpired         = errors.New("report run rows expired")
	ErrRunForbidden           = errors.New("report run is forbidden")
	ErrRunNotCancellable      = errors.New("report run cannot be cancelled")
)

type EnqueueInput struct {
	TenantID             uuid.UUID
	ReportKey            Key
	Source               Source
	IdempotencyKey       string
	Period               Period
	RequestedByRecipient *uuid.UUID
	ResultKind           ResultKind
	Priority             int
	ExecutionKey         string
}

type Run struct {
	ID                      uuid.UUID
	TenantID                uuid.UUID
	ReportKey               Key
	Source                  Source
	ResultKind              ResultKind
	Priority                int
	ExecutionKey            string
	IdempotencyKey          string
	Status                  RunStatus
	Period                  Period
	RequestedByRecipient    *uuid.UUID
	ClaimedBy               string
	LeaseExpiresAt          *time.Time
	Attempt                 int
	RowCount                int
	IsTruncated             bool
	Summary                 map[string]string
	Reconciliation          map[string]any
	SafeErrorCode           string
	SafeErrorMessage        string
	QueuedAt                time.Time
	StartedAt               *time.Time
	FinishedAt              *time.Time
	ExpiresAt               time.Time
	CreatedAt               time.Time
	UpdatedAt               time.Time
	ReportDefinitionVersion string
	DataSourceVersion       int
	ProgressPhase           ProgressPhase
	ProgressSequence        int
	ProgressCompletedSteps  int
	ProgressTotalSteps      int
	ProgressUpdatedAt       *time.Time
	ExpectedP50MS           int64
	ExpectedP90MS           int64
	ExpectedSampleCount     int
	QueuePosition           int
}

type RowsPage struct {
	Rows        []map[string]string
	NextOrdinal int
	HasMore     bool
}
