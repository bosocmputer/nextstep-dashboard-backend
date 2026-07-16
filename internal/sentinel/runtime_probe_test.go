package sentinel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeProbeRequiresTwoBadRoundsAndHonorsPersistedMemoryWindow(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	source := NewRuntimeProbeSource(directory)
	if got := source.Observations(now); len(got) != 0 {
		t.Fatalf("first invalid probe=%v", got)
	}
	got := source.Observations(now.Add(time.Minute))
	if len(got) != 1 || got[0].IncidentType != "HOST_PROBE_UNAVAILABLE" {
		t.Fatalf("second invalid probe=%v", got)
	}

	backupAt, restoreAt, memorySince := now, now, now.Add(-6*time.Minute)
	writeProbeFixture(t, directory, HostProbe{
		Version: 1, CheckedAt: now, Containers: HostContainers{API: true, Worker: true, Frontend: true, Postgres: true, Sentinel: true},
		DiskUsedPercent: 70, InodeUsedPercent: 10, MemoryAvailablePercent: 4, MemoryCriticalSince: &memorySince, NTPSynchronized: true,
		Backup: HostBackup{LastSuccessAt: &backupAt, ChecksumValid: true, RestoreVerifiedAt: &restoreAt, OffsiteConfigured: true},
	})
	got = NewRuntimeProbeSource(directory).Observations(now)
	if len(got) != 1 || got[0].IncidentType != "HOST_MEMORY_CRITICAL" {
		t.Fatalf("persisted memory window=%v", got)
	}
}

func TestRuntimeProbeRejectsOversizedJSON(t *testing.T) {
	directory := t.TempDir()
	hostDirectory := filepath.Join(directory, "host")
	if err := os.MkdirAll(hostDirectory, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hostDirectory, "host-probe.json"), make([]byte, maximumHostProbeBytes+1), 0o640); err != nil {
		t.Fatal(err)
	}
	source := NewRuntimeProbeSource(directory)
	_ = source.Observations(time.Now().UTC())
	got := source.Observations(time.Now().UTC().Add(time.Minute))
	if len(got) != 1 || got[0].SafeErrorCode != "HOST_PROBE_INVALID" {
		t.Fatalf("oversized probe=%v", got)
	}
}
