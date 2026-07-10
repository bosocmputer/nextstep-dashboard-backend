package schedule

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrNoDueSchedule = errors.New("no notification schedule is due")

type ExecutionStatus string

const (
	ExecutionCollecting ExecutionStatus = "COLLECTING"
	ExecutionFailed     ExecutionStatus = "FAILED"
)

type Execution struct {
	ID            uuid.UUID       `json:"id"`
	TenantID      uuid.UUID       `json:"tenantId"`
	ScheduleID    uuid.UUID       `json:"scheduleId"`
	ScheduledFor  time.Time       `json:"scheduledFor"`
	Status        ExecutionStatus `json:"status"`
	SafeErrorCode string          `json:"safeErrorCode,omitempty"`
	ReportRunIDs  []uuid.UUID     `json:"reportRunIds"`
}

type DueStore interface {
	MaterializeDue(context.Context, string, time.Time) (Execution, error)
}

type DueWorker struct {
	store    DueStore
	workerID string
	now      func() time.Time
}

func NewDueWorker(store DueStore, workerID string, now func() time.Time) *DueWorker {
	return &DueWorker{store: store, workerID: workerID, now: now}
}

func (worker *DueWorker) ProcessOne(ctx context.Context) (Execution, error) {
	return worker.store.MaterializeDue(ctx, worker.workerID, worker.now().UTC())
}
