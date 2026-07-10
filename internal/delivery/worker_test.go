package delivery

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/google/uuid"
)

type memoryDeliveryStore struct {
	work       Work
	accepted   bool
	retried    bool
	failedCode string
	uncertain  bool
}

func (store *memoryDeliveryStore) Claim(context.Context, string, time.Duration, time.Time) (Work, error) {
	return store.work, nil
}

func (store *memoryDeliveryStore) Accept(context.Context, uuid.UUID, string, string, time.Time) error {
	store.accepted = true
	return nil
}

func (store *memoryDeliveryStore) Retry(_ context.Context, _ uuid.UUID, _, _ string, uncertain bool, _, _ time.Time) error {
	store.retried = true
	store.uncertain = uncertain
	return nil
}

func (store *memoryDeliveryStore) Fail(_ context.Context, _ uuid.UUID, _, code string, _ time.Time) error {
	store.failedCode = code
	return nil
}

type recipientResolverFunc func(context.Context, uuid.UUID) (string, error)

func (resolver recipientResolverFunc) OutboundLineUserID(ctx context.Context, recipientID uuid.UUID) (string, error) {
	return resolver(ctx, recipientID)
}

type senderFunc func(context.Context, string, uuid.UUID, json.RawMessage) line.PushResult

func (sender senderFunc) Push(ctx context.Context, lineUserID string, retryKey uuid.UUID, payload json.RawMessage) line.PushResult {
	return sender(ctx, lineUserID, retryKey, payload)
}

func TestWorkerAcceptsDeliveryWithPersistedRetryKey(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	retryKey := uuid.New()
	store := &memoryDeliveryStore{work: Work{ID: uuid.New(), RecipientID: uuid.New(), RetryKey: retryKey, Payload: json.RawMessage(`{"type":"flex"}`), Attempt: 1, TenantActive: true}}
	worker := NewWorker(store, recipientResolverFunc(func(context.Context, uuid.UUID) (string, error) { return "U123", nil }), senderFunc(func(_ context.Context, lineID string, gotRetryKey uuid.UUID, _ json.RawMessage) line.PushResult {
		if lineID != "U123" || gotRetryKey != retryKey {
			t.Fatalf("sender identity=%q retryKey=%s", lineID, gotRetryKey)
		}
		return line.PushResult{Outcome: line.PushAccepted, SafeCode: "LINE_PUSH_ALREADY_ACCEPTED"}
	}), "delivery-a", func() time.Time { return now })

	if err := worker.ProcessOne(context.Background()); err != nil || !store.accepted {
		t.Fatalf("ProcessOne() error=%v accepted=%v", err, store.accepted)
	}
}

func TestWorkerRetriesUncertainOutcomeAndStopsAtAttemptLimit(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	for _, attempt := range []int{1, 5} {
		store := &memoryDeliveryStore{work: Work{ID: uuid.New(), RecipientID: uuid.New(), RetryKey: uuid.New(), Payload: json.RawMessage(`{"type":"flex"}`), Attempt: attempt, TenantActive: true}}
		worker := NewWorker(store, recipientResolverFunc(func(context.Context, uuid.UUID) (string, error) { return "U123", nil }), senderFunc(func(context.Context, string, uuid.UUID, json.RawMessage) line.PushResult {
			return line.PushResult{Outcome: line.PushRetryable, SafeCode: "LINE_PUSH_UNCERTAIN", Uncertain: true}
		}), "delivery-a", func() time.Time { return now })
		if err := worker.ProcessOne(context.Background()); err != nil {
			t.Fatal(err)
		}
		if attempt == 1 && (!store.retried || !store.uncertain) {
			t.Fatalf("attempt 1 not retried: %+v", store)
		}
		if attempt == 5 && store.failedCode != "LINE_PUSH_RETRY_EXHAUSTED" {
			t.Fatalf("attempt 5 failedCode=%q", store.failedCode)
		}
	}
}

func TestWorkerFailsBeforeSendWhenTenantIsInactive(t *testing.T) {
	store := &memoryDeliveryStore{work: Work{ID: uuid.New(), TenantActive: false}}
	sent := false
	worker := NewWorker(store, nil, senderFunc(func(context.Context, string, uuid.UUID, json.RawMessage) line.PushResult {
		sent = true
		return line.PushResult{}
	}), "delivery-a", time.Now)
	if err := worker.ProcessOne(context.Background()); err != nil || sent || store.failedCode != "TENANT_INACTIVE" {
		t.Fatalf("error=%v sent=%v failedCode=%q", err, sent, store.failedCode)
	}
}
