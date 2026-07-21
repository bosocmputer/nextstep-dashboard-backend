package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type diagnosisMatchKey struct {
	runID             uuid.UUID
	tenantID          uuid.UUID
	reportKey         string
	resultKind        string
	queryFingerprint  string
	connectionVersion int
	periodFrom        time.Time
	periodTo          time.Time
	observedAt        time.Time
}

func (store *SentinelStore) GetOccurrenceDiagnosis(ctx context.Context, incidentID, occurrenceID uuid.UUID) (sentinel.DiagnosisRecord, error) {
	var record sentinel.DiagnosisRecord
	var key diagnosisMatchKey
	var sourceKind, safeCode string
	var evidenceVersion, attempt, connectionVersion, currentVersion *int
	var level, category, stage, transport *string
	var occurredAt *time.Time
	var duration *int64
	var retryable, remoteUnknown *bool
	var protocolJSON []byte
	var historicalURL, currentURL string
	var runID, runTenantID *uuid.UUID
	var periodFrom, periodTo *time.Time
	err := store.pool.QueryRow(ctx, `
		select coalesce(event.source_kind, ''), coalesce(event.safe_error_code, ''), event.observed_at,
		       event.failure_evidence_version, event.failure_level, event.failure_category, event.failure_stage,
		       event.failure_transport_phase, event.failure_occurred_at, event.failure_duration_ms,
		       event.failure_attempt, event.failure_retryable, event.failure_remote_state_unknown,
		       event.connection_version, event.failure_protocol_evidence,
		       coalesce(tenant.name, ''), coalesce(history.after_json->>'endpointUrl', ''),
		       coalesce(current.endpoint_url, ''), current.version,
		       run.id, coalesce(run.tenant_id, event.tenant_id), coalesce(run.report_key, ''),
		       coalesce(run.result_kind, ''), coalesce(run.query_plan_fingerprint, ''),
		       coalesce(run.data_source_version, event.connection_version, 0), run.period_from, run.period_to
		from operational_incident_events event
		left join tenants tenant on tenant.id = event.tenant_id
		left join report_runs run on event.source_kind = 'REPORT' and run.id = event.source_id
		left join tenant_sml_connections current on current.tenant_id = event.tenant_id
		left join lateral (
		  select audit.after_json from audit_logs audit
		  where audit.tenant_id = event.tenant_id and audit.action = 'SML_CONNECTION_REPLACED'
		    and event.connection_version is not null
		    and audit.after_json->>'version' = event.connection_version::text
		  order by audit.created_at desc limit 1
		) history on true
		where event.incident_id = $1 and event.id = $2 and not event.downstream`, incidentID, occurrenceID).Scan(
		&sourceKind, &safeCode, &key.observedAt, &evidenceVersion, &level, &category, &stage, &transport,
		&occurredAt, &duration, &attempt, &retryable, &remoteUnknown, &connectionVersion, &protocolJSON,
		&record.TenantName, &historicalURL, &currentURL, &currentVersion, &runID, &runTenantID,
		&key.reportKey, &key.resultKind, &key.queryFingerprint, &key.connectionVersion, &periodFrom, &periodTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return sentinel.DiagnosisRecord{}, sentinel.ErrNotFound
	}
	if err != nil {
		return sentinel.DiagnosisRecord{}, fmt.Errorf("load incident diagnosis evidence: %w", err)
	}
	evidence := failure.EvidenceForCode(safeCode)
	evidence.Version, evidence.Level, evidence.OccurredAt = 0, failure.LevelLegacyPartial, key.observedAt
	if evidenceVersion != nil && level != nil && category != nil && stage != nil && occurredAt != nil && retryable != nil && remoteUnknown != nil {
		evidence.Version, evidence.Level = *evidenceVersion, failure.EvidenceLevel(*level)
		evidence.Category, evidence.Stage, evidence.OccurredAt = failure.Category(*category), failure.Stage(*stage), *occurredAt
		evidence.DurationMS, evidence.Attempt, evidence.Retryable, evidence.RemoteStateUnknown = duration, attempt, *retryable, *remoteUnknown
		evidence.ConnectionVersion = connectionVersion
		if transport != nil {
			evidence.TransportPhase = failure.TransportPhase(*transport)
		}
		if len(protocolJSON) > 0 {
			var protocol failure.JavaWSProtocolEvidence
			if err := json.Unmarshal(protocolJSON, &protocol); err != nil {
				return sentinel.DiagnosisRecord{}, fmt.Errorf("decode incident diagnosis protocol evidence: %w", err)
			}
			evidence.ProtocolEvidence = &protocol
		}
	}
	record.Evidence = failure.Complete(evidence)
	reference := sentinel.SMLConnectionReference{EndpointURLAtFailure: historicalURL, CurrentEndpointURL: currentURL, VersionAtFailure: connectionVersion, CurrentVersion: currentVersion, Status: sentinel.ConnectionUnavailable}
	switch {
	case historicalURL != "" && connectionVersion != nil && currentVersion != nil && *connectionVersion == *currentVersion:
		reference.Status = sentinel.ConnectionExactVersion
	case historicalURL != "":
		reference.Status = sentinel.ConnectionChanged
	case currentURL != "":
		reference.Status = sentinel.ConnectionCurrentOnly
	}
	if historicalURL != "" || currentURL != "" {
		record.ConnectionReference = &reference
	}
	if sourceKind != string(sentinel.SourceReport) || runID == nil || runTenantID == nil || periodFrom == nil || periodTo == nil || key.queryFingerprint == "" || key.reportKey == "" || key.resultKind == "" || key.connectionVersion <= 0 {
		return record, nil
	}
	key.runID, key.tenantID, key.periodFrom, key.periodTo = *runID, *runTenantID, *periodFrom, *periodTo
	if err := store.loadMatchingSuccesses(ctx, key, &record); err != nil {
		return sentinel.DiagnosisRecord{}, err
	}
	if err := store.loadDiagnosisBaseline(ctx, key, &record); err != nil {
		return sentinel.DiagnosisRecord{}, err
	}
	return record, nil
}

func (store *SentinelStore) loadMatchingSuccesses(ctx context.Context, key diagnosisMatchKey, record *sentinel.DiagnosisRecord) error {
	var priorAt, subsequentAt *time.Time
	var priorDuration, subsequentDuration *int64
	err := store.pool.QueryRow(ctx, `
		select prior.finished_at, prior.duration_ms, subsequent.finished_at, subsequent.duration_ms
		from (values (true)) seed(value)
		left join lateral (
		  select candidate.finished_at,
		         greatest(0, extract(epoch from (coalesce(candidate.source_finished_at, candidate.finished_at) - coalesce(candidate.source_started_at, candidate.started_at, candidate.queued_at))) * 1000)::bigint duration_ms
		  from report_runs candidate
		  where candidate.id <> $1 and candidate.tenant_id = $2 and candidate.report_key = $3
		    and candidate.result_kind = $4 and candidate.query_plan_fingerprint = $5
		    and candidate.data_source_version = $6 and candidate.period_from = $7 and candidate.period_to = $8
		    and candidate.status = 'SUCCEEDED' and candidate.finished_at < $9
		  order by candidate.finished_at desc, candidate.id desc limit 1
		) prior on true
		left join lateral (
		  select candidate.finished_at,
		         greatest(0, extract(epoch from (coalesce(candidate.source_finished_at, candidate.finished_at) - coalesce(candidate.source_started_at, candidate.started_at, candidate.queued_at))) * 1000)::bigint duration_ms
		  from report_runs candidate
		  where candidate.id <> $1 and candidate.tenant_id = $2 and candidate.report_key = $3
		    and candidate.result_kind = $4 and candidate.query_plan_fingerprint = $5
		    and candidate.data_source_version = $6 and candidate.period_from = $7 and candidate.period_to = $8
		    and candidate.status = 'SUCCEEDED' and candidate.finished_at > $9
		  order by candidate.finished_at, candidate.id limit 1
		) subsequent on true`, key.runID, key.tenantID, key.reportKey, key.resultKind, key.queryFingerprint, key.connectionVersion, key.periodFrom, key.periodTo, key.observedAt).Scan(&priorAt, &priorDuration, &subsequentAt, &subsequentDuration)
	if err != nil {
		return fmt.Errorf("load matching report successes: %w", err)
	}
	if priorAt != nil && priorDuration != nil {
		record.PriorMatchingSuccess = &sentinel.MatchingSuccess{FinishedAt: *priorAt, DurationMS: *priorDuration}
	}
	if subsequentAt != nil && subsequentDuration != nil {
		record.SubsequentMatchingSuccess = &sentinel.MatchingSuccess{FinishedAt: *subsequentAt, DurationMS: *subsequentDuration}
	}
	return nil
}

func (store *SentinelStore) loadDiagnosisBaseline(ctx context.Context, key diagnosisMatchKey, record *sentinel.DiagnosisRecord) error {
	var p50, p90 float64
	err := store.pool.QueryRow(ctx, `
		select coalesce(percentile_cont(0.5) within group (order by sample.duration_ms), 0),
		       coalesce(percentile_cont(0.9) within group (order by sample.duration_ms), 0), count(*)::integer
		from (
		  select greatest(0, extract(epoch from (coalesce(candidate.source_finished_at, candidate.finished_at) - coalesce(candidate.source_started_at, candidate.started_at, candidate.queued_at))) * 1000)::bigint duration_ms
		  from report_runs candidate
		  where candidate.id <> $1 and candidate.tenant_id = $2 and candidate.report_key = $3
		    and candidate.result_kind = $4 and candidate.query_plan_fingerprint = $5
		    and candidate.data_source_version = $6 and candidate.period_from = $7 and candidate.period_to = $8
		    and candidate.status = 'SUCCEEDED'
		    and candidate.finished_at is not null
		  order by candidate.finished_at desc, candidate.id desc limit 50
		) sample`, key.runID, key.tenantID, key.reportKey, key.resultKind, key.queryFingerprint, key.connectionVersion, key.periodFrom, key.periodTo).Scan(&p50, &p90, &record.Baseline.SampleCount)
	if err != nil {
		return fmt.Errorf("load matching report baseline: %w", err)
	}
	record.Baseline.P50MS, record.Baseline.P90MS = int64(math.Round(p50)), int64(math.Round(p90))
	return nil
}
