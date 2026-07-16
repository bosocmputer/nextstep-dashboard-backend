package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const databasePingTimeout = 10 * time.Second

func poolConfig(databaseURL string, maxConnections, minConnections int) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database pool configuration: %w", err)
	}
	cfg.MaxConns = int32(maxConnections)
	cfg.MinConns = int32(minConnections)
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	return cfg, nil
}

// OpenPool creates a bounded pool and verifies connectivity before returning.
// Callers must avoid logging the returned error verbatim because it can contain
// connection-string details from PostgreSQL.
func OpenPool(ctx context.Context, databaseURL string, maxConnections, minConnections int) (*pgxpool.Pool, error) {
	cfg, err := poolConfig(databaseURL, maxConnections, minConnections)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, databasePingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// OpenSentinelPool deliberately does not ping before returning. Sentinel must
// stay alive long enough to use its file-backed emergency lane when PostgreSQL
// is already unavailable during process startup. No business runtime may use
// this helper because API/Worker startup must remain fail-fast on database loss.
func OpenSentinelPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := poolConfig(databaseURL, 4, 0)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create Sentinel database pool: %w", err)
	}
	return pool, nil
}
