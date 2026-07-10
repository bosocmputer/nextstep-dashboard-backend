package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/google/uuid"
)

var (
	ErrNoDeliveryReady   = errors.New("no LINE delivery is ready")
	ErrDeliveryLeaseLost = errors.New("LINE delivery lease was lost")
)

type Work struct {
	ID           uuid.UUID
	RecipientID  uuid.UUID
	RetryKey     uuid.UUID
	Payload      json.RawMessage
	Attempt      int
	TenantActive bool
}

type Store interface {
	Claim(context.Context, string, time.Duration, time.Time) (Work, error)
	Accept(context.Context, uuid.UUID, string, string, time.Time) error
	Retry(context.Context, uuid.UUID, string, string, bool, time.Time, time.Time) error
	Fail(context.Context, uuid.UUID, string, string, time.Time) error
}

type RecipientResolver interface {
	OutboundLineUserID(context.Context, uuid.UUID) (string, error)
}

type Sender interface {
	Push(context.Context, string, uuid.UUID, json.RawMessage) line.PushResult
}

type Worker struct {
	store      Store
	recipients RecipientResolver
	sender     Sender
	workerID   string
	now        func() time.Time
}

func NewWorker(store Store, recipients RecipientResolver, sender Sender, workerID string, now func() time.Time) *Worker {
	return &Worker{store: store, recipients: recipients, sender: sender, workerID: workerID, now: now}
}

func (worker *Worker) ProcessOne(ctx context.Context) error {
	now := worker.now().UTC()
	work, err := worker.store.Claim(ctx, worker.workerID, time.Minute, now)
	if err != nil {
		return err
	}
	if !work.TenantActive {
		return worker.store.Fail(ctx, work.ID, worker.workerID, "TENANT_INACTIVE", now)
	}
	lineUserID, err := worker.recipients.OutboundLineUserID(ctx, work.RecipientID)
	if err != nil {
		return worker.store.Fail(ctx, work.ID, worker.workerID, "RECIPIENT_INACTIVE", now)
	}
	result := worker.sender.Push(ctx, lineUserID, work.RetryKey, work.Payload)
	switch result.Outcome {
	case line.PushAccepted:
		return worker.store.Accept(ctx, work.ID, worker.workerID, result.ProviderRequestID, now)
	case line.PushRetryable:
		if work.Attempt >= 5 {
			return worker.store.Fail(ctx, work.ID, worker.workerID, "LINE_PUSH_RETRY_EXHAUSTED", now)
		}
		delay := result.RetryAfter
		if delay <= 0 {
			delay = retryDelay(work.Attempt)
		}
		return worker.store.Retry(ctx, work.ID, worker.workerID, result.SafeCode, result.Uncertain, now.Add(delay), now)
	default:
		return worker.store.Fail(ctx, work.ID, worker.workerID, result.SafeCode, now)
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 30 * time.Second * time.Duration(1<<(attempt-1))
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}
