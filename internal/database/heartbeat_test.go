package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerNodeHealthRequiresEveryCriticalStage(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	node := "worker-health-test"
	stages := []struct {
		id, workerType string
		metadata       map[string]any
	}{
		{"report", "REPORT", nil}, {"scheduler", "SCHEDULER", nil}, {"retention", "RETENTION", nil},
		{"delivery-prepare", "DELIVERY", map[string]any{"stage": "prepare"}},
		{"delivery-send", "DELIVERY", map[string]any{"stage": "send"}},
	}
	for index, stage := range stages {
		if err := RecordWorkerHeartbeat(ctx, pool, node+"-"+stage.id, stage.workerType, node, stage.metadata, now); err != nil {
			t.Fatal(err)
		}
		healthy, err := WorkerNodeHealthy(ctx, pool, node, now)
		if err != nil {
			t.Fatal(err)
		}
		if healthy != (index == len(stages)-1) {
			t.Fatalf("healthy after %d stages = %v", index+1, healthy)
		}
	}
	if healthy, err := WorkerNodeHealthy(ctx, pool, node, now.Add(time.Minute)); err != nil || healthy {
		t.Fatalf("stale worker healthy=%v err=%v", healthy, err)
	}
}
