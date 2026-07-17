package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const incidentBurstWindow = 5 * time.Minute
const continuousPersistenceInterval = 5 * time.Minute

type operationalEpisode struct {
	id            uuid.UUID
	family        string
	fingerprint   string
	alertRef      string
	incidentType  string
	rootCause     sentinel.RootCause
	severity      sentinel.Severity
	safeErrorCode string
	mode          sentinel.ObservationMode
	subjectType   sentinel.SubjectType
	firstSeenAt   time.Time
	burstUntil    time.Time
	new           bool
	subjects      map[string]operationalSubject
}

type operationalSubject struct {
	status           string
	lastSeenAt       time.Time
	lastPersistedAt  time.Time
	lastFailureAt    *time.Time
	safeErrorCode    string
	measurementValue *float64
}

type observationAssignment struct {
	observation sentinel.Observation
	incidentID  uuid.UUID
	eventKind   string
	increment   bool
	meaningful  bool
}

func normalizeOperationalObservation(observation sentinel.Observation, now time.Time) sentinel.Observation {
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	}
	observation.ObservedAt = observation.ObservedAt.UTC()
	if observation.ObservationMode == "" {
		observation.ObservationMode = sentinel.ObservationDiscrete
	}
	if observation.SubjectType == "" {
		switch {
		case observation.TenantID != nil:
			observation.SubjectType = sentinel.SubjectTenant
		case observation.SourceKind == sentinel.SourceBackup:
			observation.SubjectType = sentinel.SubjectBackupPolicy
		case observation.SourceKind == sentinel.SourceDatabase:
			observation.SubjectType = sentinel.SubjectDatabase
		case observation.SourceKind == sentinel.SourceDelivery:
			observation.SubjectType = sentinel.SubjectLineProvider
		case strings.Contains(observation.SafeErrorCode, "CONTAINER"):
			observation.SubjectType = sentinel.SubjectContainer
		default:
			observation.SubjectType = sentinel.SubjectHostResource
		}
	}
	if observation.SubjectKey == "" {
		if observation.TenantID != nil {
			observation.SubjectKey = sentinel.TenantSubjectKey(*observation.TenantID)
		} else {
			observation.SubjectKey = sentinel.ResourceSubjectKey(observation.SubjectType, string(observation.SourceKind)+":"+observation.SourceID.String())
		}
	}
	return observation
}

func episodeFingerprint(family string, id uuid.UUID) string {
	digest := sha256.Sum256([]byte("nextstep-incident-episode:" + family + ":" + id.String()))
	return hex.EncodeToString(digest[:])
}

func meaningfulMeasurement(previous *float64, next *sentinel.Measurement) bool {
	if next == nil {
		return previous != nil
	}
	return previous == nil || math.Abs(*previous-next.Value) >= 1
}

func chooseOperationalEpisode(episodes []*operationalEpisode, observation sentinel.Observation) *operationalEpisode {
	for _, episode := range episodes {
		if subject, exists := episode.subjects[observation.SubjectKey]; exists && subject.status == "ACTIVE" {
			return episode
		}
	}
	for _, episode := range episodes {
		if episode.mode == observation.ObservationMode && !observation.ObservedAt.After(episode.burstUntil) {
			return episode
		}
	}
	return nil
}

func (store *SentinelStore) RecordObservations(ctx context.Context, observations []sentinel.Observation, now time.Time, aggregationWindow time.Duration, enqueue bool) error {
	if len(observations) == 0 {
		return nil
	}
	now = now.UTC()
	allNormalized := make([]sentinel.Observation, 0, len(observations))
	for _, observation := range observations {
		observation = normalizeOperationalObservation(observation, now)
		allNormalized = append(allNormalized, observation)
	}
	familySet := make(map[string]struct{})
	for _, observation := range allNormalized {
		familySet[observation.Fingerprint()] = struct{}{}
	}
	families := make([]string, 0, len(familySet))
	for family := range familySet {
		families = append(families, family)
	}
	sort.Strings(families)

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operational observation episode: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended(family, 0)) from unnest($1::text[]) family order by family`, families); err != nil {
		return fmt.Errorf("lock operational incident families: %w", err)
	}
	normalized, err := filterRecordedOperationalObservations(ctx, tx, allNormalized)
	if err != nil {
		return err
	}
	correlationKeys := make([]string, 0)
	for _, observation := range normalized {
		if observation.CorrelationKey != "" {
			correlationKeys = append(correlationKeys, observation.CorrelationKey)
		}
	}

	episodesByFamily, err := loadOperationalEpisodes(ctx, tx, families)
	if err != nil {
		return err
	}
	correlationIncidents, err := loadCorrelationIncidents(ctx, tx, correlationKeys)
	if err != nil {
		return err
	}

	sort.SliceStable(normalized, func(i, j int) bool {
		if normalized[i].Downstream != normalized[j].Downstream {
			return !normalized[i].Downstream
		}
		if !normalized[i].ObservedAt.Equal(normalized[j].ObservedAt) {
			return normalized[i].ObservedAt.Before(normalized[j].ObservedAt)
		}
		return normalized[i].SourceID.String() < normalized[j].SourceID.String()
	})
	assignments := make([]observationAssignment, 0, len(normalized))
	newEpisodes := make([]*operationalEpisode, 0)
	updateCandidates := make(map[uuid.UUID]struct{})
	versionBumps := make(map[uuid.UUID]struct{})

	for _, observation := range normalized {
		if observation.Downstream {
			incidentID, exists := correlationIncidents[observation.CorrelationKey]
			if !exists {
				continue
			}
			assignments = append(assignments, observationAssignment{observation: observation, incidentID: incidentID, eventKind: "DOWNSTREAM_IMPACT"})
			continue
		}
		family := observation.Fingerprint()
		episode := chooseOperationalEpisode(episodesByFamily[family], observation)
		if episode == nil {
			alertRef, err := sentinel.NewAlertReference()
			if err != nil {
				return err
			}
			id := uuid.New()
			episode = &operationalEpisode{
				id: id, family: family, fingerprint: episodeFingerprint(family, id), alertRef: alertRef,
				incidentType: observation.IncidentType, rootCause: observation.RootCause, severity: observation.Severity,
				safeErrorCode: observation.SafeErrorCode, mode: observation.ObservationMode, subjectType: observation.SubjectType,
				firstSeenAt: observation.ObservedAt, burstUntil: observation.ObservedAt.Add(incidentBurstWindow), new: true,
				subjects: make(map[string]operationalSubject),
			}
			episodesByFamily[family] = append([]*operationalEpisode{episode}, episodesByFamily[family]...)
			newEpisodes = append(newEpisodes, episode)
		}
		previous, exists := episode.subjects[observation.SubjectKey]
		isNewSubject := !exists || previous.status != "ACTIVE"
		meaningful := isNewSubject || previous.safeErrorCode != observation.SafeErrorCode || meaningfulMeasurement(previous.measurementValue, observation.Measurement)
		persist := observation.ObservationMode == sentinel.ObservationDiscrete || isNewSubject || meaningful || observation.ObservedAt.Sub(previous.lastPersistedAt) >= continuousPersistenceInterval
		if !persist {
			continue
		}
		eventKind := "OBSERVED"
		increment := true
		if observation.ObservationMode == sentinel.ObservationContinuous && !isNewSubject {
			increment = meaningful
			if meaningful {
				eventKind = "CONDITION_UPDATED"
			} else {
				eventKind = ""
			}
		}
		assignments = append(assignments, observationAssignment{observation: observation, incidentID: episode.id, eventKind: eventKind, increment: increment, meaningful: meaningful || isNewSubject || observation.ObservationMode == sentinel.ObservationDiscrete})
		measurementValue := previous.measurementValue
		if observation.Measurement != nil {
			value := observation.Measurement.Value
			measurementValue = &value
		}
		failureAt := observation.ObservedAt
		episode.subjects[observation.SubjectKey] = operationalSubject{
			status: "ACTIVE", lastSeenAt: observation.ObservedAt, lastPersistedAt: observation.ObservedAt,
			lastFailureAt: &failureAt, safeErrorCode: observation.SafeErrorCode, measurementValue: measurementValue,
		}
		if observation.CorrelationKey != "" {
			correlationIncidents[observation.CorrelationKey] = episode.id
		}
		if isNewSubject && !episode.new {
			updateCandidates[episode.id] = struct{}{}
		}
		if meaningful || isNewSubject || observation.ObservationMode == sentinel.ObservationDiscrete {
			versionBumps[episode.id] = struct{}{}
		}
	}

	if err := insertOperationalEpisodes(ctx, tx, newEpisodes, aggregationWindow, now); err != nil {
		return err
	}
	if err := upsertOperationalSubjects(ctx, tx, assignments); err != nil {
		return err
	}
	if err := insertOperationalEvents(ctx, tx, assignments); err != nil {
		return err
	}
	impactedIDs := assignmentIncidentIDs(assignments)
	if err := reconcileOperationalEpisodes(ctx, tx, impactedIDs, mapKeys(versionBumps), now); err != nil {
		return err
	}
	if enqueue {
		if err := enqueueOperationalEpisodeAlerts(ctx, tx, newEpisodes, mapKeys(updateCandidates), now); err != nil {
			return err
		}
	}
	if err := advanceCursorRows(ctx, tx, allNormalized, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit operational observation episode: %w", err)
	}
	return nil
}

type operationalObservationIdentity struct {
	SourceKind string    `json:"source_kind"`
	SourceID   uuid.UUID `json:"source_id"`
	ObservedAt time.Time `json:"observed_at"`
	EventKind  string    `json:"event_kind"`
}

func operationalObservationIdentityFor(observation sentinel.Observation) (operationalObservationIdentity, bool) {
	eventKind := "OBSERVED"
	if observation.Downstream {
		eventKind = "DOWNSTREAM_IMPACT"
	} else if observation.ObservationMode != sentinel.ObservationDiscrete {
		return operationalObservationIdentity{}, false
	}
	return operationalObservationIdentity{SourceKind: string(observation.SourceKind), SourceID: observation.SourceID, ObservedAt: observation.ObservedAt, EventKind: eventKind}, true
}

func operationalObservationIdentityKey(identity operationalObservationIdentity) string {
	return identity.SourceKind + "\x00" + identity.SourceID.String() + "\x00" + identity.ObservedAt.UTC().Format(time.RFC3339Nano) + "\x00" + identity.EventKind
}

// The monitor intentionally reads a five-minute cursor overlap. Deduplicate
// durable discrete events before subject counters are changed; relying on the
// event table's unique constraint would reject the event only after the subject
// occurrence had already been incremented.
func filterRecordedOperationalObservations(ctx context.Context, tx pgx.Tx, observations []sentinel.Observation) ([]sentinel.Observation, error) {
	identities := make([]operationalObservationIdentity, 0, len(observations))
	for _, observation := range observations {
		if identity, ok := operationalObservationIdentityFor(observation); ok {
			identities = append(identities, identity)
		}
	}
	recorded := make(map[string]struct{})
	if len(identities) > 0 {
		payload, err := json.Marshal(identities)
		if err != nil {
			return nil, fmt.Errorf("encode operational observation identities: %w", err)
		}
		rows, err := tx.Query(ctx, `
			select input.source_kind, input.source_id, input.observed_at, input.event_kind
			from jsonb_to_recordset($1::jsonb) input(source_kind text, source_id uuid, observed_at timestamptz, event_kind text)
			where exists (
			  select 1 from operational_incident_events event
			  where event.source_kind = input.source_kind and event.source_id = input.source_id
			    and event.observed_at = input.observed_at and event.event_kind = input.event_kind
			)`, payload)
		if err != nil {
			return nil, fmt.Errorf("inspect recorded operational observations: %w", err)
		}
		for rows.Next() {
			var identity operationalObservationIdentity
			if err := rows.Scan(&identity.SourceKind, &identity.SourceID, &identity.ObservedAt, &identity.EventKind); err != nil {
				rows.Close()
				return nil, err
			}
			recorded[operationalObservationIdentityKey(identity)] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	filtered := make([]sentinel.Observation, 0, len(observations))
	seen := make(map[string]struct{})
	for _, observation := range observations {
		identity, durable := operationalObservationIdentityFor(observation)
		if !durable {
			filtered = append(filtered, observation)
			continue
		}
		key := operationalObservationIdentityKey(identity)
		if _, exists := recorded[key]; exists {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, observation)
	}
	return filtered, nil
}

func loadOperationalEpisodes(ctx context.Context, tx pgx.Tx, families []string) (map[string][]*operationalEpisode, error) {
	result := make(map[string][]*operationalEpisode)
	rows, err := tx.Query(ctx, `
		select incident.id, incident.family_fingerprint, incident.fingerprint, incident.alert_ref,
		       incident.incident_type, incident.root_cause, incident.severity, coalesce(incident.safe_error_code, ''),
		       incident.observation_mode, incident.subject_type, incident.first_seen_at, incident.burst_until,
		       subject.subject_key, subject.status, subject.last_seen_at, subject.last_persisted_at,
		       subject.last_failure_at, subject.safe_error_code, subject.measurement_value
		from operational_incidents incident
		left join operational_incident_subjects subject on subject.incident_id = incident.id
		where incident.family_fingerprint = any($1::text[]) and incident.status in ('OPEN', 'ACKNOWLEDGED')
		order by incident.first_seen_at desc, incident.id desc`, families)
	if err != nil {
		return nil, fmt.Errorf("load operational incident episodes: %w", err)
	}
	defer rows.Close()
	byID := make(map[uuid.UUID]*operationalEpisode)
	for rows.Next() {
		var episode operationalEpisode
		var subjectKey, subjectStatus, subjectSafeCode *string
		var subjectLastSeen, subjectLastPersisted *time.Time
		var lastFailureAt *time.Time
		var measurementValue *float64
		if err := rows.Scan(&episode.id, &episode.family, &episode.fingerprint, &episode.alertRef,
			&episode.incidentType, &episode.rootCause, &episode.severity, &episode.safeErrorCode,
			&episode.mode, &episode.subjectType, &episode.firstSeenAt, &episode.burstUntil,
			&subjectKey, &subjectStatus, &subjectLastSeen, &subjectLastPersisted, &lastFailureAt, &subjectSafeCode, &measurementValue); err != nil {
			return nil, fmt.Errorf("scan operational incident episode: %w", err)
		}
		stored := byID[episode.id]
		if stored == nil {
			episode.subjects = make(map[string]operationalSubject)
			stored = &episode
			byID[episode.id] = stored
			result[episode.family] = append(result[episode.family], stored)
		}
		if subjectKey != nil && subjectStatus != nil && subjectLastSeen != nil && subjectLastPersisted != nil && subjectSafeCode != nil {
			stored.subjects[*subjectKey] = operationalSubject{
				status: *subjectStatus, lastSeenAt: *subjectLastSeen, lastPersistedAt: *subjectLastPersisted,
				lastFailureAt: lastFailureAt, safeErrorCode: *subjectSafeCode, measurementValue: measurementValue,
			}
		}
	}
	return result, rows.Err()
}

func loadCorrelationIncidents(ctx context.Context, tx pgx.Tx, keys []string) (map[string]uuid.UUID, error) {
	result := make(map[string]uuid.UUID)
	if len(keys) == 0 {
		return result, nil
	}
	rows, err := tx.Query(ctx, `
		select distinct on (event.correlation_key) event.correlation_key, event.incident_id
		from operational_incident_events event
		join operational_incidents incident on incident.id = event.incident_id
		where event.correlation_key = any($1::text[]) and incident.status in ('OPEN', 'ACKNOWLEDGED')
		order by event.correlation_key, event.observed_at desc, event.id desc`, keys)
	if err != nil {
		return nil, fmt.Errorf("load incident correlation: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var id uuid.UUID
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		result[key] = id
	}
	return result, rows.Err()
}

func insertOperationalEpisodes(ctx context.Context, tx pgx.Tx, episodes []*operationalEpisode, aggregationWindow time.Duration, now time.Time) error {
	for _, episode := range episodes {
		if _, err := tx.Exec(ctx, `
			insert into operational_incidents (
			  id, alert_ref, fingerprint, family_fingerprint, incident_type, root_cause, severity,
			  safe_error_code, occurrence_count, affected_count, active_affected_count,
			  first_seen_at, last_seen_at, aggregation_until, burst_until, reminder_due_at,
			  observation_mode, subject_type, created_at, updated_at
			) values ($1,$2,$3,$4,$5,$6,$7,nullif($8,''),1,1,1,$9,$9,$10,$11,$12,$13,$14,$15,$15)`,
			episode.id, episode.alertRef, episode.fingerprint, episode.family, episode.incidentType,
			episode.rootCause, episode.severity, episode.safeErrorCode, episode.firstSeenAt,
			episode.firstSeenAt.Add(aggregationWindow), episode.burstUntil, episode.firstSeenAt.Add(time.Hour),
			episode.mode, episode.subjectType, now); err != nil {
			return fmt.Errorf("insert operational incident episode: %w", err)
		}
	}
	return nil
}

type persistedSubjectMutation struct {
	IncidentID           uuid.UUID `json:"incident_id"`
	SubjectKey           string    `json:"subject_key"`
	SubjectType          string    `json:"subject_type"`
	TenantID             string    `json:"tenant_id"`
	SourceKind           string    `json:"source_kind"`
	ObservationMode      string    `json:"observation_mode"`
	ObservedAt           time.Time `json:"observed_at"`
	Increment            bool      `json:"increment"`
	SafeErrorCode        string    `json:"safe_error_code"`
	FailureCategory      string    `json:"failure_category"`
	FailureStage         string    `json:"failure_stage"`
	TransportPhase       string    `json:"transport_phase"`
	MeasurementKind      string    `json:"measurement_kind"`
	MeasurementValue     *float64  `json:"measurement_value"`
	MeasurementThreshold *float64  `json:"measurement_threshold"`
	MeasurementUnit      string    `json:"measurement_unit"`
}

func upsertOperationalSubjects(ctx context.Context, tx pgx.Tx, assignments []observationAssignment) error {
	mutations := make([]persistedSubjectMutation, 0, len(assignments))
	heartbeats := make([]persistedSubjectMutation, 0)
	for _, assignment := range assignments {
		if assignment.observation.Downstream {
			continue
		}
		observation := assignment.observation
		tenantID := ""
		if observation.TenantID != nil {
			tenantID = observation.TenantID.String()
		}
		mutation := persistedSubjectMutation{
			IncidentID: assignment.incidentID, SubjectKey: observation.SubjectKey, SubjectType: string(observation.SubjectType),
			TenantID: tenantID, SourceKind: string(observation.SourceKind), ObservationMode: string(observation.ObservationMode),
			ObservedAt: observation.ObservedAt, Increment: assignment.increment, SafeErrorCode: observation.SafeErrorCode,
		}
		if observation.Evidence != nil {
			mutation.FailureCategory = string(observation.Evidence.Category)
			mutation.FailureStage = string(observation.Evidence.Stage)
			mutation.TransportPhase = string(observation.Evidence.TransportPhase)
		}
		if observation.Measurement != nil {
			value, threshold := observation.Measurement.Value, observation.Measurement.Threshold
			mutation.MeasurementKind, mutation.MeasurementValue, mutation.MeasurementThreshold, mutation.MeasurementUnit = string(observation.Measurement.Kind), &value, &threshold, string(observation.Measurement.Unit)
		}
		if assignment.increment {
			mutations = append(mutations, mutation)
		} else {
			heartbeats = append(heartbeats, mutation)
		}
	}
	if len(mutations) > 0 {
		payload, err := json.Marshal(mutations)
		if err != nil {
			return fmt.Errorf("encode operational incident subjects: %w", err)
		}
		_, err = tx.Exec(ctx, `
		insert into operational_incident_subjects (
		  incident_id, subject_key, subject_type, tenant_id, source_kind, status, observation_mode,
		  first_seen_at, last_seen_at, last_persisted_at, last_failure_at, occurrence_count,
		  safe_error_code, failure_category, failure_stage, transport_phase,
		  measurement_kind, measurement_value, measurement_threshold, measurement_unit
		)
		select input.incident_id, input.subject_key, input.subject_type,
		       nullif(input.tenant_id, '')::uuid, input.source_kind, 'ACTIVE', input.observation_mode,
		       input.observed_at, input.observed_at, input.observed_at, input.observed_at, 1,
		       input.safe_error_code, nullif(input.failure_category, ''), nullif(input.failure_stage, ''),
		       nullif(input.transport_phase, ''), nullif(input.measurement_kind, ''), input.measurement_value,
		       input.measurement_threshold, nullif(input.measurement_unit, '')
		from jsonb_to_recordset($1::jsonb) as input(
		  incident_id uuid, subject_key text, subject_type text, tenant_id text, source_kind text,
		  observation_mode text, observed_at timestamptz, increment boolean, safe_error_code text,
		  failure_category text, failure_stage text, transport_phase text, measurement_kind text,
		  measurement_value double precision, measurement_threshold double precision, measurement_unit text
		)
		on conflict (incident_id, subject_key) do update
		set status = 'ACTIVE', recovered_at = null,
		    last_seen_at = greatest(operational_incident_subjects.last_seen_at, excluded.last_seen_at),
		    last_persisted_at = greatest(operational_incident_subjects.last_persisted_at, excluded.last_persisted_at),
		    last_failure_at = greatest(coalesce(operational_incident_subjects.last_failure_at, excluded.last_failure_at), excluded.last_failure_at),
		    occurrence_count = operational_incident_subjects.occurrence_count + 1,
		    safe_error_code = excluded.safe_error_code,
		    failure_category = excluded.failure_category, failure_stage = excluded.failure_stage,
		    transport_phase = excluded.transport_phase, measurement_kind = excluded.measurement_kind,
		    measurement_value = excluded.measurement_value, measurement_threshold = excluded.measurement_threshold,
		    measurement_unit = excluded.measurement_unit`, payload)
		if err != nil {
			return fmt.Errorf("upsert operational incident subjects: %w", err)
		}
	}
	if len(heartbeats) > 0 {
		payload, err := json.Marshal(heartbeats)
		if err != nil {
			return fmt.Errorf("encode operational incident heartbeats: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			update operational_incident_subjects subject
			set last_seen_at = greatest(subject.last_seen_at, input.observed_at),
			    last_persisted_at = greatest(subject.last_persisted_at, input.observed_at),
			    measurement_value = coalesce(input.measurement_value, subject.measurement_value)
			from jsonb_to_recordset($1::jsonb) as input(
			  incident_id uuid, subject_key text, observed_at timestamptz, measurement_value double precision
			)
			where subject.incident_id = input.incident_id and subject.subject_key = input.subject_key`, payload); err != nil {
			return fmt.Errorf("persist operational incident heartbeats: %w", err)
		}
	}
	return nil
}

type persistedOperationalEvent struct {
	IncidentID          uuid.UUID  `json:"incident_id"`
	EventKind           string     `json:"event_kind"`
	SourceKind          string     `json:"source_kind"`
	SourceID            uuid.UUID  `json:"source_id"`
	TenantID            string     `json:"tenant_id"`
	SafeErrorCode       string     `json:"safe_error_code"`
	ObservedAt          time.Time  `json:"observed_at"`
	CorrelationKey      string     `json:"correlation_key"`
	Downstream          bool       `json:"downstream"`
	EvidenceVersion     *int       `json:"failure_evidence_version"`
	EvidenceLevel       string     `json:"failure_level"`
	EvidenceCategory    string     `json:"failure_category"`
	EvidenceStage       string     `json:"failure_stage"`
	EvidenceTransport   string     `json:"failure_transport_phase"`
	EvidenceOccurredAt  *time.Time `json:"failure_occurred_at"`
	EvidenceDurationMS  *int64     `json:"failure_duration_ms"`
	EvidenceAttempt     *int       `json:"failure_attempt"`
	EvidenceRetryable   *bool      `json:"failure_retryable"`
	RemoteStateUnknown  *bool      `json:"failure_remote_state_unknown"`
	ConnectionVersion   *int       `json:"connection_version"`
	ReportKey           string     `json:"report_key"`
	TriggerKind         string     `json:"trigger_kind"`
	ReportsTotal        *int       `json:"reports_total"`
	ReportsSucceeded    *int       `json:"reports_succeeded"`
	ReportsFailed       *int       `json:"reports_failed"`
	ReportsCancelled    *int       `json:"reports_cancelled"`
	NotificationOutcome string     `json:"notification_outcome"`
}

func insertOperationalEvents(ctx context.Context, tx pgx.Tx, assignments []observationAssignment) error {
	events := make([]persistedOperationalEvent, 0, len(assignments))
	for _, assignment := range assignments {
		if assignment.eventKind == "" {
			continue
		}
		observation := assignment.observation
		tenantID := ""
		if observation.TenantID != nil {
			tenantID = observation.TenantID.String()
		}
		event := persistedOperationalEvent{
			IncidentID: assignment.incidentID, EventKind: assignment.eventKind, SourceKind: string(observation.SourceKind),
			SourceID: observation.SourceID, TenantID: tenantID, SafeErrorCode: observation.SafeErrorCode,
			ObservedAt: observation.ObservedAt, CorrelationKey: observation.CorrelationKey, Downstream: observation.Downstream,
			ReportKey: observation.ReportKey, TriggerKind: string(observation.TriggerKind),
		}
		if evidence := observation.Evidence; evidence != nil {
			if evidence.Version > 0 {
				value := evidence.Version
				event.EvidenceVersion = &value
			}
			event.EvidenceLevel, event.EvidenceCategory, event.EvidenceStage = string(evidence.Level), string(evidence.Category), string(evidence.Stage)
			event.EvidenceTransport, event.EvidenceOccurredAt, event.EvidenceDurationMS, event.EvidenceAttempt = string(evidence.TransportPhase), &evidence.OccurredAt, evidence.DurationMS, evidence.Attempt
			retryable, remote := evidence.Retryable, evidence.RemoteStateUnknown
			event.EvidenceRetryable, event.RemoteStateUnknown, event.ConnectionVersion = &retryable, &remote, evidence.ConnectionVersion
		}
		if impact := observation.Impact; impact != nil {
			total, succeeded, failed, cancelled := impact.ReportsTotal, impact.ReportsSucceeded, impact.ReportsFailed, impact.ReportsCancelled
			event.ReportsTotal, event.ReportsSucceeded, event.ReportsFailed, event.ReportsCancelled = &total, &succeeded, &failed, &cancelled
			event.NotificationOutcome = string(impact.Notification)
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return nil
	}
	payload, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("encode operational incident events: %w", err)
	}
	_, err = tx.Exec(ctx, `
		insert into operational_incident_events (
		  incident_id, event_kind, source_kind, source_id, tenant_id, safe_error_code, observed_at,
		  correlation_key, downstream, failure_evidence_version, failure_level, failure_category,
		  failure_stage, failure_transport_phase, failure_occurred_at, failure_duration_ms,
		  failure_attempt, failure_retryable, failure_remote_state_unknown, connection_version,
		  report_key, trigger_kind, reports_total, reports_succeeded, reports_failed,
		  reports_cancelled, notification_outcome
		)
		select input.incident_id, input.event_kind, input.source_kind, input.source_id,
		       nullif(input.tenant_id, '')::uuid, nullif(input.safe_error_code, ''), input.observed_at,
		       nullif(input.correlation_key, ''), input.downstream, input.failure_evidence_version,
		       nullif(input.failure_level, ''), nullif(input.failure_category, ''), nullif(input.failure_stage, ''),
		       nullif(input.failure_transport_phase, ''), input.failure_occurred_at, input.failure_duration_ms,
		       input.failure_attempt, input.failure_retryable, input.failure_remote_state_unknown,
		       input.connection_version, nullif(input.report_key, ''), nullif(input.trigger_kind, ''),
		       input.reports_total, input.reports_succeeded, input.reports_failed, input.reports_cancelled,
		       nullif(input.notification_outcome, '')
		from jsonb_to_recordset($1::jsonb) as input(
		  incident_id uuid, event_kind text, source_kind text, source_id uuid, tenant_id text,
		  safe_error_code text, observed_at timestamptz, correlation_key text, downstream boolean,
		  failure_evidence_version integer, failure_level text, failure_category text, failure_stage text,
		  failure_transport_phase text, failure_occurred_at timestamptz, failure_duration_ms bigint,
		  failure_attempt integer, failure_retryable boolean, failure_remote_state_unknown boolean,
		  connection_version integer, report_key text, trigger_kind text, reports_total integer,
		  reports_succeeded integer, reports_failed integer, reports_cancelled integer, notification_outcome text
		)
		on conflict do nothing`, payload)
	if err != nil {
		return fmt.Errorf("insert operational incident events: %w", err)
	}
	return nil
}

func reconcileOperationalEpisodes(ctx context.Context, tx pgx.Tx, incidentIDs, versionBumps []uuid.UUID, now time.Time) error {
	if len(incidentIDs) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		with subject_aggregate as (
		  select subject.incident_id, sum(subject.occurrence_count)::integer occurrence_count,
		         count(*)::integer affected_count,
		         count(*) filter (where subject.status = 'ACTIVE')::integer active_affected_count,
		         min(subject.first_seen_at) first_seen_at, max(subject.last_seen_at) last_seen_at,
		         case when count(distinct subject.safe_error_code) = 1 then max(subject.safe_error_code) else 'MULTIPLE_SAFE_ERRORS' end subject_safe_error_code,
		         case when count(*) = 1 then max(subject.measurement_kind) end measurement_kind,
		         case when count(*) = 1 then max(subject.measurement_value) end measurement_value,
		         case when count(*) = 1 then max(subject.measurement_threshold) end measurement_threshold,
		         case when count(*) = 1 then max(subject.measurement_unit) end measurement_unit
		  from operational_incident_subjects subject where subject.incident_id = any($1::uuid[])
		  group by subject.incident_id
		), event_aggregate as (
		  select event.incident_id, count(distinct event.safe_error_code)::integer safe_error_code_count,
		         max(event.safe_error_code) safe_error_code
		  from operational_incident_events event
		  where event.incident_id = any($1::uuid[]) and not event.downstream
		    and event.safe_error_code is not null
		  group by event.incident_id
		)
		update operational_incidents incident
		set occurrence_count = aggregate.occurrence_count,
		    affected_count = aggregate.affected_count,
		    active_affected_count = aggregate.active_affected_count,
		    first_seen_at = least(incident.first_seen_at, aggregate.first_seen_at),
		    last_seen_at = greatest(incident.last_seen_at, aggregate.last_seen_at),
		    safe_error_code = case
		      when coalesce(event.safe_error_code_count, 0) > 1 then 'MULTIPLE_SAFE_ERRORS'
		      when event.safe_error_code_count = 1 then event.safe_error_code
		      else aggregate.subject_safe_error_code
		    end,
		    measurement_kind = aggregate.measurement_kind,
		    measurement_value = aggregate.measurement_value,
		    measurement_threshold = aggregate.measurement_threshold,
		    measurement_unit = aggregate.measurement_unit
		from subject_aggregate aggregate
		left join event_aggregate event on event.incident_id = aggregate.incident_id
		where incident.id = aggregate.incident_id`, incidentIDs); err != nil {
		return fmt.Errorf("reconcile operational incident episodes: %w", err)
	}
	if len(versionBumps) > 0 {
		if _, err := tx.Exec(ctx, `update operational_incidents set version = version + 1, updated_at = $2 where id = any($1::uuid[]) and created_at <> $2`, versionBumps, now); err != nil {
			return fmt.Errorf("version meaningful operational changes: %w", err)
		}
	}
	return nil
}

func enqueueOperationalEpisodeAlerts(ctx context.Context, tx pgx.Tx, newEpisodes []*operationalEpisode, updateCandidates []uuid.UUID, now time.Time) error {
	newIDs := make([]uuid.UUID, 0, len(newEpisodes))
	for _, episode := range newEpisodes {
		newIDs = append(newIDs, episode.id)
	}
	if len(newIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into operational_alert_outbox (incident_id, alert_kind, available_at, created_at, updated_at)
			select incident.id, 'OPEN', incident.aggregation_until, $2, $2
			from operational_incidents incident
			where incident.id = any($1::uuid[]) and incident.status = 'OPEN' and incident.severity = 'P1'
			on conflict (incident_id, alert_kind) do nothing`, newIDs, now); err != nil {
			return fmt.Errorf("enqueue operational episode open alerts: %w", err)
		}
	}
	if len(updateCandidates) > 0 {
		if _, err := tx.Exec(ctx, `
			insert into operational_alert_outbox (incident_id, alert_kind, available_at, created_at, updated_at)
			select incident.id, 'UPDATE', greatest($2, incident.burst_until), $2, $2
			from operational_incidents incident
			where incident.id = any($1::uuid[]) and incident.status = 'OPEN' and incident.severity = 'P1'
			  and not incident.update_alert_sent
			  and exists (select 1 from operational_alert_outbox opened where opened.incident_id = incident.id and opened.alert_kind = 'OPEN' and opened.status = 'SENT')
			on conflict (incident_id, alert_kind) do nothing`, updateCandidates, now); err != nil {
			return fmt.Errorf("enqueue operational episode update alerts: %w", err)
		}
	}
	return nil
}

func advanceCursorRows(ctx context.Context, tx pgx.Tx, observations []sentinel.Observation, now time.Time) error {
	type cursorAdvance struct {
		observedAt time.Time
		sourceID   uuid.UUID
	}
	advances := make(map[string]cursorAdvance)
	for _, observation := range observations {
		if observation.CursorKey == "" {
			continue
		}
		current, exists := advances[observation.CursorKey]
		if !exists || observation.ObservedAt.After(current.observedAt) || (observation.ObservedAt.Equal(current.observedAt) && observation.SourceID.String() > current.sourceID.String()) {
			advances[observation.CursorKey] = cursorAdvance{observedAt: observation.ObservedAt, sourceID: observation.SourceID}
		}
	}
	for key, advance := range advances {
		if _, err := tx.Exec(ctx, `
			update operational_monitor_cursors
			set cursor_updated_at = greatest(cursor_updated_at, $2), cursor_id = $3, updated_at = $4
			where monitor_key = $1`, key, advance.observedAt, advance.sourceID, now); err != nil {
			return fmt.Errorf("advance operational observation cursor %s: %w", key, err)
		}
	}
	return nil
}

func assignmentIncidentIDs(assignments []observationAssignment) []uuid.UUID {
	set := make(map[uuid.UUID]struct{})
	for _, assignment := range assignments {
		set[assignment.incidentID] = struct{}{}
	}
	return mapKeys(set)
}

func mapKeys(values map[uuid.UUID]struct{}) []uuid.UUID {
	result := make([]uuid.UUID, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

type activeOperationalSubject struct {
	Family     string `json:"family"`
	SubjectKey string `json:"subject_key"`
}

type recoveredOperationalSubject struct {
	incidentID uuid.UUID
	sourceKind string
	tenantID   *uuid.UUID
}

func (store *SentinelStore) AdvanceLifecycle(ctx context.Context, activeObservations []sentinel.Observation, continuousSnapshotComplete bool, now time.Time, enqueue bool) error {
	now = now.UTC()
	active := make([]activeOperationalSubject, 0)
	seen := make(map[string]struct{})
	for _, observation := range activeObservations {
		observation = normalizeOperationalObservation(observation, now)
		if observation.ObservationMode != sentinel.ObservationContinuous || observation.Downstream {
			continue
		}
		key := observation.Fingerprint() + "\x00" + observation.SubjectKey
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		active = append(active, activeOperationalSubject{Family: observation.Fingerprint(), SubjectKey: observation.SubjectKey})
	}
	activeJSON, err := json.Marshal(active)
	if err != nil {
		return fmt.Errorf("encode active operational subjects: %w", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin operational subject lifecycle: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	recovered := make([]recoveredOperationalSubject, 0)
	if continuousSnapshotComplete {
		rows, err := tx.Query(ctx, `
			update operational_incident_subjects subject
			set status = 'RECOVERED', recovered_at = $2
			from operational_incidents incident
			where incident.id = subject.incident_id and incident.status in ('OPEN', 'ACKNOWLEDGED')
			  and subject.status = 'ACTIVE' and subject.observation_mode = 'CONTINUOUS'
			  and not exists (
			    select 1 from jsonb_to_recordset($1::jsonb) active(family text, subject_key text)
			    where active.family = incident.family_fingerprint and active.subject_key = subject.subject_key
			  )
			returning subject.incident_id, subject.source_kind, subject.tenant_id`, activeJSON, now)
		if err != nil {
			return fmt.Errorf("recover continuous operational subjects: %w", err)
		}
		for rows.Next() {
			var item recoveredOperationalSubject
			if err := rows.Scan(&item.incidentID, &item.sourceKind, &item.tenantID); err != nil {
				rows.Close()
				return err
			}
			recovered = append(recovered, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	rows, err := tx.Query(ctx, `
		update operational_incident_subjects subject
		set status = 'RECOVERED', recovered_at = $1
		from operational_incidents incident
		where incident.id = subject.incident_id and incident.status in ('OPEN', 'ACKNOWLEDGED')
		  and subject.status = 'ACTIVE' and subject.observation_mode = 'DISCRETE'
		  and subject.tenant_id is not null and subject.last_failure_at is not null
		  and (
		    (subject.source_kind in ('REPORT', 'NOTIFICATION', 'SML_CIRCUIT') and exists (
		      select 1 from notification_runs recovered
		      where recovered.tenant_id = subject.tenant_id and recovered.trigger_kind = 'SCHEDULED'
		        and recovered.status = 'COMPLETED' and recovered.updated_at > subject.last_failure_at
		    ))
		    or (subject.source_kind = 'DELIVERY' and exists (
		      select 1 from line_deliveries recovered
		      where recovered.tenant_id = subject.tenant_id and recovered.status = 'ACCEPTED'
		        and recovered.updated_at > subject.last_failure_at
		    ))
		  )
		returning subject.incident_id, subject.source_kind, subject.tenant_id`, now)
	if err != nil {
		return fmt.Errorf("recover discrete operational subjects: %w", err)
	}
	for rows.Next() {
		var item recoveredOperationalSubject
		if err := rows.Scan(&item.incidentID, &item.sourceKind, &item.tenantID); err != nil {
			rows.Close()
			return err
		}
		recovered = append(recovered, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if len(recovered) > 0 {
		for _, item := range recovered {
			if _, err := tx.Exec(ctx, `
				insert into operational_incident_events (incident_id, event_kind, source_kind, tenant_id, observed_at)
				values ($1, 'SUBJECT_RECOVERED', $2, $3, $4)`, item.incidentID, item.sourceKind, item.tenantID, now); err != nil {
				return fmt.Errorf("record operational subject recovery: %w", err)
			}
		}
		incidentIDs := make(map[uuid.UUID]struct{})
		for _, item := range recovered {
			incidentIDs[item.incidentID] = struct{}{}
		}
		if _, err := tx.Exec(ctx, `
			update operational_incidents incident
			set active_affected_count = counts.active_count,
			    updated_at = $2, version = version + 1
			from (
			  select subject.incident_id, count(*) filter (where subject.status = 'ACTIVE')::integer active_count
			  from operational_incident_subjects subject where subject.incident_id = any($1::uuid[])
			  group by subject.incident_id
			) counts where incident.id = counts.incident_id`, mapKeys(incidentIDs), now); err != nil {
			return fmt.Errorf("reconcile active operational subjects: %w", err)
		}
	}

	resolvedRows, err := tx.Query(ctx, `
		update operational_incidents incident
		set status = 'RESOLVED', resolved_at = $1, reminder_due_at = null,
		    active_affected_count = 0, version = version + 1, updated_at = $1
		where incident.status in ('OPEN', 'ACKNOWLEDGED')
		  and exists (select 1 from operational_incident_subjects subject where subject.incident_id = incident.id)
		  and not exists (select 1 from operational_incident_subjects subject where subject.incident_id = incident.id and subject.status = 'ACTIVE')
		returning incident.id, incident.severity`, now)
	if err != nil {
		return fmt.Errorf("resolve operational incidents from subject evidence: %w", err)
	}
	type resolvedIncident struct {
		id       uuid.UUID
		severity string
	}
	resolved := make([]resolvedIncident, 0)
	for resolvedRows.Next() {
		var item resolvedIncident
		if err := resolvedRows.Scan(&item.id, &item.severity); err != nil {
			resolvedRows.Close()
			return err
		}
		resolved = append(resolved, item)
	}
	if err := resolvedRows.Err(); err != nil {
		resolvedRows.Close()
		return err
	}
	resolvedRows.Close()
	for _, item := range resolved {
		if _, err := tx.Exec(ctx, `insert into operational_incident_events (incident_id, event_kind, observed_at) values ($1, 'EVIDENCE_RESOLVED', $2)`, item.id, now); err != nil {
			return fmt.Errorf("record operational recovery evidence: %w", err)
		}
		if enqueue && item.severity == string(sentinel.SeverityP1) {
			if _, err := tx.Exec(ctx, `
				insert into operational_alert_outbox (incident_id, alert_kind, available_at, created_at, updated_at)
				select $1, 'RECOVERY', $2, $2, $2
				where exists (select 1 from operational_alert_outbox where incident_id = $1 and alert_kind = 'OPEN' and status = 'SENT')
				on conflict (incident_id, alert_kind) do nothing`, item.id, now); err != nil {
				return fmt.Errorf("enqueue operational recovery alert: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit operational subject lifecycle: %w", err)
	}
	return nil
}
