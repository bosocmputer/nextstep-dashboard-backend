package database

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (store *ScheduleStore) MaterializeTest(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, tenantID, scheduleID uuid.UUID, now time.Time) (schedule.Execution, error) {
	requestJSON, _ := json.Marshal(map[string]string{"tenantId": tenantID.String(), "scheduleId": scheduleID.String()})
	requestHash := sha256.Sum256(requestJSON)
	actorScope := "admin:test-schedule:" + tenantID.String() + ":" + hex.EncodeToString(actorHash)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("begin test schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		insert into idempotency_requests (actor_scope, idempotency_key, request_hash, expires_at)
		values ($1, $2, $3, $4)
		on conflict (actor_scope, idempotency_key) do nothing`, actorScope, idempotencyKey, requestHash[:], now.Add(24*time.Hour)); err != nil {
		return schedule.Execution{}, fmt.Errorf("reserve test schedule idempotency key: %w", err)
	}
	var storedHash, responseJSON []byte
	if err := tx.QueryRow(ctx, `
		select request_hash, response_json from idempotency_requests
		where actor_scope = $1 and idempotency_key = $2
		for update`, actorScope, idempotencyKey).Scan(&storedHash, &responseJSON); err != nil {
		return schedule.Execution{}, fmt.Errorf("read test schedule idempotency key: %w", err)
	}
	if subtle.ConstantTimeCompare(storedHash, requestHash[:]) != 1 {
		return schedule.Execution{}, schedule.ErrConflict
	}
	if len(responseJSON) > 0 {
		var replay schedule.Execution
		if err := json.Unmarshal(responseJSON, &replay); err != nil {
			return schedule.Execution{}, fmt.Errorf("decode test schedule replay: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return schedule.Execution{}, fmt.Errorf("commit test schedule replay: %w", err)
		}
		return replay, nil
	}

	item, err := getSchedule(ctx, tx, tenantID, scheduleID, true)
	if err != nil {
		return schedule.Execution{}, err
	}
	if item.Status == schedule.StatusExpired || item.Status == schedule.StatusArchived {
		return schedule.Execution{}, schedule.ErrStateConflict
	}
	readiness, err := scheduleReadiness(ctx, tx, tenantID, []uuid.UUID{scheduleID}, now)
	if err != nil {
		return schedule.Execution{}, err
	}
	if blockers := readiness[scheduleID]; len(blockers) > 0 {
		return schedule.Execution{}, &schedule.ReadinessError{Blockers: blockers}
	}
	location, err := time.LoadLocation(item.Timezone)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("load test schedule timezone: %w", err)
	}
	period, err := report.ResolvePeriod(item.PeriodPreset, location, now, nil, nil)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("resolve test report period: %w", err)
	}
	var scheduledFor time.Time
	if err := tx.QueryRow(ctx, `
		select greatest($2::timestamptz, coalesce(max(scheduled_for) + interval '1 microsecond', $2::timestamptz))
		from notification_runs where schedule_id = $1`, scheduleID, now).Scan(&scheduledFor); err != nil {
		return schedule.Execution{}, fmt.Errorf("reserve test schedule timestamp: %w", err)
	}
	execution := schedule.Execution{
		ID: uuid.New(), TenantID: tenantID, ScheduleID: scheduleID, ScheduledFor: scheduledFor,
		Status: schedule.ExecutionCollecting, ReportRunIDs: make([]uuid.UUID, 0, len(item.ReportKeys)),
	}
	if _, err := tx.Exec(ctx, `
		insert into notification_runs (
		  id, tenant_id, schedule_id, scheduled_for, status, next_attempt_at, created_at, updated_at,
		  materialization_version, trigger_kind
		) values ($1, $2, $3, $4, 'COLLECTING', $5, $5, $5, 2, 'TEST')`, execution.ID, tenantID, scheduleID, scheduledFor, now); err != nil {
		return schedule.Execution{}, fmt.Errorf("insert test notification run: %w", err)
	}
	idempotencyDigest := sha256.Sum256([]byte(idempotencyKey))
	actorDigest := sha256.Sum256(actorHash)
	for index, reportKey := range item.ReportKeys {
		effectivePeriod := period
		if definition, ok := report.DefinitionFor(reportKey); ok {
			effectivePeriod, err = store.periodPolicy.Resolve(tenantID, item.PeriodPreset, definition.ParameterKind, location, now)
			if err != nil {
				return schedule.Execution{}, fmt.Errorf("resolve test report %s period: %w", reportKey, err)
			}
		}
		paramsJSON, _ := json.Marshal(map[string]string{"dateFrom": effectivePeriod.DateFrom, "dateTo": effectivePeriod.DateTo})
		reportRunID := uuid.New()
		queryPlanFingerprint := report.QueryPlanFingerprint(reportKey, report.ResultSummary)
		reportIdempotencyKey := "schedule-test:" + scheduleID.String() + ":" + hex.EncodeToString(actorDigest[:8]) + ":" + hex.EncodeToString(idempotencyDigest[:16]) + ":" + string(reportKey)
		if _, err := tx.Exec(ctx, `
			insert into report_runs (
			  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
			  period_from, period_to, params_json, queued_at, expires_at, created_at, updated_at,
			  result_kind, priority, report_definition_version, data_source_version, progress_phase, progress_updated_at,
			  query_plan_fingerprint
			)
			select $1, $2, $3, 'SCHEDULE', $4, 'QUEUED', $5, $6::date, $7::date, $8, $9, $10, $9, $9,
			       'SUMMARY', 100, d.version, coalesce(c.version, 0), 'QUEUED', $9, $11
			from report_definitions d left join tenant_sml_connections c on c.tenant_id = $2
			where d.report_key = $3`,
			reportRunID, tenantID, reportKey, reportIdempotencyKey, effectivePeriod.Preset, effectivePeriod.DateFrom, effectivePeriod.DateTo,
			paramsJSON, now, now.AddDate(0, 3, 0), queryPlanFingerprint); err != nil {
			return schedule.Execution{}, fmt.Errorf("enqueue test report %s: %w", reportKey, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into notification_run_reports (notification_run_id, report_key, report_run_id, position)
			values ($1, $2, $3, $4)`, execution.ID, reportKey, reportRunID, index+1); err != nil {
			return schedule.Execution{}, fmt.Errorf("link test report %s: %w", reportKey, err)
		}
		execution.ReportRunIDs = append(execution.ReportRunIDs, reportRunID)
	}
	responseJSON, err = json.Marshal(execution)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("encode test schedule response: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update idempotency_requests set response_status = 202, response_json = $3
		where actor_scope = $1 and idempotency_key = $2`, actorScope, idempotencyKey, responseJSON); err != nil {
		return schedule.Execution{}, fmt.Errorf("complete test schedule idempotency key: %w", err)
	}
	afterJSON, _ := json.Marshal(map[string]any{
		"notificationRunId": execution.ID, "reportRunIds": execution.ReportRunIDs, "recipientCount": len(item.RecipientIDs),
	})
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SCHEDULE_TEST_SEND_ENQUEUED", "NOTIFICATION_SCHEDULE", scheduleID.String(), requestID, nil, afterJSON, now); err != nil {
		return schedule.Execution{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Execution{}, fmt.Errorf("commit test schedule: %w", err)
	}
	return execution, nil
}

func (store *ScheduleStore) MaterializeDue(ctx context.Context, workerID string, now time.Time) (schedule.Execution, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("begin materialize due schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var scheduledFor time.Time
	item, err := scanSchedule(tx.QueryRow(ctx, `
		select `+scheduleColumns+`, s.next_run_at
		from notification_schedules s
		where s.status = 'ACTIVE' and s.next_run_at <= $1
		order by s.next_run_at, s.id
		for update of s skip locked
		limit 1`, now), &scheduledFor)
	if errors.Is(err, pgx.ErrNoRows) {
		return schedule.Execution{}, schedule.ErrNoDueSchedule
	}
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("claim due schedule: %w", err)
	}

	readiness, err := scheduleReadiness(ctx, tx, item.TenantID, []uuid.UUID{item.ID}, now)
	if err != nil {
		return schedule.Execution{}, err
	}
	blockers := readiness[item.ID]
	executionID := uuid.New()
	if len(blockers) > 0 {
		safeCode := strings.Join(blockers, ",")
		if _, err := tx.Exec(ctx, `
			insert into notification_runs (
			  id, tenant_id, schedule_id, scheduled_for, status, safe_error_code,
			  claimed_by, claimed_at, attempt, next_attempt_at, finished_at, created_at, updated_at, trigger_kind
			) values ($1, $2, $3, $4, 'FAILED', $5, $6, $7, 1, $7, $7, $7, $7, 'SCHEDULED')`,
			executionID, item.TenantID, item.ID, scheduledFor, safeCode, workerID, now); err != nil {
			return schedule.Execution{}, fmt.Errorf("record blocked notification run: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			update notification_schedules
			set status = 'PAUSED', next_run_at = null, version = version + 1, updated_at = $3
			where tenant_id = $1 and id = $2`, item.TenantID, item.ID, now); err != nil {
			return schedule.Execution{}, fmt.Errorf("pause blocked notification schedule: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return schedule.Execution{}, fmt.Errorf("commit blocked notification run: %w", err)
		}
		return schedule.Execution{
			ID: executionID, TenantID: item.TenantID, ScheduleID: item.ID, ScheduledFor: scheduledFor,
			Status: schedule.ExecutionFailed, SafeErrorCode: safeCode,
		}, nil
	}

	nextOccurrences, err := schedule.NextOccurrences(item.Input, scheduledFor, 1)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("calculate next due occurrence: %w", err)
	}
	location, err := time.LoadLocation(item.Timezone)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("load due schedule timezone: %w", err)
	}
	period, err := report.ResolvePeriod(item.PeriodPreset, location, scheduledFor, nil, nil)
	if err != nil {
		return schedule.Execution{}, fmt.Errorf("resolve due report period: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into notification_runs (
		  id, tenant_id, schedule_id, scheduled_for, status, next_attempt_at, created_at, updated_at,
		  materialization_version, trigger_kind
		) values ($1, $2, $3, $4, 'COLLECTING', $5, $5, $5, 2, 'SCHEDULED')`, executionID, item.TenantID, item.ID, scheduledFor, now.Add(5*time.Second)); err != nil {
		return schedule.Execution{}, fmt.Errorf("insert notification run: %w", err)
	}
	reportRunIDs := make([]uuid.UUID, 0, len(item.ReportKeys))
	for index, reportKey := range item.ReportKeys {
		effectivePeriod := period
		if definition, ok := report.DefinitionFor(reportKey); ok {
			effectivePeriod, err = store.periodPolicy.Resolve(item.TenantID, item.PeriodPreset, definition.ParameterKind, location, scheduledFor)
			if err != nil {
				return schedule.Execution{}, fmt.Errorf("resolve scheduled report %s period: %w", reportKey, err)
			}
		}
		paramsJSON, _ := json.Marshal(map[string]string{"dateFrom": effectivePeriod.DateFrom, "dateTo": effectivePeriod.DateTo})
		reportRunID := uuid.New()
		queryPlanFingerprint := report.QueryPlanFingerprint(reportKey, report.ResultSummary)
		idempotencyKey := "schedule:" + item.ID.String() + ":" + scheduledFor.UTC().Format("20060102T150405Z") + ":" + string(reportKey)
		if _, err := tx.Exec(ctx, `
			insert into report_runs (
			  id, tenant_id, report_key, source, idempotency_key, status, period_preset,
			  period_from, period_to, params_json, queued_at, expires_at, created_at, updated_at,
			  result_kind, priority, report_definition_version, data_source_version, progress_phase, progress_updated_at,
			  query_plan_fingerprint
			)
			select $1, $2, $3, 'SCHEDULE', $4, 'QUEUED', $5, $6::date, $7::date, $8, $9, $10, $9, $9,
			       'SUMMARY', 100, d.version, coalesce(c.version, 0), 'QUEUED', $9, $11
			from report_definitions d left join tenant_sml_connections c on c.tenant_id = $2
			where d.report_key = $3`,
			reportRunID, item.TenantID, reportKey, idempotencyKey, effectivePeriod.Preset, effectivePeriod.DateFrom, effectivePeriod.DateTo,
			paramsJSON, now, now.AddDate(0, 3, 0), queryPlanFingerprint); err != nil {
			return schedule.Execution{}, fmt.Errorf("enqueue scheduled report %s: %w", reportKey, err)
		}
		if _, err := tx.Exec(ctx, `
			insert into notification_run_reports (notification_run_id, report_key, report_run_id, position)
			values ($1, $2, $3, $4)`, executionID, reportKey, reportRunID, index+1); err != nil {
			return schedule.Execution{}, fmt.Errorf("link scheduled report %s: %w", reportKey, err)
		}
		reportRunIDs = append(reportRunIDs, reportRunID)
	}
	if _, err := tx.Exec(ctx, `
		update notification_schedules
		set next_run_at = $3, updated_at = $4
		where tenant_id = $1 and id = $2`, item.TenantID, item.ID, nextOccurrences[0], now); err != nil {
		return schedule.Execution{}, fmt.Errorf("advance notification schedule: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Execution{}, fmt.Errorf("commit notification materialization: %w", err)
	}
	return schedule.Execution{
		ID: executionID, TenantID: item.TenantID, ScheduleID: item.ID, ScheduledFor: scheduledFor,
		Status: schedule.ExecutionCollecting, ReportRunIDs: reportRunIDs,
	}, nil
}

func scanSchedule(row rowScanner, extra ...any) (schedule.Schedule, error) {
	var item schedule.Schedule
	var days []int16
	var reportKeys []string
	destinations := []any{
		&item.ID, &item.TenantID, &item.Name, &item.Status, &item.LocalTime, &item.Timezone,
		&item.PeriodPreset, &item.Version, &item.CreatedAt, &item.UpdatedAt, &item.ArchivedAt,
		&days, &reportKeys, &item.RecipientIDs,
	}
	destinations = append(destinations, extra...)
	err := row.Scan(destinations...)
	item.DaysOfWeek = make([]int, len(days))
	for index, day := range days {
		item.DaysOfWeek[index] = int(day)
	}
	item.ReportKeys = make([]report.Key, len(reportKeys))
	for index, key := range reportKeys {
		item.ReportKeys[index] = report.Key(key)
	}
	return item, err
}
