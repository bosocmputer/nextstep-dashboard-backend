package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func RecordWorkerHeartbeat(ctx context.Context, pool *pgxpool.Pool, workerID, workerType, nodeName string, metadata map[string]any, now time.Time) error {
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode worker heartbeat metadata: %w", err)
	}
	_, err = pool.Exec(ctx, `
		insert into worker_heartbeats (worker_id, worker_type, node_name, metadata_json, started_at, heartbeat_at)
		values ($1, $2, $3, $4, $5, $5)
		on conflict (worker_id) do update
		set worker_type = excluded.worker_type,
		    node_name = excluded.node_name,
		    metadata_json = excluded.metadata_json,
		    heartbeat_at = excluded.heartbeat_at`,
		workerID, workerType, nodeName, metadataJSON, now,
	)
	if err != nil {
		return fmt.Errorf("record worker heartbeat: %w", err)
	}
	return nil
}
