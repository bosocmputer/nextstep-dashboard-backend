package sentinel

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

type RuntimeProbeSource struct {
	directory            string
	consecutiveUnhealthy map[string]int
	memoryCriticalSince  *time.Time
	backupPolicy         BackupPolicy
}

func NewRuntimeProbeSource(directory string) *RuntimeProbeSource {
	return &RuntimeProbeSource{directory: directory, consecutiveUnhealthy: make(map[string]int), backupPolicy: BackupPolicyPreMigrationOnly}
}

type BackupPolicy string

const (
	BackupPolicyPreMigrationOnly BackupPolicy = "PRE_MIGRATION_ONLY"
	BackupPolicyLocalDaily       BackupPolicy = "LOCAL_DAILY"
	BackupPolicyLocalAndOffsite  BackupPolicy = "LOCAL_AND_OFFSITE"
)

func ParseBackupPolicy(raw string) (BackupPolicy, error) {
	policy := BackupPolicy(strings.TrimSpace(raw))
	switch policy {
	case BackupPolicyPreMigrationOnly, BackupPolicyLocalDaily, BackupPolicyLocalAndOffsite:
		return policy, nil
	default:
		return "", fmt.Errorf("BACKUP_POLICY must be PRE_MIGRATION_ONLY, LOCAL_DAILY, or LOCAL_AND_OFFSITE")
	}
}

func (source *RuntimeProbeSource) ConfigureBackupPolicy(policy BackupPolicy) *RuntimeProbeSource {
	source.backupPolicy = policy
	return source
}

func (source *RuntimeProbeSource) Observations(now time.Time) []Observation {
	var probe HostProbe
	if err := readStrictJSON(filepath.Join(source.directory, "host", "host-probe.json"), maximumHostProbeBytes, &probe); err != nil || !validHostProbe(probe) || probe.CheckedAt.Before(now.Add(-2*time.Minute)) {
		if source.increment("host-probe") < 2 {
			return nil
		}
		return []Observation{platformObservation("HOST_PROBE_UNAVAILABLE", "HOST_PROBE_INVALID", SeverityP1, SourceHost, now)}
	}
	source.reset("host-probe")
	observations := make([]Observation, 0, 12)
	for key, healthy := range map[string]bool{"api": probe.Containers.API, "worker": probe.Containers.Worker, "frontend": probe.Containers.Frontend, "postgres": probe.Containers.Postgres, "sentinel": probe.Containers.Sentinel} {
		if healthy {
			source.reset("container-" + key)
			continue
		}
		if source.increment("container-"+key) < 2 {
			continue
		}
		observations = append(observations, platformObservation("NEXTSTEP_CONTAINER_UNHEALTHY", "CONTAINER_"+strings.ToUpper(key)+"_UNHEALTHY", SeverityP1, SourceHost, now))
	}
	if probe.DiskUsedPercent >= 92 {
		observations = append(observations, capacityObservation("HOST_DISK_CRITICAL", SeverityP1, probe.DiskUsedPercent, now))
	} else if probe.DiskUsedPercent >= 85 {
		observations = append(observations, capacityObservation("HOST_DISK_WARNING", SeverityP2, probe.DiskUsedPercent, now))
	}
	if probe.InodeUsedPercent >= 95 {
		observations = append(observations, capacityObservation("HOST_INODE_CRITICAL", SeverityP1, probe.InodeUsedPercent, now))
	}
	if probe.MemoryAvailablePercent <= 5 {
		if probe.MemoryCriticalSince != nil && (source.memoryCriticalSince == nil || probe.MemoryCriticalSince.Before(*source.memoryCriticalSince)) {
			started := probe.MemoryCriticalSince.UTC()
			source.memoryCriticalSince = &started
		} else if source.memoryCriticalSince == nil {
			started := now
			source.memoryCriticalSince = &started
		}
		if !source.memoryCriticalSince.After(now.Add(-5 * time.Minute)) {
			observations = append(observations, capacityObservation("HOST_MEMORY_CRITICAL", SeverityP1, probe.MemoryAvailablePercent, now))
		}
	} else {
		source.memoryCriticalSince = nil
		if probe.MemoryAvailablePercent <= 15 {
			observations = append(observations, capacityObservation("HOST_MEMORY_WARNING", SeverityP2, probe.MemoryAvailablePercent, now))
		}
	}
	if !probe.NTPSynchronized {
		observations = append(observations, platformObservation("HOST_TIME_UNSYNCHRONIZED", "HOST_TIME_UNSYNCHRONIZED", SeverityP1, SourceHost, now))
	}
	if source.backupPolicy != BackupPolicyPreMigrationOnly && (probe.Backup.LastSuccessAt == nil || probe.Backup.LastSuccessAt.Before(now.Add(-48*time.Hour))) {
		observations = append(observations, platformObservation("BACKUP_OVERDUE", "BACKUP_OVERDUE", SeverityP1, SourceBackup, now))
	} else if source.backupPolicy != BackupPolicyPreMigrationOnly && probe.Backup.LastSuccessAt != nil && probe.Backup.LastSuccessAt.Before(now.Add(-26*time.Hour)) {
		observations = append(observations, platformObservation("BACKUP_STALE", "BACKUP_STALE", SeverityP2, SourceBackup, now))
	}
	if source.backupPolicy != BackupPolicyPreMigrationOnly && !probe.Backup.ChecksumValid {
		observations = append(observations, platformObservation("BACKUP_CHECKSUM_INVALID", "BACKUP_CHECKSUM_INVALID", SeverityP1, SourceBackup, now))
	}
	if source.backupPolicy != BackupPolicyPreMigrationOnly && (probe.Backup.RestoreVerifiedAt == nil || probe.Backup.RestoreVerifiedAt.Before(now.Add(-45*24*time.Hour))) {
		observations = append(observations, platformObservation("RESTORE_VERIFICATION_OVERDUE", "RESTORE_VERIFICATION_OVERDUE", SeverityP1, SourceBackup, now))
	} else if source.backupPolicy != BackupPolicyPreMigrationOnly && probe.Backup.RestoreVerifiedAt != nil && probe.Backup.RestoreVerifiedAt.Before(now.Add(-35*24*time.Hour)) {
		observations = append(observations, platformObservation("RESTORE_VERIFICATION_STALE", "RESTORE_VERIFICATION_STALE", SeverityP2, SourceBackup, now))
	}
	if source.backupPolicy == BackupPolicyLocalAndOffsite && !probe.Backup.OffsiteConfigured {
		observations = append(observations, platformObservation("BACKUP_OFFSITE_NOT_CONFIGURED", "BACKUP_OFFSITE_NOT_CONFIGURED", SeverityP2, SourceBackup, now))
	}
	return observations
}

func (source *RuntimeProbeSource) increment(key string) int {
	source.consecutiveUnhealthy[key]++
	return source.consecutiveUnhealthy[key]
}
func (source *RuntimeProbeSource) reset(key string) { delete(source.consecutiveUnhealthy, key) }

func platformObservation(incidentType, safeCode string, severity Severity, sourceKind SourceKind, now time.Time) Observation {
	subjectType := SubjectHostResource
	if sourceKind == SourceBackup {
		subjectType = SubjectBackupPolicy
	} else if strings.HasPrefix(safeCode, "CONTAINER_") || strings.Contains(safeCode, "_CONTAINER_") {
		subjectType = SubjectContainer
	} else if sourceKind == SourceDatabase {
		subjectType = SubjectDatabase
	}
	return Observation{IncidentType: incidentType, RootCause: RootPlatform, Severity: severity, SourceKind: sourceKind, SourceID: stableSentinelID(incidentType), SafeErrorCode: safeCode, ObservedAt: now, ObservationMode: ObservationContinuous, SubjectType: subjectType, SubjectKey: ResourceSubjectKey(subjectType, incidentType)}
}
func capacityObservation(safeCode string, severity Severity, value float64, now time.Time) Observation {
	var measurement *Measurement
	switch safeCode {
	case "HOST_DISK_WARNING":
		measurement = &Measurement{Kind: MeasurementDiskUsedPercent, Value: value, Threshold: 85, Unit: MeasurementPercent}
	case "HOST_DISK_CRITICAL":
		measurement = &Measurement{Kind: MeasurementDiskUsedPercent, Value: value, Threshold: 92, Unit: MeasurementPercent}
	case "HOST_MEMORY_WARNING":
		measurement = &Measurement{Kind: MeasurementMemoryAvailablePercent, Value: value, Threshold: 15, Unit: MeasurementPercent}
	case "HOST_MEMORY_CRITICAL":
		measurement = &Measurement{Kind: MeasurementMemoryAvailablePercent, Value: value, Threshold: 5, Unit: MeasurementPercent}
	}
	return Observation{IncidentType: safeCode, RootCause: RootCapacity, Severity: severity, SourceKind: SourceHost, SourceID: stableSentinelID(safeCode), SafeErrorCode: safeCode, ObservedAt: now, ObservationMode: ObservationContinuous, SubjectType: SubjectHostResource, SubjectKey: ResourceSubjectKey(SubjectHostResource, safeCode), Measurement: measurement}
}
func stableSentinelID(value string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("nextstep-sentinel:"+value))
}
