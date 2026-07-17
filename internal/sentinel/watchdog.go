package sentinel

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const maximumHostProbeBytes = 16 * 1024

type HostContainers struct {
	API      bool `json:"api"`
	Worker   bool `json:"worker"`
	Frontend bool `json:"frontend"`
	Postgres bool `json:"postgres"`
	Sentinel bool `json:"sentinel"`
}

type HostBackup struct {
	LastSuccessAt     *time.Time `json:"lastSuccessAt,omitempty"`
	ChecksumValid     bool       `json:"checksumValid"`
	RestoreVerifiedAt *time.Time `json:"restoreVerifiedAt,omitempty"`
	OffsiteConfigured bool       `json:"offsiteConfigured"`
}

type HostProbe struct {
	Version                int            `json:"version"`
	CheckedAt              time.Time      `json:"checkedAt"`
	Containers             HostContainers `json:"containers"`
	DiskUsedPercent        float64        `json:"diskUsedPercent"`
	InodeUsedPercent       float64        `json:"inodeUsedPercent"`
	MemoryAvailablePercent float64        `json:"memoryAvailablePercent"`
	MemoryCriticalSince    *time.Time     `json:"memoryCriticalSince,omitempty"`
	NTPSynchronized        bool           `json:"ntpSynchronized"`
	Backup                 HostBackup     `json:"backup"`
}

type WatchdogStatus struct {
	Status           string    `json:"status"`
	CheckedAt        time.Time `json:"checkedAt"`
	MonitorFresh     bool      `json:"monitorFresh"`
	HostProbeFresh   bool      `json:"hostProbeFresh"`
	SafeErrorCodes   []string  `json:"safeErrorCodes"`
	SafeWarningCodes []string  `json:"safeWarningCodes,omitempty"`
}

type Watchdog struct {
	runtimeDirectory string
	now              func() time.Time
	backupPolicy     BackupPolicy
}

func NewWatchdog(runtimeDirectory string, now func() time.Time) *Watchdog {
	return &Watchdog{runtimeDirectory: runtimeDirectory, now: now, backupPolicy: BackupPolicyPreMigrationOnly}
}

func (watchdog *Watchdog) ConfigureBackupPolicy(policy BackupPolicy) *Watchdog {
	watchdog.backupPolicy = policy
	return watchdog
}

func (watchdog *Watchdog) Status() WatchdogStatus {
	now := watchdog.now().UTC()
	status := WatchdogStatus{Status: "ok", CheckedAt: now, SafeErrorCodes: make([]string, 0), SafeWarningCodes: make([]string, 0)}
	var heartbeat MonitorHeartbeat
	if err := readStrictJSON(filepath.Join(watchdog.runtimeDirectory, "monitor", "monitor-heartbeat.json"), 4096, &heartbeat); err != nil || heartbeat.Version != 1 || heartbeat.EvaluationDurationMs < 0 || heartbeat.EvaluationDurationMs > 30_000 {
		status.SafeErrorCodes = append(status.SafeErrorCodes, "SENTINEL_HEARTBEAT_INVALID")
	} else {
		status.MonitorFresh = !heartbeat.CheckedAt.After(now.Add(30*time.Second)) && heartbeat.CheckedAt.After(now.Add(-90*time.Second))
		if !status.MonitorFresh {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "SENTINEL_HEARTBEAT_STALE")
		}
		if !heartbeat.DatabaseReachable {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "DATABASE_UNAVAILABLE")
		}
	}
	var probe HostProbe
	if err := readStrictJSON(filepath.Join(watchdog.runtimeDirectory, "host", "host-probe.json"), maximumHostProbeBytes, &probe); err != nil || !validHostProbe(probe) {
		status.SafeErrorCodes = append(status.SafeErrorCodes, "HOST_PROBE_INVALID")
	} else {
		status.HostProbeFresh = !probe.CheckedAt.After(now.Add(30*time.Second)) && probe.CheckedAt.After(now.Add(-2*time.Minute))
		if !status.HostProbeFresh {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "HOST_PROBE_STALE")
		}
		if !probe.Containers.API {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "API_CONTAINER_UNHEALTHY")
		}
		if !probe.Containers.Worker {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "WORKER_CONTAINER_UNHEALTHY")
		}
		if !probe.Containers.Frontend {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "FRONTEND_CONTAINER_UNHEALTHY")
		}
		if !probe.Containers.Postgres {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "POSTGRES_CONTAINER_UNHEALTHY")
		}
		if !probe.Containers.Sentinel {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "SENTINEL_CONTAINER_UNHEALTHY")
		}
		if probe.DiskUsedPercent >= 92 {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "HOST_DISK_CRITICAL")
		} else if probe.DiskUsedPercent >= 85 {
			status.SafeWarningCodes = append(status.SafeWarningCodes, "HOST_DISK_WARNING")
		}
		if probe.InodeUsedPercent >= 95 {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "HOST_INODE_CRITICAL")
		}
		if probe.MemoryCriticalSince != nil && !probe.MemoryCriticalSince.After(now.Add(-5*time.Minute)) && probe.MemoryAvailablePercent <= 5 {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "HOST_MEMORY_CRITICAL")
		} else if probe.MemoryAvailablePercent <= 15 {
			status.SafeWarningCodes = append(status.SafeWarningCodes, "HOST_MEMORY_WARNING")
		}
		if !probe.NTPSynchronized {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "HOST_TIME_UNSYNCHRONIZED")
		}
		if watchdog.backupPolicy != BackupPolicyPreMigrationOnly && (probe.Backup.LastSuccessAt == nil || probe.Backup.LastSuccessAt.Before(now.Add(-48*time.Hour))) {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "BACKUP_OVERDUE")
		} else if watchdog.backupPolicy != BackupPolicyPreMigrationOnly && probe.Backup.LastSuccessAt != nil && probe.Backup.LastSuccessAt.Before(now.Add(-26*time.Hour)) {
			status.SafeWarningCodes = append(status.SafeWarningCodes, "BACKUP_STALE")
		}
		if watchdog.backupPolicy != BackupPolicyPreMigrationOnly && !probe.Backup.ChecksumValid {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "BACKUP_CHECKSUM_INVALID")
		}
		if watchdog.backupPolicy != BackupPolicyPreMigrationOnly && (probe.Backup.RestoreVerifiedAt == nil || probe.Backup.RestoreVerifiedAt.Before(now.Add(-45*24*time.Hour))) {
			status.SafeErrorCodes = append(status.SafeErrorCodes, "RESTORE_VERIFICATION_OVERDUE")
		} else if watchdog.backupPolicy != BackupPolicyPreMigrationOnly && probe.Backup.RestoreVerifiedAt != nil && probe.Backup.RestoreVerifiedAt.Before(now.Add(-35*24*time.Hour)) {
			status.SafeWarningCodes = append(status.SafeWarningCodes, "RESTORE_VERIFICATION_STALE")
		}
		if watchdog.backupPolicy == BackupPolicyLocalAndOffsite && !probe.Backup.OffsiteConfigured {
			status.SafeWarningCodes = append(status.SafeWarningCodes, "BACKUP_OFFSITE_NOT_CONFIGURED")
		}
	}
	slices.Sort(status.SafeErrorCodes)
	slices.Sort(status.SafeWarningCodes)
	if len(status.SafeErrorCodes) > 0 {
		status.Status = "degraded"
	}
	return status
}

func validHostProbe(probe HostProbe) bool {
	return probe.Version == 1 && !probe.CheckedAt.IsZero() &&
		probe.DiskUsedPercent >= 0 && probe.DiskUsedPercent <= 100 && probe.InodeUsedPercent >= 0 && probe.InodeUsedPercent <= 100 &&
		probe.MemoryAvailablePercent >= 0 && probe.MemoryAvailablePercent <= 100
}

func readStrictJSON(path string, maximum int64, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maximum {
		return errors.New("runtime JSON file is invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maximum+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("runtime JSON has trailing data")
	}
	return nil
}
