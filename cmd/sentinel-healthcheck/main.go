package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
)

func main() {
	directory := os.Getenv("SENTINEL_RUNTIME_DIR")
	if directory == "" {
		directory = "/run/nextstep-dashboard"
	}
	rawPolicy := os.Getenv("BACKUP_POLICY")
	if rawPolicy == "" {
		rawPolicy = string(sentinel.BackupPolicyPreMigrationOnly)
	}
	policy, err := sentinel.ParseBackupPolicy(rawPolicy)
	if err != nil {
		os.Exit(1)
	}
	status := sentinel.NewWatchdog(directory, time.Now).ConfigureBackupPolicy(policy).Status()
	// Container health is intentionally based only on its own heartbeat. Host
	// probe failures are reported by /health/watchdog and must not restart the
	// monitor that is responsible for alerting about them.
	if !status.MonitorFresh || filepath.Clean(directory) == "." {
		os.Exit(1)
	}
}
