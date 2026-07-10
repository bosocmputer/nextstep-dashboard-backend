package retention

import (
	"context"
	"testing"
	"time"
)

type retentionStoreFunc func(context.Context, Policy, time.Time) (Counts, error)

func (run retentionStoreFunc) Run(ctx context.Context, policy Policy, now time.Time) (Counts, error) {
	return run(ctx, policy, now)
}

func TestProductionPolicyKeepsSnapshots90AndHistory365Days(t *testing.T) {
	policy := ProductionPolicy()
	if policy.SnapshotRetention != 90*24*time.Hour || policy.HistoryRetention != 365*24*time.Hour || policy.BatchSize != 5000 {
		t.Fatalf("ProductionPolicy() = %+v", policy)
	}
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	called := false
	worker := NewWorker(retentionStoreFunc(func(_ context.Context, got Policy, gotNow time.Time) (Counts, error) {
		called = got == policy && gotNow.Equal(now)
		return Counts{AuditLogs: 2}, nil
	}), policy, func() time.Time { return now })
	counts, err := worker.Process(context.Background())
	if err != nil || !called || counts.AuditLogs != 2 {
		t.Fatalf("Process() = %+v, %v called=%v", counts, err, called)
	}
}
