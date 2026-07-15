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

func WorkerNodeHealthy(ctx context.Context, pool *pgxpool.Pool, nodeName string, now time.Time) (bool, error) {
	if nodeName == "" || len(nodeName) > 255 {
		return false, nil
	}
	var reportReady, schedulerReady, retentionReady, notificationReady, deliveryReady bool
	err := pool.QueryRow(ctx, `
		select
		  coalesce(bool_or(
		    worker_type = 'REPORT'
		    and nullif(metadata_json ->> 'recoveryLoopAt', '')::timestamptz >= $2
		  ), false),
		  coalesce(bool_or(worker_type = 'SCHEDULER'), false),
		  coalesce(bool_or(worker_type = 'RETENTION'), false),
		  coalesce(bool_or(worker_type = 'DELIVERY' and metadata_json ->> 'stage' = 'prepare'), false),
		  coalesce(bool_or(worker_type = 'DELIVERY' and metadata_json ->> 'stage' = 'send'), false)
		from worker_heartbeats
		where node_name = $1 and heartbeat_at >= $2`, nodeName, now.Add(-45*time.Second)).Scan(
		&reportReady, &schedulerReady, &retentionReady, &notificationReady, &deliveryReady,
	)
	if err != nil {
		return false, fmt.Errorf("read worker heartbeat health: %w", err)
	}
	return reportReady && schedulerReady && retentionReady && notificationReady && deliveryReady, nil
}
