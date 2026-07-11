package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/notification"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type NotificationStore struct {
	pool *pgxpool.Pool
}

const maximumNotificationPayloadBytes = 30 * 1024

func NewNotificationStore(pool *pgxpool.Pool) *NotificationStore {
	return &NotificationStore{pool: pool}
}

func (store *NotificationStore) Claim(ctx context.Context, workerID string, lease time.Duration, now time.Time) (notification.Work, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return notification.Work{}, fmt.Errorf("begin claim notification: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var runID uuid.UUID
	err = tx.QueryRow(ctx, `
		select id from notification_runs
		where status = 'COLLECTING' and next_attempt_at <= $1
		  and (claimed_by is null or lease_expires_at < $1)
		order by next_attempt_at, scheduled_for, id
		for update skip locked
		limit 1`, now).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return notification.Work{}, notification.ErrNoExecutionReady
	}
	if err != nil {
		return notification.Work{}, fmt.Errorf("select notification claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update notification_runs
		set claimed_by = $2, claimed_at = $3, lease_expires_at = $4,
		    attempt = attempt + 1, updated_at = $3
		where id = $1`, runID, workerID, now, now.Add(lease)); err != nil {
		return notification.Work{}, fmt.Errorf("claim notification run: %w", err)
	}
	work, err := loadNotificationWork(ctx, tx, runID, now)
	if err != nil {
		return notification.Work{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return notification.Work{}, fmt.Errorf("commit notification claim: %w", err)
	}
	return work, nil
}

func (store *NotificationStore) Defer(ctx context.Context, runID uuid.UUID, workerID string, availableAt, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update notification_runs
		set claimed_by = null, claimed_at = null, lease_expires_at = null,
		    next_attempt_at = $3, updated_at = $4
		where id = $1 and claimed_by = $2 and status = 'COLLECTING'`, runID, workerID, availableAt, now)
	if err != nil {
		return fmt.Errorf("defer notification run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return notification.ErrExecutionLeaseLost
	}
	return nil
}

func (store *NotificationStore) Fail(ctx context.Context, runID uuid.UUID, workerID, safeCode string, now time.Time) error {
	result, err := store.pool.Exec(ctx, `
		update notification_runs
		set status = 'FAILED', safe_error_code = $3, claimed_by = null, claimed_at = null,
		    lease_expires_at = null, finished_at = $4, updated_at = $4
		where id = $1 and claimed_by = $2 and status = 'COLLECTING'`, runID, workerID, safeCode, now)
	if err != nil {
		return fmt.Errorf("fail notification run: %w", err)
	}
	if result.RowsAffected() != 1 {
		return notification.ErrExecutionLeaseLost
	}
	return nil
}

func (store *NotificationStore) Publish(ctx context.Context, runID uuid.UUID, workerID string, deliveries []notification.PreparedDelivery, partial bool, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin publish notification deliveries: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var tenantID uuid.UUID
	if err := tx.QueryRow(ctx, `
		select tenant_id from notification_runs
		where id = $1 and claimed_by = $2 and status = 'COLLECTING' and lease_expires_at >= $3
		for update`, runID, workerID, now).Scan(&tenantID); errors.Is(err, pgx.ErrNoRows) {
		return notification.ErrExecutionLeaseLost
	} else if err != nil {
		return fmt.Errorf("lock notification publication: %w", err)
	}
	published := 0
	for _, delivery := range deliveries {
		if len(delivery.Payload) == 0 || len(delivery.Payload) > maximumNotificationPayloadBytes || len(delivery.ReferenceHash) == 0 || len(delivery.ReportKeys) == 0 {
			return errors.New("prepared notification delivery is invalid")
		}
		keyStrings := make([]string, len(delivery.ReportKeys))
		for index, key := range delivery.ReportKeys {
			keyStrings[index] = string(key)
		}
		var permissionCount int
		if err := tx.QueryRow(ctx, `
			select count(distinct p.report_key)
			from notification_runs n
			join notification_schedule_recipients sr on sr.schedule_id = n.schedule_id and sr.recipient_id = $3
			join tenant_memberships m on m.tenant_id = n.tenant_id and m.recipient_id = sr.recipient_id and m.status = 'ACTIVE'
			join line_recipients recipient on recipient.id = sr.recipient_id and recipient.status = 'ACTIVE'
			join recipient_report_permissions p on p.tenant_id = n.tenant_id and p.recipient_id = sr.recipient_id
			join notification_run_reports linked on linked.notification_run_id = n.id and linked.report_key = p.report_key
			join report_runs report_run on report_run.id = linked.report_run_id and report_run.status = 'SUCCEEDED'
			where n.id = $1 and n.tenant_id = $2 and p.report_key = any($4::text[])`,
			runID, tenantID, delivery.RecipientID, keyStrings).Scan(&permissionCount); err != nil {
			return fmt.Errorf("recheck delivery permission: %w", err)
		}
		if permissionCount != len(keyStrings) {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into line_deliveries (
			  id, tenant_id, notification_run_id, recipient_id, status, retry_key,
			  next_attempt_at, created_at, updated_at, expires_at
			) values ($1, $2, $3, $4, 'PENDING', $5, $6, $6, $6, $7)`,
			delivery.ID, tenantID, runID, delivery.RecipientID, delivery.RetryKey, now, now.AddDate(1, 0, 0)); err != nil {
			return fmt.Errorf("insert LINE delivery: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into line_delivery_outbox (delivery_id, tenant_id, payload_json, available_at, created_at)
			values ($1, $2, $3, $4, $4)`, delivery.ID, tenantID, delivery.Payload, now); err != nil {
			return fmt.Errorf("insert LINE delivery outbox: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into delivery_access_links (reference_hash, delivery_id, tenant_id, recipient_id, created_at, expires_at)
			values ($1, $2, $3, $4, $5, $6)`, delivery.ReferenceHash, delivery.ID, tenantID, delivery.RecipientID, now, now.AddDate(1, 0, 0)); err != nil {
			return fmt.Errorf("insert delivery access link: %w", err)
		}
		published++
	}
	if published == 0 {
		if _, err := tx.Exec(ctx, `
			update notification_runs
			set status = 'FAILED', safe_error_code = 'NO_ELIGIBLE_RECIPIENTS',
			    claimed_by = null, claimed_at = null, lease_expires_at = null,
			    finished_at = $3, updated_at = $3
			where id = $1 and claimed_by = $2`, runID, workerID, now); err != nil {
			return fmt.Errorf("fail permission-revoked notification: %w", err)
		}
	} else {
		var safeCode any
		if partial {
			safeCode = "REPORT_PARTIAL_FAILURE"
		}
		if _, err := tx.Exec(ctx, `
			update notification_runs
			set status = 'SENDING', safe_error_code = $3,
			    claimed_by = null, claimed_at = null, lease_expires_at = null, updated_at = $4
			where id = $1 and claimed_by = $2`, runID, workerID, safeCode, now); err != nil {
			return fmt.Errorf("mark notification sending: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit notification deliveries: %w", err)
	}
	return nil
}

func loadNotificationWork(ctx context.Context, tx pgx.Tx, runID uuid.UUID, now time.Time) (notification.Work, error) {
	var work notification.Work
	if err := tx.QueryRow(ctx, `
		select n.id, n.tenant_id, n.schedule_id, tenant.name, tenant.timezone
		from notification_runs n
		join tenants tenant on tenant.id = n.tenant_id
		where n.id = $1`, runID).Scan(&work.ID, &work.TenantID, &work.ScheduleID, &work.TenantName, &work.Timezone); err != nil {
		return notification.Work{}, fmt.Errorf("load notification work: %w", err)
	}
	rows, err := tx.Query(ctx, `
		select report_run.id, report_run.report_key, report_run.status, report_run.period_preset,
		       report_run.period_from::text, report_run.period_to::text,
		       report_run.summary_json, report_run.dashboard_json, report_run.finished_at, report_run.expires_at
		from notification_run_reports linked
		join notification_runs notification_run on notification_run.id = linked.notification_run_id
		join report_runs report_run on report_run.id = linked.report_run_id
		join notification_schedule_reports scheduled
		  on scheduled.schedule_id = notification_run.schedule_id and scheduled.report_key = linked.report_key
		where linked.notification_run_id = $1
		order by scheduled.position`, runID)
	if err != nil {
		return notification.Work{}, fmt.Errorf("load notification report results: %w", err)
	}
	defer rows.Close()
	terminalFailures := 0
	for rows.Next() {
		var reportRunID uuid.UUID
		var key report.Key
		var status report.RunStatus
		var period report.Period
		var summaryJSON, dashboardJSON []byte
		var finishedAt *time.Time
		var expiresAt time.Time
		if err := rows.Scan(&reportRunID, &key, &status, &period.Preset, &period.DateFrom, &period.DateTo, &summaryJSON, &dashboardJSON, &finishedAt, &expiresAt); err != nil {
			return notification.Work{}, fmt.Errorf("scan notification report result: %w", err)
		}
		switch status {
		case report.StatusQueued, report.StatusClaimed, report.StatusRunning:
			work.Pending = true
		case report.StatusSucceeded:
			if !expiresAt.After(now) || finishedAt == nil {
				terminalFailures++
				continue
			}
			metrics := make(map[string]string)
			if err := json.Unmarshal(summaryJSON, &metrics); err != nil {
				return notification.Work{}, fmt.Errorf("decode notification report summary: %w", err)
			}
			var dashboard *report.Dashboard
			if string(dashboardJSON) != "{}" {
				var decoded report.Dashboard
				if err := json.Unmarshal(dashboardJSON, &decoded); err != nil {
					return notification.Work{}, fmt.Errorf("decode notification report dashboard: %w", err)
				}
				dashboard = &decoded
			}
			work.Reports = append(work.Reports, notification.ReportResult{RunID: reportRunID, Key: key, Period: period, Metrics: metrics, Dashboard: dashboard, FinishedAt: *finishedAt})
		default:
			terminalFailures++
		}
	}
	if err := rows.Err(); err != nil {
		return notification.Work{}, fmt.Errorf("iterate notification report results: %w", err)
	}
	work.Partial = terminalFailures > 0 && len(work.Reports) > 0
	if work.Pending {
		return work, nil
	}
	targetRows, err := tx.Query(ctx, `
		select sr.recipient_id, array_agg(scheduled.report_key order by scheduled.position)
		from notification_runs n
		join notification_schedule_recipients sr on sr.schedule_id = n.schedule_id
		join tenant_memberships membership
		  on membership.tenant_id = n.tenant_id and membership.recipient_id = sr.recipient_id and membership.status = 'ACTIVE'
		join line_recipients recipient on recipient.id = sr.recipient_id and recipient.status = 'ACTIVE'
		join notification_schedule_reports scheduled on scheduled.schedule_id = n.schedule_id
		join recipient_report_permissions permission
		  on permission.tenant_id = n.tenant_id and permission.recipient_id = sr.recipient_id and permission.report_key = scheduled.report_key
		join notification_run_reports linked
		  on linked.notification_run_id = n.id and linked.report_key = scheduled.report_key
		join report_runs report_run on report_run.id = linked.report_run_id and report_run.status = 'SUCCEEDED' and report_run.expires_at > $2
		where n.id = $1
		group by sr.recipient_id
		having count(distinct permission.report_key) = (
		  select count(*) from notification_schedule_reports expected
		  where expected.schedule_id = (select schedule_id from notification_runs where id = $1)
		)
		order by sr.recipient_id`, runID, now)
	if err != nil {
		return notification.Work{}, fmt.Errorf("load notification targets: %w", err)
	}
	defer targetRows.Close()
	for targetRows.Next() {
		var target notification.Target
		var keys []string
		if err := targetRows.Scan(&target.RecipientID, &keys); err != nil {
			return notification.Work{}, fmt.Errorf("scan notification target: %w", err)
		}
		target.ReportKeys = make([]report.Key, len(keys))
		for index, key := range keys {
			target.ReportKeys[index] = report.Key(key)
		}
		work.Targets = append(work.Targets, target)
	}
	if err := targetRows.Err(); err != nil {
		return notification.Work{}, fmt.Errorf("iterate notification targets: %w", err)
	}
	return work, nil
}
