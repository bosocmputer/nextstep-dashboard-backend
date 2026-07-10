package quota

import (
	"context"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
)

type State string

const (
	StateReady     State = "READY"
	StateUnlimited State = "UNLIMITED"
	StateStale     State = "STALE"
	StateUnsynced  State = "UNSYNCED"
)

type Status struct {
	State                     State      `json:"state"`
	ProviderLimit             *int       `json:"providerLimit"`
	ProviderConsumed          *int       `json:"providerConsumed"`
	LocallyAccepted           int        `json:"locallyAccepted"`
	OperationalReservePercent int        `json:"operationalReservePercent"`
	SyncedAt                  *time.Time `json:"syncedAt"`
}

type Provider interface {
	Fetch(context.Context) (line.QuotaUsage, error)
}

type Store interface {
	Sync(context.Context, line.QuotaUsage, time.Time) (Status, error)
}

type Worker struct {
	provider Provider
	store    Store
	now      func() time.Time
}

func NewWorker(provider Provider, store Store, now func() time.Time) *Worker {
	return &Worker{provider: provider, store: store, now: now}
}

func (worker *Worker) Process(ctx context.Context) (Status, error) {
	usage, err := worker.provider.Fetch(ctx)
	if err != nil {
		return Status{}, err
	}
	return worker.store.Sync(ctx, usage, worker.now().UTC())
}
