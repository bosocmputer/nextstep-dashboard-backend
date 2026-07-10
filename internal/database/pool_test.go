package database

import (
	"testing"
	"time"
)

func TestPoolConfigAppliesProductionBounds(t *testing.T) {
	cfg, err := poolConfig("postgres://nextstep@example.internal/nextstep?sslmode=verify-full", 24, 3)
	if err != nil {
		t.Fatalf("poolConfig() error = %v", err)
	}
	if cfg.MaxConns != 24 || cfg.MinConns != 3 {
		t.Fatalf("connections = %d/%d, want 3/24", cfg.MinConns, cfg.MaxConns)
	}
	if cfg.MaxConnLifetime != 30*time.Minute || cfg.MaxConnIdleTime != 5*time.Minute || cfg.HealthCheckPeriod != 30*time.Second {
		t.Fatalf("unexpected pool lifecycle: lifetime=%s idle=%s health=%s", cfg.MaxConnLifetime, cfg.MaxConnIdleTime, cfg.HealthCheckPeriod)
	}
}
