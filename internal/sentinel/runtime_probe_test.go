package sentinel

import (
	"os"
	"path/filepath"
	"strings"
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

func TestPreMigrationOnlyPolicySuppressesBackupAndRestoreObservations(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	writeProbeFixture(t, directory, HostProbe{
		Version: 1, CheckedAt: now,
		Containers:      HostContainers{API: true, Worker: true, Frontend: true, Postgres: true, Sentinel: true},
		DiskUsedPercent: 70, InodeUsedPercent: 10, MemoryAvailablePercent: 50, NTPSynchronized: true,
		Backup: HostBackup{},
	})
	got := NewRuntimeProbeSource(directory).ConfigureBackupPolicy(BackupPolicyPreMigrationOnly).Observations(now)
	for _, observation := range got {
		if observation.SourceKind == SourceBackup || strings.HasPrefix(observation.SafeErrorCode, "BACKUP_") || strings.HasPrefix(observation.SafeErrorCode, "RESTORE_") {
			t.Fatalf("pre-migration-only policy produced backup observation: %+v", observation)
		}
	}
}

func TestParseBackupPolicyFailsClosed(t *testing.T) {
	if got, err := ParseBackupPolicy("PRE_MIGRATION_ONLY"); err != nil || got != BackupPolicyPreMigrationOnly {
		t.Fatalf("policy=%q err=%v", got, err)
	}
	if _, err := ParseBackupPolicy("disabled"); err == nil {
		t.Fatal("invalid backup policy was accepted")
	}
}

func TestRuntimeCapacityObservationCarriesContinuousMeasurement(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	backupAt, restoreAt := now, now
	writeProbeFixture(t, directory, HostProbe{
		Version: 1, CheckedAt: now,
		Containers:      HostContainers{API: true, Worker: true, Frontend: true, Postgres: true, Sentinel: true},
		DiskUsedPercent: 89, InodeUsedPercent: 10, MemoryAvailablePercent: 50, NTPSynchronized: true,
		Backup: HostBackup{LastSuccessAt: &backupAt, ChecksumValid: true, RestoreVerifiedAt: &restoreAt, OffsiteConfigured: true},
	})
	got := NewRuntimeProbeSource(directory).Observations(now)
	if len(got) != 1 {
		t.Fatalf("observations = %+v", got)
	}
	observation := got[0]
	if observation.ObservationMode != ObservationContinuous || observation.SubjectType != SubjectHostResource || observation.Measurement == nil {
		t.Fatalf("continuous metadata = %+v", observation)
	}
	if observation.Measurement.Kind != MeasurementDiskUsedPercent || observation.Measurement.Value != 89 || observation.Measurement.Threshold != 85 {
		t.Fatalf("measurement = %+v", observation.Measurement)
	}
}
