package report

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

type Source string

const (
	SourceDashboard Source = "DASHBOARD"
	SourceSchedule  Source = "SCHEDULE"
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
}

type Run struct {
	ID                   uuid.UUID
	TenantID             uuid.UUID
	ReportKey            Key
	Source               Source
	IdempotencyKey       string
	Status               RunStatus
	Period               Period
	RequestedByRecipient *uuid.UUID
	ClaimedBy            string
	LeaseExpiresAt       *time.Time
	Attempt              int
	RowCount             int
	IsTruncated          bool
	Summary              map[string]string
	Reconciliation       map[string]any
	SafeErrorCode        string
	SafeErrorMessage     string
	QueuedAt             time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	ExpiresAt            time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type RowsPage struct {
	Rows        []map[string]string
	NextOrdinal int
	HasMore     bool
}
