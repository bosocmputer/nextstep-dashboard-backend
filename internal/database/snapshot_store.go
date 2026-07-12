package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	backgroundScheduleGuard     = 5 * time.Minute
	currentSnapshotHardStale    = 24 * time.Hour
	historicalSnapshotTTL       = 24 * time.Hour
	historicalSnapshotRetention = 90 * 24 * time.Hour
)

func (store *ReportStore) RevalidateSnapshot(ctx context.Context, tenantID uuid.UUID, reportKey report.Key, period report.Period, now time.Time) (viewer.ReportRevalidation, error) {
	definition, ok := report.DefinitionFor(reportKey)
	if !ok {
		return viewer.ReportRevalidation{}, report.ErrRunNotFound
	}
	policy, err := NewRefreshPolicyStore(store.pool).GetRefreshPolicy(ctx, tenantID)
	if err != nil {
		return viewer.ReportRevalidation{}, err
	}
	interval, enabled := policy.IntervalFor(definition)
	var dataSourceVersion int
	if err := store.pool.QueryRow(ctx, `select version from tenant_sml_connections where tenant_id = $1`, tenantID).Scan(&dataSourceVersion); err != nil {
		return viewer.ReportRevalidation{}, report.ErrRunNotFound
	}
	snapshot, err := store.GetExactSnapshot(ctx, tenantID, reportKey, period, policy, now)
	if err != nil && !errors.Is(err, report.ErrRunNotFound) {
		return viewer.ReportRevalidation{}, err
	}
	if err == nil && snapshot.FreshnessStatus == viewer.FreshnessFresh {
		return viewer.ReportRevalidation{Disposition: viewer.RevalidationFreshCache, Snapshot: &snapshot}, nil
	}
	if !enabled {
		return viewer.ReportRevalidation{Disposition: viewer.RevalidationDisabled, Snapshot: optionalSnapshot(snapshot, err)}, nil
	}
	var scheduleSoon bool
	if err := store.pool.QueryRow(ctx, `
		select exists (
		  select 1 from notification_schedules
		  where tenant_id = $1 and status = 'ACTIVE'
		    and next_run_at between $2 and $3
		)`, tenantID, now, now.Add(backgroundScheduleGuard)).Scan(&scheduleSoon); err != nil {
		return viewer.ReportRevalidation{}, fmt.Errorf("check dashboard refresh schedule guard: %w", err)
	}
	if scheduleSoon {
		return viewer.ReportRevalidation{Disposition: viewer.RevalidationDisabled, Snapshot: optionalSnapshot(snapshot, err), RetryAfter: int(backgroundScheduleGuard.Seconds())}, nil
	}
	var openUntil *time.Time
	if circuitErr := store.pool.QueryRow(ctx, `select open_until from tenant_sml_circuits where tenant_id = $1`, tenantID).Scan(&openUntil); circuitErr != nil && !errors.Is(circuitErr, pgx.ErrNoRows) {
		return viewer.ReportRevalidation{}, fmt.Errorf("read tenant SML circuit: %w", circuitErr)
	}
	if openUntil != nil && openUntil.After(now) {
		return viewer.ReportRevalidation{Disposition: viewer.RevalidationCircuitOpen, Snapshot: optionalSnapshot(snapshot, err), RetryAfter: max(1, int(openUntil.Sub(now).Seconds()))}, nil
	}
	executionKey := snapshotExecutionKey(tenantID, reportKey, period, definition.ParameterKind, definition.Version, dataSourceVersion)
	bucket := now.Unix() / max(1, int64(interval.Seconds()))
	idempotencyKey := "background-" + executionKey[:24] + fmt.Sprintf("-%d", bucket)
	run, enqueueErr := store.Enqueue(ctx, report.EnqueueInput{
		TenantID: tenantID, ReportKey: reportKey, Source: report.SourceBackground,
		ResultKind: report.ResultSummary, Priority: 20, ExecutionKey: executionKey,
		IdempotencyKey: idempotencyKey, Period: period,
	}, now)
	if enqueueErr != nil {
		return viewer.ReportRevalidation{}, enqueueErr
	}
	disposition := viewer.RevalidationMissingRefreshing
	activeRun := run.Status == report.StatusQueued || run.Status == report.StatusClaimed || run.Status == report.StatusRunning
	if err == nil {
		if activeRun {
			disposition = viewer.RevalidationStaleRefreshing
			snapshot.FreshnessStatus = viewer.FreshnessRefreshing
		} else {
			disposition = viewer.RevalidationJoined
			snapshot.FreshnessStatus = viewer.FreshnessFailed
		}
	} else if !activeRun {
		disposition = viewer.RevalidationJoined
	}
	return viewer.ReportRevalidation{Disposition: disposition, Snapshot: optionalSnapshot(snapshot, err), Run: &run}, nil
}

func (store *ReportStore) GetExactSnapshotForPeriod(ctx context.Context, tenantID uuid.UUID, reportKey report.Key, period report.Period, now time.Time) (viewer.DashboardSnapshot, error) {
	snapshots, err := store.GetExactSnapshotsForPeriods(ctx, tenantID, []viewer.SnapshotPeriodRequest{{ReportKey: reportKey, Period: period}}, now)
	if err != nil {
		return viewer.DashboardSnapshot{}, err
	}
	snapshot, found := snapshots[reportKey]
	if !found {
		return viewer.DashboardSnapshot{}, report.ErrRunNotFound
	}
	return snapshot, nil
}

func (store *ReportStore) GetExactSnapshotsForPeriods(ctx context.Context, tenantID uuid.UUID, requests []viewer.SnapshotPeriodRequest, now time.Time) (map[report.Key]viewer.DashboardSnapshot, error) {
	result := make(map[report.Key]viewer.DashboardSnapshot, len(requests))
	if len(requests) == 0 {
		return result, nil
	}
	policy, err := NewRefreshPolicyStore(store.pool).GetRefreshPolicy(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	reportKeys := make([]string, 0, len(requests))
	dateFrom := make([]string, 0, len(requests))
	dateTo := make([]string, 0, len(requests))
	periods := make(map[report.Key]report.Period, len(requests))
	for _, request := range requests {
		if _, ok := report.DefinitionFor(request.ReportKey); !ok {
			return nil, report.ErrRunNotFound
		}
		reportKeys = append(reportKeys, string(request.ReportKey))
		dateFrom = append(dateFrom, request.Period.DateFrom)
		dateTo = append(dateTo, request.Period.DateTo)
		periods[request.ReportKey] = request.Period
	}
	rows, err := store.pool.Query(ctx, `
		with requested as (
		  select report_key, period_from, period_to, ordinal
		  from unnest($2::text[], $3::date[], $4::date[]) with ordinality
		       as input(report_key, period_from, period_to, ordinal)
		), latest as (
		  select distinct on (requested.ordinal)
		         requested.ordinal, requested.report_key, r.id, r.dashboard_json,
		         r.period_from::text, r.period_to::text, r.started_at, r.finished_at,
		         r.report_definition_version, r.data_source_version, r.result_kind, r.expires_at
		  from requested
		  join report_runs r on r.tenant_id = $1 and r.report_key = requested.report_key
		    and r.period_from = requested.period_from and r.period_to = requested.period_to
		  join report_definitions d on d.report_key = r.report_key and d.version = r.report_definition_version
		  join tenant_sml_connections c on c.tenant_id = r.tenant_id and c.version = r.data_source_version
		  where r.status = 'SUCCEEDED' and r.dashboard_json <> '{}'::jsonb
		  order by requested.ordinal, r.finished_at desc nulls last, r.id desc
		)
		select report_key, id, dashboard_json, period_from, period_to, started_at, finished_at,
		       report_definition_version, data_source_version, result_kind, expires_at
		from latest order by ordinal`, tenantID, reportKeys, dateFrom, dateTo)
	if err != nil {
		return nil, fmt.Errorf("get exact dashboard snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var reportKey report.Key
		var snapshot viewer.DashboardSnapshot
		var dashboardJSON []byte
		var resultKind report.ResultKind
		var expiresAt time.Time
		if err := rows.Scan(&reportKey, &snapshot.RunID, &dashboardJSON, &snapshot.PeriodFrom, &snapshot.PeriodTo,
			&snapshot.SourceStartedAt, &snapshot.SourceFinishedAt, &snapshot.ReportDefinitionVersion,
			&snapshot.DataSourceVersion, &resultKind, &expiresAt); err != nil {
			return nil, fmt.Errorf("scan exact dashboard snapshot: %w", err)
		}
		if err := json.Unmarshal(dashboardJSON, &snapshot.Dashboard); err != nil {
			return nil, fmt.Errorf("decode exact dashboard snapshot: %w", err)
		}
		definition, _ := report.DefinitionFor(reportKey)
		period := periods[reportKey]
		if err := finalizeSnapshot(&snapshot, resultKind, expiresAt, definition, period, policy, now); err != nil {
			continue
		}
		result[reportKey] = snapshot
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate exact dashboard snapshots: %w", err)
	}
	return result, nil
}

func (store *ReportStore) GetExactSnapshot(ctx context.Context, tenantID uuid.UUID, reportKey report.Key, period report.Period, policy report.RefreshPolicy, now time.Time) (viewer.DashboardSnapshot, error) {
	// Kept as an internal compatibility helper for callers that already loaded policy.
	// The public lookup path uses the batch method so an overview cache hit is one query.
	snapshot, err := store.getExactSnapshotWithPolicy(ctx, tenantID, reportKey, period, policy, now)
	return snapshot, err
}

func (store *ReportStore) getExactSnapshotWithPolicy(ctx context.Context, tenantID uuid.UUID, reportKey report.Key, period report.Period, policy report.RefreshPolicy, now time.Time) (viewer.DashboardSnapshot, error) {
	definition, ok := report.DefinitionFor(reportKey)
	if !ok {
		return viewer.DashboardSnapshot{}, report.ErrRunNotFound
	}
	var snapshot viewer.DashboardSnapshot
	var dashboardJSON []byte
	var resultKind report.ResultKind
	var expiresAt time.Time
	err := store.pool.QueryRow(ctx, `
		select r.id, r.dashboard_json, r.period_from::text, r.period_to::text,
		       r.started_at, r.finished_at, r.report_definition_version,
		       r.data_source_version, r.result_kind, r.expires_at
		from report_runs r
		join report_definitions d on d.report_key = r.report_key and d.version = r.report_definition_version
		join tenant_sml_connections c on c.tenant_id = r.tenant_id and c.version = r.data_source_version
		where r.tenant_id = $1 and r.report_key = $2
		  and r.period_from = $3::date and r.period_to = $4::date
		  and r.status = 'SUCCEEDED' and r.dashboard_json <> '{}'::jsonb
		order by r.finished_at desc nulls last, r.id desc
		limit 1`, tenantID, reportKey, period.DateFrom, period.DateTo).Scan(
		&snapshot.RunID, &dashboardJSON, &snapshot.PeriodFrom, &snapshot.PeriodTo,
		&snapshot.SourceStartedAt, &snapshot.SourceFinishedAt,
		&snapshot.ReportDefinitionVersion, &snapshot.DataSourceVersion,
		&resultKind, &expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return viewer.DashboardSnapshot{}, report.ErrRunNotFound
	}
	if err != nil {
		return viewer.DashboardSnapshot{}, fmt.Errorf("get exact dashboard snapshot: %w", err)
	}
	if err := json.Unmarshal(dashboardJSON, &snapshot.Dashboard); err != nil {
		return viewer.DashboardSnapshot{}, fmt.Errorf("decode exact dashboard snapshot: %w", err)
	}
	if err := finalizeSnapshot(&snapshot, resultKind, expiresAt, definition, period, policy, now); err != nil {
		return viewer.DashboardSnapshot{}, err
	}
	return snapshot, nil
}

func finalizeSnapshot(snapshot *viewer.DashboardSnapshot, resultKind report.ResultKind, expiresAt time.Time, definition report.Definition, period report.Period, policy report.RefreshPolicy, now time.Time) error {
	if snapshot.SourceFinishedAt == nil {
		return report.ErrRunNotFound
	}
	location, _ := time.LoadLocation("Asia/Bangkok")
	today := now.In(location).Format("2006-01-02")
	closedPeriod := period.DateTo < today
	refreshInterval, _ := policy.IntervalFor(definition)
	if closedPeriod {
		refreshInterval = historicalSnapshotTTL
	}
	freshUntil := snapshot.SourceFinishedAt.Add(refreshInterval)
	staleUntil := snapshot.SourceFinishedAt.Add(currentSnapshotHardStale)
	if closedPeriod {
		staleUntil = snapshot.SourceFinishedAt.Add(historicalSnapshotRetention)
	}
	snapshot.FreshUntil = &freshUntil
	snapshot.StaleUntil = &staleUntil
	snapshot.DetailsAvailable = resultKind == report.ResultDetail && expiresAt.After(now)
	if resultKind == report.ResultDetail {
		snapshot.DetailsExpiresAt = &expiresAt
	}
	switch {
	case now.Before(freshUntil):
		snapshot.FreshnessStatus = viewer.FreshnessFresh
	case now.Before(staleUntil):
		snapshot.FreshnessStatus = viewer.FreshnessStale
	default:
		snapshot.FreshnessStatus = viewer.FreshnessExpired
	}
	return nil
}

func snapshotExecutionKey(tenantID uuid.UUID, reportKey report.Key, period report.Period, periodMode report.ParameterKind, definitionVersion string, dataSourceVersion int) string {
	sum := sha256.Sum256([]byte(tenantID.String() + "\x00" + string(reportKey) + "\x00" + string(periodMode) + "\x00" + period.DateFrom + "\x00" + period.DateTo + "\x00" + definitionVersion + fmt.Sprintf("\x00%d\x00SUMMARY", dataSourceVersion)))
	return hex.EncodeToString(sum[:])
}

func optionalSnapshot(snapshot viewer.DashboardSnapshot, err error) *viewer.DashboardSnapshot {
	if err != nil || snapshot.RunID == uuid.Nil {
		return nil
	}
	return &snapshot
}
