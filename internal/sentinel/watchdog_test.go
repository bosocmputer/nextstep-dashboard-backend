package sentinel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchdogRequiresFreshMonitorAndHostProbe(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	heartbeat := MonitorHeartbeat{Version: 1, CheckedAt: now.Add(-10 * time.Second), Mode: ModeObserve, DatabaseReachable: true, LastEvaluationSucceeded: true}
	if err := WriteMonitorHeartbeat(directory, heartbeat); err != nil {
		t.Fatal(err)
	}
	backupAt, restoreAt := now.Add(-24*time.Hour), now.Add(-30*24*time.Hour)
	writeProbeFixture(t, directory, HostProbe{
		Version: 1, CheckedAt: now.Add(-10 * time.Second), Containers: HostContainers{API: true, Worker: true, Frontend: true, Postgres: true, Sentinel: true},
		DiskUsedPercent: 72, InodeUsedPercent: 10, MemoryAvailablePercent: 40, NTPSynchronized: true,
		Backup: HostBackup{LastSuccessAt: &backupAt, ChecksumValid: true, RestoreVerifiedAt: &restoreAt},
	})
	status := NewWatchdog(directory, func() time.Time { return now }).Status()
	if status.Status != "ok" || len(status.SafeErrorCodes) != 0 {
		t.Fatalf("status = %+v", status)
	}

	status = NewWatchdog(directory, func() time.Time { return now.Add(3 * time.Minute) }).Status()
	if status.Status != "degraded" || len(status.SafeErrorCodes) == 0 {
		t.Fatalf("stale status = %+v", status)
	}
}

func TestWatchdogRejectsMalformedOversizedAndCriticalProbe(t *testing.T) {
	directory := t.TempDir()
	now := time.Now().UTC()
	if err := WriteMonitorHeartbeat(directory, MonitorHeartbeat{Version: 1, CheckedAt: now, Mode: ModeObserve, DatabaseReachable: true, LastEvaluationSucceeded: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "host"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "host", "host-probe.json"), make([]byte, maximumHostProbeBytes+1), 0o640); err != nil {
		t.Fatal(err)
	}
	if status := NewWatchdog(directory, func() time.Time { return now }).Status(); status.Status != "degraded" {
		t.Fatalf("oversized = %+v", status)
	}

	backupAt, restoreAt := now.Add(-49*time.Hour), now.Add(-46*24*time.Hour)
	writeProbeFixture(t, directory, HostProbe{Version: 1, CheckedAt: now, Containers: HostContainers{API: true, Worker: false, Frontend: true, Postgres: true, Sentinel: true}, DiskUsedPercent: 93, InodeUsedPercent: 96, MemoryAvailablePercent: 4, NTPSynchronized: false, Backup: HostBackup{LastSuccessAt: &backupAt, ChecksumValid: false, RestoreVerifiedAt: &restoreAt}})
	status := NewWatchdog(directory, func() time.Time { return now }).Status()
	if status.Status != "degraded" || len(status.SafeErrorCodes) < 4 {
		t.Fatalf("critical = %+v", status)
	}
	for _, code := range status.SafeErrorCodes { if code == "BACKUP_OVERDUE" || code == "BACKUP_CHECKSUM_INVALID" || code == "RESTORE_VERIFICATION_OVERDUE" { t.Fatalf("pre-migration policy emitted backup health code: %+v", status) } }
}

func writeProbeFixture(t *testing.T, directory string, probe HostProbe) {
	t.Helper()
	body, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "host"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "host", "host-probe.json"), body, 0o640); err != nil {
		t.Fatal(err)
	}
}
