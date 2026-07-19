package sentinel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type MonitorHeartbeat struct {
	Version                 int                         `json:"version"`
	CheckedAt               time.Time                   `json:"checkedAt"`
	Mode                    Mode                        `json:"mode"`
	DatabaseReachable       bool                        `json:"databaseReachable"`
	LastEvaluationSucceeded bool                        `json:"lastEvaluationSucceeded"`
	EvaluationDurationMs    int64                       `json:"evaluationDurationMs"`
	TelegramContextStatus   TelegramTenantContextStatus `json:"telegramContextStatus,omitempty"`
	TelegramContextTotals   map[string]uint64           `json:"sentinelTelegramContextTotal,omitempty"`
}

func WriteMonitorHeartbeat(runtimeDirectory string, heartbeat MonitorHeartbeat) error {
	body, err := json.Marshal(heartbeat)
	if err != nil || len(body) > 4096 {
		return fmt.Errorf("encode Sentinel heartbeat")
	}
	monitorDirectory := filepath.Join(runtimeDirectory, "monitor")
	if err := os.MkdirAll(monitorDirectory, 0o750); err != nil {
		return fmt.Errorf("create Sentinel runtime directory: %w", err)
	}
	temporary, err := os.CreateTemp(monitorDirectory, ".monitor-heartbeat-*")
	if err != nil {
		return fmt.Errorf("create Sentinel heartbeat: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, bytes.NewReader(body)); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, filepath.Join(monitorDirectory, "monitor-heartbeat.json"))
}
