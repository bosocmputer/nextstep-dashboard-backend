package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type publishedGeneration struct {
	ID                uuid.UUID
	Key               string
	Period            report.Period
	SourceConsistency report.SourceConsistency
	SourceStartedAt   *time.Time
	SourceFinishedAt  *time.Time
	PublishedAt       *time.Time
	Total             int
}

func (store *ReportStore) GetPublishedOverviewForPeriods(ctx context.Context, tenantID uuid.UUID, requests []viewer.SnapshotPeriodRequest, now time.Time) (viewer.ExecutiveOverview, error) {
	if !store.generationCacheEnabled || len(requests) == 0 {
		return viewer.ExecutiveOverview{}, report.ErrRunNotFound
	}
	var dataSourceVersion int
	if err := store.pool.QueryRow(ctx, `select coalesce((select version from tenant_sml_connections where tenant_id = $1), 0)`, tenantID).Scan(&dataSourceVersion); err != nil {
		return viewer.ExecutiveOverview{}, fmt.Errorf("load published generation data source version: %w", err)
	}
	inputs := make([]report.EnqueueInput, 0, len(requests))
	for _, request := range requests {
		inputs = append(inputs, report.EnqueueInput{TenantID: tenantID, ReportKey: request.ReportKey, Period: request.Period, ResultKind: report.ResultSummary})
	}
	descriptor, err := describeDashboardGeneration(tenantID, inputs, dataSourceVersion)
	if err != nil {
		return viewer.ExecutiveOverview{}, err
	}
	generation, err := store.scanPublishedGeneration(store.pool.QueryRow(ctx, `
		select generation.id, generation.generation_key, generation.period_preset,
		       coalesce(generation.period_from::text, ''), coalesce(generation.period_to::text, ''),
		       generation.source_consistency, generation.source_started_at,
		       generation.source_finished_at, generation.published_at, generation.total
		from dashboard_generation_heads head
		join dashboard_generations generation on generation.id = head.published_generation_id
		where head.tenant_id = $1 and head.generation_key = $2
		  and generation.status = 'PUBLISHED' and generation.data_source_version = $3`, tenantID, descriptor.GenerationKey, dataSourceVersion))
	if err != nil {
		return viewer.ExecutiveOverview{}, err
	}
	return store.loadPublishedGeneration(ctx, tenantID, generation, now)
}

func (store *ReportStore) GetLatestPublishedOverview(ctx context.Context, tenantID uuid.UUID, reportKeys []report.Key, now time.Time) (viewer.ExecutiveOverview, error) {
	if !store.generationCacheEnabled || len(reportKeys) == 0 {
		return viewer.ExecutiveOverview{}, report.ErrRunNotFound
	}
	keys := make([]string, len(reportKeys))
	for index, key := range reportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return viewer.ExecutiveOverview{}, report.ErrRunNotFound
		}
		keys[index] = string(key)
	}
	sort.Strings(keys)
	generation, err := store.scanPublishedGeneration(store.pool.QueryRow(ctx, `
		select generation.id, generation.generation_key, generation.period_preset,
		       coalesce(generation.period_from::text, ''), coalesce(generation.period_to::text, ''),
		       generation.source_consistency, generation.source_started_at,
		       generation.source_finished_at, generation.published_at, generation.total
		from dashboard_generation_heads head
		join dashboard_generations generation on generation.id = head.published_generation_id
		join tenant_sml_connections connection
		  on connection.tenant_id = generation.tenant_id and connection.version = generation.data_source_version
		where head.tenant_id = $1 and generation.status = 'PUBLISHED' and generation.total = $2
		  and (select array_agg(link.report_key order by link.report_key)
		       from dashboard_generation_reports link where link.generation_id = generation.id) = $3::text[]
		order by generation.published_at desc, generation.id desc limit 1`, tenantID, len(keys), keys))
	if err != nil {
		return viewer.ExecutiveOverview{}, err
	}
	return store.loadPublishedGeneration(ctx, tenantID, generation, now)
}

func (store *ReportStore) scanPublishedGeneration(row rowScanner) (publishedGeneration, error) {
	var generation publishedGeneration
	if err := row.Scan(&generation.ID, &generation.Key, &generation.Period.Preset,
		&generation.Period.DateFrom, &generation.Period.DateTo, &generation.SourceConsistency,
		&generation.SourceStartedAt, &generation.SourceFinishedAt, &generation.PublishedAt, &generation.Total); errors.Is(err, pgx.ErrNoRows) {
		return publishedGeneration{}, report.ErrRunNotFound
	} else if err != nil {
		return publishedGeneration{}, fmt.Errorf("scan published dashboard generation: %w", err)
	}
	return generation, nil
}

func (store *ReportStore) loadPublishedGeneration(ctx context.Context, tenantID uuid.UUID, generation publishedGeneration, now time.Time) (viewer.ExecutiveOverview, error) {
	policy, err := NewRefreshPolicyStore(store.pool).GetRefreshPolicy(ctx, tenantID)
	if err != nil {
		return viewer.ExecutiveOverview{}, err
	}
	rows, err := store.pool.Query(ctx, `
		select run.id, run.report_key, run.dashboard_json,
		       run.period_from::text, run.period_to::text,
		       coalesce(run.source_started_at, run.started_at, run.queued_at),
		       coalesce(run.source_finished_at, run.finished_at),
		       run.report_definition_version, run.data_source_version,
		       run.query_plan_fingerprint, run.source_consistency, run.result_kind, run.expires_at
		from dashboard_generation_reports link
		join report_runs run on run.id = link.report_run_id and run.tenant_id = link.tenant_id
		join report_definitions definition
		  on definition.report_key = run.report_key and definition.version = run.report_definition_version
		join tenant_sml_connections connection
		  on connection.tenant_id = run.tenant_id and connection.version = run.data_source_version
		where link.generation_id = $1 and link.tenant_id = $2
		  and run.status = 'SUCCEEDED' and run.result_kind = 'SUMMARY'
		order by link.position`, generation.ID, tenantID)
	if err != nil {
		return viewer.ExecutiveOverview{}, fmt.Errorf("load published dashboard generation: %w", err)
	}
	defer rows.Close()
	items := make([]viewer.DashboardSnapshot, 0, generation.Total)
	dataStatus := viewer.FreshnessFresh
	for rows.Next() {
		var snapshot viewer.DashboardSnapshot
		var dashboardJSON []byte
		var reportKey report.Key
		var resultKind report.ResultKind
		var expiresAt time.Time
		if err := rows.Scan(&snapshot.RunID, &reportKey, &dashboardJSON,
			&snapshot.PeriodFrom, &snapshot.PeriodTo, &snapshot.SourceStartedAt, &snapshot.SourceFinishedAt,
			&snapshot.ReportDefinitionVersion, &snapshot.DataSourceVersion, &snapshot.QueryPlanFingerprint,
			&snapshot.SourceConsistency, &resultKind, &expiresAt); err != nil {
			return viewer.ExecutiveOverview{}, fmt.Errorf("scan published dashboard snapshot: %w", err)
		}
		if snapshot.QueryPlanFingerprint != report.QueryPlanFingerprint(reportKey, report.ResultSummary) {
			return viewer.ExecutiveOverview{}, report.ErrRunNotFound
		}
		if err := json.Unmarshal(dashboardJSON, &snapshot.Dashboard); err != nil || snapshot.Dashboard.ReportKey != reportKey {
			return viewer.ExecutiveOverview{}, report.ErrRunNotFound
		}
		definition, ok := report.DefinitionFor(reportKey)
		if !ok {
			return viewer.ExecutiveOverview{}, report.ErrRunNotFound
		}
		period := report.Period{Preset: snapshot.Dashboard.Period.Preset, DateFrom: snapshot.PeriodFrom, DateTo: snapshot.PeriodTo}
		if err := finalizeSnapshot(&snapshot, resultKind, expiresAt, definition, period, policy, now); err != nil {
			return viewer.ExecutiveOverview{}, report.ErrRunNotFound
		}
		if snapshot.FreshnessStatus == viewer.FreshnessExpired {
			dataStatus = viewer.FreshnessExpired
		} else if snapshot.FreshnessStatus == viewer.FreshnessStale && dataStatus == viewer.FreshnessFresh {
			dataStatus = viewer.FreshnessStale
		}
		items = append(items, snapshot)
	}
	if err := rows.Err(); err != nil {
		return viewer.ExecutiveOverview{}, fmt.Errorf("iterate published dashboard generation: %w", err)
	}
	if len(items) != generation.Total {
		return viewer.ExecutiveOverview{}, report.ErrRunNotFound
	}
	if dataStatus == viewer.FreshnessExpired {
		items = []viewer.DashboardSnapshot{}
	}
	requestedPeriod := generation.Period
	return viewer.ExecutiveOverview{
		TenantID: tenantID, GenerationID: &generation.ID, GenerationKey: generation.Key,
		RequestedPeriod: &requestedPeriod, DataStatus: dataStatus,
		SourceConsistency: generation.SourceConsistency, SourceStartedAt: generation.SourceStartedAt,
		SourceFinishedAt: generation.SourceFinishedAt, PublishedAt: generation.PublishedAt, Items: items,
	}, nil
}
