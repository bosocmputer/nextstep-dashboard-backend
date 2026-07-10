package quota

import (
	"context"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
)

type fakeProvider struct {
	usage line.QuotaUsage
	err   error
}

func (provider fakeProvider) Fetch(context.Context) (line.QuotaUsage, error) {
	return provider.usage, provider.err
}

type fakeStore struct {
	status Status
	usage  line.QuotaUsage
	calls  int
}

func (store *fakeStore) Sync(_ context.Context, usage line.QuotaUsage, _ time.Time) (Status, error) {
	store.calls++
	store.usage = usage
	return store.status, nil
}

func TestWorkerSyncsProviderWideUsage(t *testing.T) {
	limit := 5000
	store := &fakeStore{status: Status{ProviderLimit: &limit, ProviderConsumed: intPointer(4200), State: StateReady}}
	worker := NewWorker(fakeProvider{usage: line.QuotaUsage{Limit: &limit, Consumed: 4200}}, store, time.Now)

	status, err := worker.Process(context.Background())
	if err != nil || store.calls != 1 || store.usage.Consumed != 4200 || status.State != StateReady {
		t.Fatalf("Process() = %+v, %v calls=%d usage=%+v", status, err, store.calls, store.usage)
	}
}

func intPointer(value int) *int { return &value }
