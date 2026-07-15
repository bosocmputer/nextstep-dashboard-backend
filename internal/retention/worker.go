package retention

import (
	"context"
	"time"
)

type Policy struct {
	SnapshotRetention   time.Duration
	HistoryRetention    time.Duration
	ExpiredSessionGrace time.Duration
	BatchSize           int
}

func ProductionPolicy() Policy {
	return Policy{
		SnapshotRetention:   90 * 24 * time.Hour,
		HistoryRetention:    365 * 24 * time.Hour,
		ExpiredSessionGrace: 7 * 24 * time.Hour,
		BatchSize:           5000,
	}
}

type Counts struct {
	ReportRows             int64
	ReportRuns             int64
	DashboardRefreshes     int64
	DashboardGenerations   int64
	ScrubbedReportRuns     int64
	ScrubbedOutboxPayloads int64
	Deliveries             int64
	NotificationRuns       int64
	AuditLogs              int64
	Sessions               int64
	IdempotencyRequests    int64
	AccessLinks            int64
}

type Store interface {
	Run(context.Context, Policy, time.Time) (Counts, error)
}

type Worker struct {
	store  Store
	policy Policy
	now    func() time.Time
}

func NewWorker(store Store, policy Policy, now func() time.Time) *Worker {
	return &Worker{store: store, policy: policy, now: now}
}

func (worker *Worker) Process(ctx context.Context) (Counts, error) {
	return worker.store.Run(ctx, worker.policy, worker.now().UTC())
}
