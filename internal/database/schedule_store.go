package database

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ScheduleStore struct {
	pool         *pgxpool.Pool
	periodPolicy schedule.PeriodPolicy
}

func NewScheduleStore(pool *pgxpool.Pool) *ScheduleStore {
	return &ScheduleStore{pool: pool}
}

func (store *ScheduleStore) ConfigureSmartPeriods(enabled bool, tenantIDs []uuid.UUID, observers ...schedule.PeriodResolutionObserver) *ScheduleStore {
	store.periodPolicy = schedule.NewPeriodPolicy(enabled, tenantIDs, observers...)
	return store
}

func (store *ScheduleStore) Create(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, tenantID uuid.UUID, input schedule.Input, now time.Time) (schedule.Schedule, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 {
		return schedule.Schedule{}, &schedule.ValidationError{Field: "idempotencyKey", Code: "INVALID_IDEMPOTENCY_KEY"}
	}
	requestJSON, err := json.Marshal(struct {
		TenantID uuid.UUID      `json:"tenantId"`
		Input    schedule.Input `json:"input"`
	}{TenantID: tenantID, Input: input})
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("encode schedule create request: %w", err)
	}
	requestHash := sha256.Sum256(requestJSON)
	actorScope := "admin:create-schedule:" + tenantID.String() + ":" + hex.EncodeToString(actorHash)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("begin create schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		insert into idempotency_requests (actor_scope, idempotency_key, request_hash, expires_at)
		values ($1, $2, $3, $4)
		on conflict (actor_scope, idempotency_key) do nothing`, actorScope, idempotencyKey, requestHash[:], now.Add(24*time.Hour)); err != nil {
		return schedule.Schedule{}, fmt.Errorf("reserve schedule idempotency key: %w", err)
	}
	var storedHash, responseJSON []byte
	if err := tx.QueryRow(ctx, `
		select request_hash, response_json from idempotency_requests
		where actor_scope = $1 and idempotency_key = $2
		for update`, actorScope, idempotencyKey).Scan(&storedHash, &responseJSON); err != nil {
		return schedule.Schedule{}, fmt.Errorf("read schedule idempotency key: %w", err)
	}
	if subtle.ConstantTimeCompare(storedHash, requestHash[:]) != 1 {
		return schedule.Schedule{}, schedule.ErrConflict
	}
	if len(responseJSON) > 0 {
		var replay schedule.Schedule
		if err := json.Unmarshal(responseJSON, &replay); err != nil {
			return schedule.Schedule{}, fmt.Errorf("decode idempotent schedule response: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return schedule.Schedule{}, fmt.Errorf("commit schedule replay: %w", err)
		}
		return replay, nil
	}
	var tenantExists bool
	if err := tx.QueryRow(ctx, `select exists(select 1 from tenants where id = $1)`, tenantID).Scan(&tenantExists); err != nil {
		return schedule.Schedule{}, fmt.Errorf("validate schedule tenant: %w", err)
	}
	if !tenantExists {
		return schedule.Schedule{}, schedule.ErrNotFound
	}
	var scheduleID uuid.UUID
	if err := tx.QueryRow(ctx, `
		insert into notification_schedules (tenant_id, name, status, local_time, timezone, period_preset, created_at, updated_at)
		values ($1, $2, 'DRAFT', $3::time, $4, $5, $6, $6)
		returning id`, tenantID, input.Name, input.LocalTime, input.Timezone, input.PeriodPreset, now).Scan(&scheduleID); err != nil {
		return schedule.Schedule{}, mapScheduleWriteError(err, "insert schedule")
	}
	if err := replaceScheduleChildren(ctx, tx, tenantID, scheduleID, input); err != nil {
		return schedule.Schedule{}, err
	}
	created, err := getSchedule(ctx, tx, tenantID, scheduleID, false)
	if err != nil {
		return schedule.Schedule{}, err
	}
	responseJSON, err = json.Marshal(created)
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("encode schedule response: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update idempotency_requests set response_status = 201, response_json = $3
		where actor_scope = $1 and idempotency_key = $2`, actorScope, idempotencyKey, responseJSON); err != nil {
		return schedule.Schedule{}, fmt.Errorf("complete schedule idempotency key: %w", err)
	}
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SCHEDULE_CREATED", "NOTIFICATION_SCHEDULE", scheduleID.String(), requestID, nil, responseJSON, now); err != nil {
		return schedule.Schedule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Schedule{}, fmt.Errorf("commit create schedule: %w", err)
	}
	return created, nil
}

func (store *ScheduleStore) List(ctx context.Context, filter schedule.ListFilter) (schedule.Page, error) {
	var cursorTime *time.Time
	var cursorID *uuid.UUID
	if filter.Cursor != "" {
		valueTime, valueID, err := decodeTenantCursor(filter.Cursor)
		if err != nil {
			return schedule.Page{}, &schedule.ValidationError{Field: "cursor", Code: "INVALID_CURSOR"}
		}
		cursorTime, cursorID = &valueTime, &valueID
	}
	rows, err := store.pool.Query(ctx, `
		select `+scheduleColumns+`
		from notification_schedules s
		where s.tenant_id = $1
		  and ($4 or s.status <> 'ARCHIVED')
		  and ($5::text is null or s.status = $5)
		  and ($6::text = '' or strpos(lower(s.name), lower($6)) > 0)
		  and ($2::timestamptz is null or (s.updated_at, s.id) < ($2, $3))
		order by s.updated_at desc, s.id desc
		limit $7`, filter.TenantID, cursorTime, cursorID, filter.IncludeArchived, filter.Status, filter.Search, filter.PageSize+1)
	if err != nil {
		return schedule.Page{}, fmt.Errorf("list schedules: %w", err)
	}
	defer rows.Close()
	items := make([]schedule.Schedule, 0, filter.PageSize+1)
	for rows.Next() {
		item, err := scanSchedule(rows)
		if err != nil {
			return schedule.Page{}, fmt.Errorf("scan schedule: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return schedule.Page{}, fmt.Errorf("iterate schedules: %w", err)
	}
	hasMore := len(items) > filter.PageSize
	if hasMore {
		items = items[:filter.PageSize]
	}
	nextCursor := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		nextCursor = encodeTenantCursor(last.UpdatedAt, last.ID)
	}
	return schedule.Page{Data: items, NextCursor: nextCursor, HasMore: hasMore}, nil
}

func (store *ScheduleStore) Get(ctx context.Context, tenantID, scheduleID uuid.UUID) (schedule.Schedule, error) {
	return getSchedule(ctx, store.pool, tenantID, scheduleID, false)
}

func (store *ScheduleStore) Update(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, input schedule.Input, version int, now time.Time) (schedule.Schedule, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("begin update schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockTenantRecipientScheduleMutation(ctx, tx, tenantID); err != nil {
		return schedule.Schedule{}, err
	}
	before, err := getSchedule(ctx, tx, tenantID, scheduleID, true)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if before.Status != schedule.StatusDraft && before.Status != schedule.StatusPaused {
		return schedule.Schedule{}, schedule.ErrStateConflict
	}
	if before.Version != version {
		return schedule.Schedule{}, schedule.ErrVersionConflict
	}
	if _, err := tx.Exec(ctx, `
		update notification_schedules
		set name = $3, local_time = $4::time, timezone = $5, period_preset = $6,
		    version = version + 1, updated_at = $7
		where tenant_id = $1 and id = $2`, tenantID, scheduleID, input.Name, input.LocalTime, input.Timezone, input.PeriodPreset, now); err != nil {
		return schedule.Schedule{}, mapScheduleWriteError(err, "update schedule")
	}
	if err := replaceScheduleChildren(ctx, tx, tenantID, scheduleID, input); err != nil {
		return schedule.Schedule{}, err
	}
	updated, err := getSchedule(ctx, tx, tenantID, scheduleID, false)
	if err != nil {
		return schedule.Schedule{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(updated)
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SCHEDULE_UPDATED", "NOTIFICATION_SCHEDULE", scheduleID.String(), requestID, beforeJSON, afterJSON, now); err != nil {
		return schedule.Schedule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Schedule{}, fmt.Errorf("commit update schedule: %w", err)
	}
	return updated, nil
}

// Recipient revocation and schedule activation share this lock so an activation
// cannot pass readiness while the same tenant membership is being revoked.
func lockTenantRecipientScheduleMutation(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended($1, 0))`, "tenant-recipient-schedule:"+tenantID.String()); err != nil {
		return fmt.Errorf("lock tenant recipient schedule mutation: %w", err)
	}
	return nil
}

func (store *ScheduleStore) Readiness(ctx context.Context, tenantID uuid.UUID, scheduleIDs []uuid.UUID, now time.Time) (map[uuid.UUID][]string, error) {
	return scheduleReadiness(ctx, store.pool, tenantID, scheduleIDs, now)
}

func (store *ScheduleStore) Activate(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, nextRunAt, now time.Time) (schedule.Schedule, error) {
	if !nextRunAt.After(now) {
		return schedule.Schedule{}, schedule.ErrStateConflict
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("begin activate schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockTenantRecipientScheduleMutation(ctx, tx, tenantID); err != nil {
		return schedule.Schedule{}, err
	}
	before, err := getSchedule(ctx, tx, tenantID, scheduleID, true)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if before.Status == schedule.StatusActive {
		if err := tx.Commit(ctx); err != nil {
			return schedule.Schedule{}, fmt.Errorf("commit activate replay: %w", err)
		}
		return before, nil
	}
	if before.Status != schedule.StatusDraft && before.Status != schedule.StatusPaused {
		return schedule.Schedule{}, schedule.ErrStateConflict
	}
	readiness, err := scheduleReadiness(ctx, tx, tenantID, []uuid.UUID{scheduleID}, now)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if blockers := readiness[scheduleID]; len(blockers) > 0 {
		return schedule.Schedule{}, &schedule.ReadinessError{Blockers: blockers}
	}
	if _, err := tx.Exec(ctx, `
		update notification_schedules
		set status = 'ACTIVE', next_run_at = $3, version = version + 1, updated_at = $4
		where tenant_id = $1 and id = $2`, tenantID, scheduleID, nextRunAt, now); err != nil {
		return schedule.Schedule{}, fmt.Errorf("activate schedule: %w", err)
	}
	activated, err := getSchedule(ctx, tx, tenantID, scheduleID, false)
	if err != nil {
		return schedule.Schedule{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(activated)
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SCHEDULE_ACTIVATED", "NOTIFICATION_SCHEDULE", scheduleID.String(), requestID, beforeJSON, afterJSON, now); err != nil {
		return schedule.Schedule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Schedule{}, fmt.Errorf("commit activate schedule: %w", err)
	}
	return activated, nil
}

func (store *ScheduleStore) Pause(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, now time.Time) (schedule.Schedule, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("begin pause schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := getSchedule(ctx, tx, tenantID, scheduleID, true)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if before.Status == schedule.StatusPaused {
		if err := tx.Commit(ctx); err != nil {
			return schedule.Schedule{}, fmt.Errorf("commit pause replay: %w", err)
		}
		return before, nil
	}
	if before.Status != schedule.StatusActive {
		return schedule.Schedule{}, schedule.ErrStateConflict
	}
	if _, err := tx.Exec(ctx, `
		update notification_schedules
		set status = 'PAUSED', next_run_at = null, version = version + 1, updated_at = $3
		where tenant_id = $1 and id = $2`, tenantID, scheduleID, now); err != nil {
		return schedule.Schedule{}, fmt.Errorf("pause schedule: %w", err)
	}
	paused, err := getSchedule(ctx, tx, tenantID, scheduleID, false)
	if err != nil {
		return schedule.Schedule{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(paused)
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SCHEDULE_PAUSED", "NOTIFICATION_SCHEDULE", scheduleID.String(), requestID, beforeJSON, afterJSON, now); err != nil {
		return schedule.Schedule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Schedule{}, fmt.Errorf("commit pause schedule: %w", err)
	}
	return paused, nil
}

func (store *ScheduleStore) Archive(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, version int, now time.Time) (schedule.Schedule, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("begin archive schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := getSchedule(ctx, tx, tenantID, scheduleID, true)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if before.Status == schedule.StatusArchived {
		if err := tx.Commit(ctx); err != nil {
			return schedule.Schedule{}, fmt.Errorf("commit archive replay: %w", err)
		}
		return before, nil
	}
	if before.Status != schedule.StatusDraft && before.Status != schedule.StatusPaused {
		return schedule.Schedule{}, schedule.ErrStateConflict
	}
	if before.Version != version {
		return schedule.Schedule{}, schedule.ErrVersionConflict
	}
	if _, err := tx.Exec(ctx, `
		update notification_schedules
		set status = 'ARCHIVED', archived_at = $3, next_run_at = null,
		    version = version + 1, updated_at = $3
		where tenant_id = $1 and id = $2`, tenantID, scheduleID, now); err != nil {
		return schedule.Schedule{}, fmt.Errorf("archive schedule: %w", err)
	}
	archived, err := getSchedule(ctx, tx, tenantID, scheduleID, false)
	if err != nil {
		return schedule.Schedule{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(archived)
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SCHEDULE_ARCHIVED", "NOTIFICATION_SCHEDULE", scheduleID.String(), requestID, beforeJSON, afterJSON, now); err != nil {
		return schedule.Schedule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Schedule{}, fmt.Errorf("commit archive schedule: %w", err)
	}
	return archived, nil
}

func (store *ScheduleStore) Restore(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, version int, now time.Time) (schedule.Schedule, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("begin restore schedule: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	before, err := getSchedule(ctx, tx, tenantID, scheduleID, true)
	if err != nil {
		return schedule.Schedule{}, err
	}
	if before.Status != schedule.StatusArchived {
		return schedule.Schedule{}, schedule.ErrStateConflict
	}
	if before.Version != version {
		return schedule.Schedule{}, schedule.ErrVersionConflict
	}
	if _, err := tx.Exec(ctx, `
		update notification_schedules
		set status = 'DRAFT', archived_at = null, next_run_at = null,
		    version = version + 1, updated_at = $3
		where tenant_id = $1 and id = $2`, tenantID, scheduleID, now); err != nil {
		return schedule.Schedule{}, fmt.Errorf("restore schedule: %w", err)
	}
	restored, err := getSchedule(ctx, tx, tenantID, scheduleID, false)
	if err != nil {
		return schedule.Schedule{}, err
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(restored)
	if err := insertAudit(ctx, tx, tenantID, actorHash, "SCHEDULE_RESTORED", "NOTIFICATION_SCHEDULE", scheduleID.String(), requestID, beforeJSON, afterJSON, now); err != nil {
		return schedule.Schedule{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return schedule.Schedule{}, fmt.Errorf("commit restore schedule: %w", err)
	}
	return restored, nil
}

type scheduleQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getSchedule(ctx context.Context, queryer scheduleQuerier, tenantID, scheduleID uuid.UUID, lock bool) (schedule.Schedule, error) {
	lockClause := ""
	if lock {
		lockClause = " for update of s"
	}
	item, err := scanSchedule(queryer.QueryRow(ctx, `
		select `+scheduleColumns+`
		from notification_schedules s
		where s.tenant_id = $1 and s.id = $2`+lockClause, tenantID, scheduleID))
	if errors.Is(err, pgx.ErrNoRows) {
		return schedule.Schedule{}, schedule.ErrNotFound
	}
	if err != nil {
		return schedule.Schedule{}, fmt.Errorf("get schedule: %w", err)
	}
	return item, nil
}

func replaceScheduleChildren(ctx context.Context, tx pgx.Tx, tenantID, scheduleID uuid.UUID, input schedule.Input) error {
	var existingKeyValues []string
	if err := tx.QueryRow(ctx, `
		select coalesce(array_agg(report_key), '{}')
		from notification_schedule_reports
		where tenant_id = $1 and schedule_id = $2`, tenantID, scheduleID).Scan(&existingKeyValues); err != nil {
		return fmt.Errorf("load existing schedule reports: %w", err)
	}
	existingKeys := make(map[report.Key]struct{}, len(existingKeyValues))
	for _, key := range existingKeyValues {
		existingKeys[report.Key(key)] = struct{}{}
	}
	for _, key := range input.ReportKeys {
		definition, ok := report.DefinitionFor(key)
		_, alreadySelected := existingKeys[key]
		if !ok || !report.CanSelect(definition, alreadySelected) {
			return &schedule.ValidationError{Field: "reportKeys", Code: "DEPRECATED_REPORT"}
		}
	}
	if _, err := tx.Exec(ctx, `delete from notification_schedule_days where tenant_id = $1 and schedule_id = $2`, tenantID, scheduleID); err != nil {
		return fmt.Errorf("clear schedule days: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from notification_schedule_reports where tenant_id = $1 and schedule_id = $2`, tenantID, scheduleID); err != nil {
		return fmt.Errorf("clear schedule reports: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from notification_schedule_recipients where tenant_id = $1 and schedule_id = $2`, tenantID, scheduleID); err != nil {
		return fmt.Errorf("clear schedule recipients: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into notification_schedule_days (tenant_id, schedule_id, day_of_week)
		select $1, $2, value from unnest($3::smallint[]) as value`, tenantID, scheduleID, input.DaysOfWeek); err != nil {
		return mapScheduleWriteError(err, "insert schedule days")
	}
	reportKeys := make([]string, len(input.ReportKeys))
	positions := make([]int16, len(input.ReportKeys))
	for index, key := range input.ReportKeys {
		reportKeys[index] = string(key)
		positions[index] = int16(index + 1)
	}
	if _, err := tx.Exec(ctx, `
		insert into notification_schedule_reports (tenant_id, schedule_id, report_key, position)
		select $1, $2, values.report_key, values.position
		from unnest($3::text[], $4::smallint[]) as values(report_key, position)`, tenantID, scheduleID, reportKeys, positions); err != nil {
		return mapScheduleWriteError(err, "insert schedule reports")
	}
	if _, err := tx.Exec(ctx, `
		insert into notification_schedule_recipients (tenant_id, schedule_id, recipient_id)
		select $1, $2, value from unnest($3::uuid[]) as value`, tenantID, scheduleID, input.RecipientIDs); err != nil {
		return mapScheduleWriteError(err, "insert schedule recipients")
	}
	var missingPermission bool
	if err := tx.QueryRow(ctx, `
		select exists (
		  select 1
		  from notification_schedule_recipients sr
		  join notification_schedule_reports scheduled on scheduled.schedule_id = sr.schedule_id
		  left join recipient_report_permissions permission
		    on permission.tenant_id = sr.tenant_id
		   and permission.recipient_id = sr.recipient_id
		   and permission.report_key = scheduled.report_key
		  where sr.schedule_id = $1 and permission.report_key is null
		)`, scheduleID).Scan(&missingPermission); err != nil {
		return fmt.Errorf("validate schedule recipient permissions: %w", err)
	}
	if missingPermission {
		return &schedule.ValidationError{Field: "recipientIds", Code: "RECIPIENT_PERMISSION_MISMATCH"}
	}
	return nil
}

func scheduleReadiness(ctx context.Context, queryer scheduleQuerier, tenantID uuid.UUID, scheduleIDs []uuid.UUID, now time.Time) (map[uuid.UUID][]string, error) {
	result := make(map[uuid.UUID][]string, len(scheduleIDs))
	if len(scheduleIDs) == 0 {
		return result, nil
	}
	rows, err := queryer.Query(ctx, `
		select s.id,
		       (t.status <> 'ACTIVE' or t.access_ends_at <= $3) as tenant_inactive,
		       not exists (
		         select 1 from tenant_sml_connections c
		         where c.tenant_id = s.tenant_id and c.readiness_status = 'READY' and c.last_tested_at is not null
		       ) as sml_not_ready,
		       exists (
		         select 1 from notification_schedule_recipients sr
		         left join tenant_memberships m on m.tenant_id = sr.tenant_id and m.recipient_id = sr.recipient_id
		         left join line_recipients r on r.id = sr.recipient_id
		         where sr.schedule_id = s.id and (m.status is distinct from 'ACTIVE' or r.status is distinct from 'ACTIVE')
		       ) as recipient_not_active,
		       exists (
		         select 1
		         from notification_schedule_recipients sr
		         join notification_schedule_reports scheduled on scheduled.schedule_id = sr.schedule_id
		         left join recipient_report_permissions p
		           on p.tenant_id = sr.tenant_id and p.report_key = scheduled.report_key and p.recipient_id = sr.recipient_id
		         where sr.schedule_id = s.id and p.report_key is null
		       ) as permission_mismatch
		from notification_schedules s
		join tenants t on t.id = s.tenant_id
		where s.tenant_id = $1 and s.id = any($2::uuid[])
		order by s.id`, tenantID, scheduleIDs, now)
	if err != nil {
		return nil, fmt.Errorf("read schedule readiness: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scheduleID uuid.UUID
		var tenantInactive, smlNotReady, recipientNotActive, permissionMismatch bool
		if err := rows.Scan(&scheduleID, &tenantInactive, &smlNotReady, &recipientNotActive, &permissionMismatch); err != nil {
			return nil, fmt.Errorf("scan schedule readiness: %w", err)
		}
		blockers := make([]string, 0, 4)
		if tenantInactive {
			blockers = append(blockers, schedule.BlockerTenantInactive)
		}
		if smlNotReady {
			blockers = append(blockers, schedule.BlockerSMLNotReady)
		}
		if recipientNotActive {
			blockers = append(blockers, schedule.BlockerRecipientNotActive)
		}
		if permissionMismatch {
			blockers = append(blockers, schedule.BlockerRecipientPermissionMismatch)
		}
		result[scheduleID] = blockers
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schedule readiness: %w", err)
	}
	return result, nil
}

const scheduleColumns = `
s.id, s.tenant_id, s.name, s.status, to_char(s.local_time, 'HH24:MI'), s.timezone,
s.period_preset, s.version, s.created_at, s.updated_at, s.archived_at,
array(select d.day_of_week from notification_schedule_days d where d.schedule_id = s.id order by d.day_of_week),
array(select r.report_key from notification_schedule_reports r where r.schedule_id = s.id order by r.position),
array(select recipient.recipient_id from notification_schedule_recipients recipient where recipient.schedule_id = s.id order by recipient.recipient_id)`

func mapScheduleWriteError(err error, operation string) error {
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) {
		switch postgresError.Code {
		case "23505":
			return schedule.ErrConflict
		case "23503":
			return &schedule.ValidationError{Field: "recipientIds", Code: "RECIPIENT_NOT_IN_TENANT"}
		case "23514", "22P02":
			return &schedule.ValidationError{Field: "schedule", Code: "INVALID_SCHEDULE"}
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
