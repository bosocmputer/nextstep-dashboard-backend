package schedule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type memoryTestSendStore struct {
	execution Execution
	err       error
	calls     int
}

func (store *memoryTestSendStore) MaterializeTest(context.Context, []byte, string, string, uuid.UUID, uuid.UUID, time.Time) (Execution, error) {
	store.calls++
	return store.execution, store.err
}

func TestSendServiceRequiresConfiguredLINEBeforeEnqueue(t *testing.T) {
	store := &memoryTestSendStore{}
	service := NewTestSendService(store, false, time.Now)

	_, err := service.Enqueue(context.Background(), []byte("admin"), "request", "test-send-001", uuid.New(), uuid.New())
	var readinessError *ReadinessError
	if !errors.As(err, &readinessError) || len(readinessError.Blockers) != 1 || readinessError.Blockers[0] != BlockerLineNotConfigured || store.calls != 0 {
		t.Fatalf("Enqueue() error=%v calls=%d", err, store.calls)
	}
}

func TestSendServiceValidatesIdempotencyAndDelegates(t *testing.T) {
	execution := Execution{ID: uuid.New(), Status: ExecutionCollecting}
	store := &memoryTestSendStore{execution: execution}
	service := NewTestSendService(store, true, func() time.Time { return time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC) })

	if _, err := service.Enqueue(context.Background(), []byte("admin"), "request", "short", uuid.New(), uuid.New()); err == nil || store.calls != 0 {
		t.Fatalf("invalid idempotency accepted: err=%v calls=%d", err, store.calls)
	}
	result, err := service.Enqueue(context.Background(), []byte("admin"), "request", "test-send-001", uuid.New(), uuid.New())
	if err != nil || result.ID != execution.ID || store.calls != 1 {
		t.Fatalf("Enqueue()=%+v err=%v calls=%d", result, err, store.calls)
	}
}
