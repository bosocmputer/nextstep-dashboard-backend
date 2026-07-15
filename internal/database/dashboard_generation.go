package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type dashboardGenerationDescriptor struct {
	GenerationKey           string
	ReportSetHash           string
	QueryPlanSetFingerprint string
	RequestJSON             []byte
	PeriodPreset            report.Preset
	PeriodFrom              string
	PeriodTo                string
}

func updateDashboardGenerationsForRun(ctx context.Context, tx pgx.Tx, runID uuid.UUID, now time.Time) error {
	rows, err := tx.Query(ctx, `
		with affected as (
		  select distinct generation_id from dashboard_generation_reports where report_run_id = $1
		), counts as (
		  select g.id,
		         count(*) filter (where r.status = 'SUCCEEDED')::integer as succeeded,
		         count(*) filter (where r.status in ('FAILED', 'CANCELLED', 'EXPIRED'))::integer as failed,
		         min(r.source_started_at) as source_started_at,
		         max(r.source_finished_at) as source_finished_at,
		         bool_or(r.source_consistency = 'CHUNK_WINDOW') as has_chunk_window
		  from dashboard_generations g
		  join affected a on a.generation_id = g.id
		  join dashboard_generation_reports link on link.generation_id = g.id
		  join report_runs r on r.id = link.report_run_id
		  where g.status = 'BUILDING'
		  group by g.id
		)
		update dashboard_generations g
		set completed = counts.succeeded,
		    failed = counts.failed,
		    source_started_at = counts.source_started_at,
		    source_finished_at = counts.source_finished_at,
		    source_consistency = case when counts.has_chunk_window then 'CHUNK_WINDOW' else 'SERIAL_WINDOW' end,
		    status = case when counts.failed > 0 then 'FAILED'
		                  when counts.succeeded = g.total then 'PUBLISHED' else 'BUILDING' end,
		    published_at = case when counts.failed = 0 and counts.succeeded = g.total then $2::timestamptz else null::timestamptz end,
		    finished_at = case when counts.failed > 0 or counts.succeeded = g.total then $2::timestamptz else null::timestamptz end,
		    updated_at = $2
		from counts where g.id = counts.id
		returning g.id, g.tenant_id, g.generation_key, g.status`, runID, now)
	if err != nil {
		return fmt.Errorf("update dashboard generations for run: %w", err)
	}
	defer rows.Close()
	type publishedGeneration struct {
		id, tenantID uuid.UUID
		key          string
		status       string
	}
	updated := make([]publishedGeneration, 0)
	for rows.Next() {
		var item publishedGeneration
		if err := rows.Scan(&item.id, &item.tenantID, &item.key, &item.status); err != nil {
			return fmt.Errorf("scan updated dashboard generation: %w", err)
		}
		updated = append(updated, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate updated dashboard generations: %w", err)
	}
	for _, item := range updated {
		if item.status != "PUBLISHED" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into dashboard_generation_heads (tenant_id, generation_key, published_generation_id, version, updated_at)
			values ($1, $2, $3, 1, $4)
			on conflict (tenant_id, generation_key) do update
			set published_generation_id = excluded.published_generation_id,
			    version = dashboard_generation_heads.version + 1,
			    updated_at = excluded.updated_at`, item.tenantID, item.key, item.id, now); err != nil {
			return fmt.Errorf("publish dashboard generation head: %w", err)
		}
	}
	return nil
}

type canonicalGenerationReport struct {
	ReportKey            report.Key    `json:"reportKey"`
	Period               report.Period `json:"period"`
	QueryPlanFingerprint string        `json:"queryPlanFingerprint"`
}

func describeDashboardGeneration(tenantID uuid.UUID, inputs []report.EnqueueInput, dataSourceVersion int) (dashboardGenerationDescriptor, error) {
	if tenantID == uuid.Nil || len(inputs) == 0 {
		return dashboardGenerationDescriptor{}, fmt.Errorf("dashboard generation input is incomplete")
	}
	items := make([]canonicalGenerationReport, 0, len(inputs))
	keys := make([]string, 0, len(inputs))
	queryFingerprints := make([]string, 0, len(inputs))
	periodFrom, periodTo := "", ""
	preset := inputs[0].Period.Preset
	for _, input := range inputs {
		fingerprint := report.QueryPlanFingerprint(input.ReportKey, report.ResultSummary)
		if fingerprint == "" || input.Period.DateFrom == "" || input.Period.DateTo == "" {
			return dashboardGenerationDescriptor{}, fmt.Errorf("dashboard generation report contract is incomplete")
		}
		items = append(items, canonicalGenerationReport{ReportKey: input.ReportKey, Period: input.Period, QueryPlanFingerprint: fingerprint})
		keys = append(keys, string(input.ReportKey))
		queryFingerprints = append(queryFingerprints, string(input.ReportKey)+":"+fingerprint)
		if periodFrom == "" || input.Period.DateFrom < periodFrom {
			periodFrom = input.Period.DateFrom
		}
		if periodTo == "" || input.Period.DateTo > periodTo {
			periodTo = input.Period.DateTo
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ReportKey < items[j].ReportKey })
	sort.Strings(keys)
	sort.Strings(queryFingerprints)
	requestJSON, err := json.Marshal(items)
	if err != nil {
		return dashboardGenerationDescriptor{}, fmt.Errorf("encode dashboard generation request: %w", err)
	}
	reportSetHash := sha256Hex(strings.Join(keys, "\x00"))
	querySetFingerprint := sha256Hex(strings.Join(queryFingerprints, "\x00"))
	generationKey := sha256Hex(tenantID.String() + "\x00" + string(requestJSON) + fmt.Sprintf("\x00%d\x00%s\x00%s", dataSourceVersion, reportSetHash, report.ResultSummary))
	return dashboardGenerationDescriptor{
		GenerationKey: generationKey, ReportSetHash: reportSetHash,
		QueryPlanSetFingerprint: querySetFingerprint, RequestJSON: requestJSON,
		PeriodPreset: preset, PeriodFrom: periodFrom, PeriodTo: periodTo,
	}, nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
