package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListIncidentOccurrences is intentionally a single bounded query. URLs are
// returned only from this authenticated Admin detail endpoint and are further
// sanitized by sentinel.AdminService before serialization.
func (store *SentinelStore) ListIncidentOccurrences(ctx context.Context, incidentID uuid.UUID, filter sentinel.OccurrenceFilter) (sentinel.OccurrencePage, error) {
	var exists bool
	if err := store.pool.QueryRow(ctx, `select exists(select 1 from operational_incidents where id = $1)`, incidentID).Scan(&exists); err != nil {
		return sentinel.OccurrencePage{}, fmt.Errorf("inspect operational incident: %w", err)
	}
	if !exists {
		return sentinel.OccurrencePage{}, sentinel.ErrNotFound
	}
	cursorTime, cursorID, err := operationsCursor(filter.Cursor)
	if err != nil {
		return sentinel.OccurrencePage{}, sentinel.ErrInvalidInput
	}
	rows, err := store.pool.Query(ctx, `
		select event.id, coalesce(event.tenant_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       coalesce(tenant.name, ''), coalesce(event.report_key, ''), coalesce(event.source_kind, ''),
		       coalesce(event.safe_error_code, ''), event.observed_at,
		       event.failure_evidence_version, event.failure_level, event.failure_category, event.failure_stage,
		       event.failure_transport_phase, event.failure_occurred_at, event.failure_duration_ms,
		       event.failure_attempt, event.failure_retryable, event.failure_remote_state_unknown,
		       event.connection_version, event.failure_protocol_evidence, event.reports_total, event.reports_succeeded, event.reports_failed,
		       event.reports_cancelled, event.notification_outcome,
		       coalesce(history.after_json->>'endpointUrl', ''), coalesce(current.endpoint_url, ''), current.version,
		       test.cooldown_until
		from operational_incident_events event
		left join tenants tenant on tenant.id = event.tenant_id
		left join tenant_sml_connections current on current.tenant_id = event.tenant_id
		left join lateral (
		  select audit.after_json from audit_logs audit
		  where audit.tenant_id = event.tenant_id and audit.action = 'SML_CONNECTION_REPLACED'
		    and event.connection_version is not null
		    and audit.after_json->>'version' = event.connection_version::text
		  order by audit.created_at desc limit 1
		) history on true
		left join sml_connection_tests test on test.tenant_id = event.tenant_id
		where event.incident_id = $1 and event.event_kind in ('OBSERVED', 'CONDITION_UPDATED')
		  and not event.downstream
		  and ($2::timestamptz is null or (event.observed_at, event.id) < ($2, $3))
		order by event.observed_at desc, event.id desc limit $4`, incidentID, cursorTime, cursorID, filter.PageSize+1)
	if err != nil {
		return sentinel.OccurrencePage{}, fmt.Errorf("list operational incident occurrences: %w", err)
	}
	defer rows.Close()
	items := make([]sentinel.IncidentOccurrence, 0, filter.PageSize+1)
	for rows.Next() {
		var item sentinel.IncidentOccurrence
		var evidenceVersion, attempt, connectionVersion, currentVersion *int
		var level, category, stage, transport *string
		var occurredAt *time.Time
		var duration *int64
		var retryable, remoteUnknown *bool
		var total, succeeded, failed, cancelled *int
		var outcome *string
		var historicalURL, currentURL string
		var cooldown *time.Time
		var protocolJSON []byte
		if err := rows.Scan(&item.ID, &item.TenantID, &item.TenantName, &item.ReportKey, &item.SourceKind,
			&item.SafeErrorCode, &item.ObservedAt, &evidenceVersion, &level, &category, &stage, &transport,
			&occurredAt, &duration, &attempt, &retryable, &remoteUnknown, &connectionVersion, &protocolJSON,
			&total, &succeeded, &failed, &cancelled, &outcome, &historicalURL, &currentURL, &currentVersion, &cooldown); err != nil {
			return sentinel.OccurrencePage{}, fmt.Errorf("scan operational incident occurrence: %w", err)
		}
		if level != nil && category != nil && stage != nil && occurredAt != nil && retryable != nil && remoteUnknown != nil {
			evidence := failure.Evidence{Level: failure.EvidenceLevel(*level), Category: failure.Category(*category), Stage: failure.Stage(*stage), OccurredAt: *occurredAt, DurationMS: duration, Attempt: attempt, Retryable: *retryable, RemoteStateUnknown: *remoteUnknown, ConnectionVersion: connectionVersion, SafeErrorCode: item.SafeErrorCode}
			if evidenceVersion != nil {
				evidence.Version = *evidenceVersion
			}
			if transport != nil {
				evidence.TransportPhase = failure.TransportPhase(*transport)
			}
			if len(protocolJSON) > 0 {
				var protocol failure.JavaWSProtocolEvidence
				if err := json.Unmarshal(protocolJSON, &protocol); err != nil {
					return sentinel.OccurrencePage{}, fmt.Errorf("decode occurrence protocol evidence: %w", err)
				}
				evidence.ProtocolEvidence = &protocol
			}
			item.FailureEvidence = &evidence
		}
		if total != nil && succeeded != nil && failed != nil && cancelled != nil {
			impact := failure.Impact{ReportsTotal: *total, ReportsSucceeded: *succeeded, ReportsFailed: *failed, ReportsCancelled: *cancelled, Notification: failure.NotificationOutcomeUnknown}
			if outcome != nil {
				impact.Notification = failure.NotificationOutcome(*outcome)
			}
			item.Impact = &impact
		}
		if strings.HasPrefix(strings.ToUpper(item.SafeErrorCode), "SML_") || (category != nil && strings.HasPrefix(*category, "JAVA_WS")) {
			reference := sentinel.SMLConnectionReference{EndpointURLAtFailure: historicalURL, CurrentEndpointURL: currentURL, VersionAtFailure: connectionVersion, CurrentVersion: currentVersion, Status: sentinel.ConnectionUnavailable, TestAvailableAt: cooldown}
			switch {
			case historicalURL != "" && connectionVersion != nil && currentVersion != nil && *connectionVersion == *currentVersion:
				reference.Status = sentinel.ConnectionExactVersion
			case historicalURL != "":
				reference.Status = sentinel.ConnectionChanged
			case currentURL != "":
				reference.Status = sentinel.ConnectionCurrentOnly
			}
			item.ConnectionReference = &reference
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return sentinel.OccurrencePage{}, fmt.Errorf("iterate operational incident occurrences: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	next := ""
	if hasMore {
		last := items[len(items)-1]
		next = encodeTenantCursor(last.ObservedAt, last.ID)
	}
	return sentinel.OccurrencePage{Data: items, NextCursor: next, HasMore: hasMore}, nil
}
