package schedule

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

type TestSendStore interface {
	MaterializeTest(context.Context, []byte, string, string, uuid.UUID, uuid.UUID, time.Time) (Execution, error)
}

type TestSendService struct {
	store     TestSendStore
	lineReady bool
	now       func() time.Time
}

func NewTestSendService(store TestSendStore, lineReady bool, now func() time.Time) *TestSendService {
	return &TestSendService{store: store, lineReady: lineReady, now: now}
}

func (service *TestSendService) Enqueue(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, tenantID, scheduleID uuid.UUID) (Execution, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return Execution{}, &ValidationError{Field: "idempotencyKey", Code: "INVALID_IDEMPOTENCY_KEY"}
	}
	if !service.lineReady {
		return Execution{}, &ReadinessError{Blockers: []string{BlockerLineNotConfigured}}
	}
	return service.store.MaterializeTest(ctx, actorHash, requestID, idempotencyKey, tenantID, scheduleID, service.now().UTC())
}
