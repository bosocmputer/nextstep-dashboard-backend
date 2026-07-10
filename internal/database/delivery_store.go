package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/delivery"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DeliveryStore struct {
	pool *pgxpool.Pool
}

func NewDeliveryStore(pool *pgxpool.Pool) *DeliveryStore {
	return &DeliveryStore{pool: pool}
}

func (store *DeliveryStore) Claim(ctx context.Context, workerID string, lease time.Duration, now time.Time) (delivery.Work, error) {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return delivery.Work{}, fmt.Errorf("begin claim LINE delivery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var outboxID uuid.UUID
	err = tx.QueryRow(ctx, `
		select outbox.id
		from line_delivery_outbox outbox
		join line_deliveries delivery on delivery.id = outbox.delivery_id
		where outbox.completed_at is null and outbox.available_at <= $1
		  and (outbox.claimed_by is null or outbox.lease_expires_at < $1)
		  and delivery.status in ('PENDING', 'RETRY_WAIT', 'UNCERTAIN', 'SENDING')
		order by outbox.available_at, outbox.id
		for update of outbox skip locked
		limit 1`, now).Scan(&outboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery.Work{}, delivery.ErrNoDeliveryReady
	}
	if err != nil {
		return delivery.Work{}, fmt.Errorf("select LINE delivery claim: %w", err)
	}
	var work delivery.Work
	var payload []byte
	if err := tx.QueryRow(ctx, `
		update line_delivery_outbox outbox
		set claimed_by = $2, claimed_at = $3, lease_expires_at = $4, attempt = outbox.attempt + 1
		from line_deliveries delivery
		join tenants tenant on tenant.id = delivery.tenant_id
		where outbox.id = $1 and delivery.id = outbox.delivery_id
		returning delivery.id, delivery.recipient_id, delivery.retry_key, outbox.payload_json,
		          delivery.attempt + 1, (tenant.status = 'ACTIVE' and tenant.access_ends_at > $3)`,
		outboxID, workerID, now, now.Add(lease)).Scan(
		&work.ID, &work.RecipientID, &work.RetryKey, &payload, &work.Attempt, &work.TenantActive,
	); err != nil {
		return delivery.Work{}, fmt.Errorf("claim LINE delivery outbox: %w", err)
	}
	work.Payload = payload
	if _, err := tx.Exec(ctx, `
		update line_deliveries
		set status = 'SENDING', attempt = attempt + 1, updated_at = $2
		where id = $1`, work.ID, now); err != nil {
		return delivery.Work{}, fmt.Errorf("mark LINE delivery sending: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return delivery.Work{}, fmt.Errorf("commit LINE delivery claim: %w", err)
	}
	return work, nil
}

func (store *DeliveryStore) Accept(ctx context.Context, deliveryID uuid.UUID, workerID, providerRequestID string, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin accept LINE delivery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	notificationRunID, err := lockDeliveryOutbox(ctx, tx, deliveryID, workerID, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update line_deliveries
		set status = 'ACCEPTED', provider_request_id = nullif($2, ''), safe_error_code = null,
		    next_attempt_at = null, accepted_at = $3, updated_at = $3
		where id = $1`, deliveryID, providerRequestID, now); err != nil {
		return fmt.Errorf("accept LINE delivery: %w", err)
	}
	if _, err := tx.Exec(ctx, `update line_delivery_outbox set completed_at = $2, lease_expires_at = null where delivery_id = $1`, deliveryID, now); err != nil {
		return fmt.Errorf("complete LINE delivery outbox: %w", err)
	}
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if _, err := tx.Exec(ctx, `
		insert into line_monthly_quota (quota_month, locally_accepted, updated_at)
		values ($1, 1, $2)
		on conflict (quota_month) do update
		set locally_accepted = line_monthly_quota.locally_accepted + 1, updated_at = excluded.updated_at`, month, now); err != nil {
		return fmt.Errorf("record accepted LINE quota: %w", err)
	}
	if err := finalizeNotificationRun(ctx, tx, notificationRunID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit accepted LINE delivery: %w", err)
	}
	return nil
}

func (store *DeliveryStore) Retry(ctx context.Context, deliveryID uuid.UUID, workerID, safeCode string, uncertain bool, availableAt, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin retry LINE delivery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockDeliveryOutbox(ctx, tx, deliveryID, workerID, now); err != nil {
		return err
	}
	status := "RETRY_WAIT"
	if uncertain {
		status = "UNCERTAIN"
	}
	if _, err := tx.Exec(ctx, `
		update line_deliveries
		set status = $2, safe_error_code = $3, next_attempt_at = $4, updated_at = $5
		where id = $1`, deliveryID, status, safeCode, availableAt, now); err != nil {
		return fmt.Errorf("retry LINE delivery: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		update line_delivery_outbox
		set available_at = $2, claimed_by = null, claimed_at = null, lease_expires_at = null
		where delivery_id = $1`, deliveryID, availableAt); err != nil {
		return fmt.Errorf("release LINE delivery retry: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit LINE delivery retry: %w", err)
	}
	return nil
}

func (store *DeliveryStore) Fail(ctx context.Context, deliveryID uuid.UUID, workerID, safeCode string, now time.Time) error {
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail LINE delivery: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	notificationRunID, err := lockDeliveryOutbox(ctx, tx, deliveryID, workerID, now)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		update line_deliveries
		set status = 'FAILED_PERMANENT', safe_error_code = $2, next_attempt_at = null, updated_at = $3
		where id = $1`, deliveryID, safeCode, now); err != nil {
		return fmt.Errorf("fail LINE delivery: %w", err)
	}
	if _, err := tx.Exec(ctx, `update line_delivery_outbox set completed_at = $2, lease_expires_at = null where delivery_id = $1`, deliveryID, now); err != nil {
		return fmt.Errorf("complete failed LINE outbox: %w", err)
	}
	if err := finalizeNotificationRun(ctx, tx, notificationRunID, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit failed LINE delivery: %w", err)
	}
	return nil
}

func lockDeliveryOutbox(ctx context.Context, tx pgx.Tx, deliveryID uuid.UUID, workerID string, now time.Time) (uuid.UUID, error) {
	var notificationRunID uuid.UUID
	err := tx.QueryRow(ctx, `
		select delivery.notification_run_id
		from line_delivery_outbox outbox
		join line_deliveries delivery on delivery.id = outbox.delivery_id
		where delivery.id = $1 and outbox.claimed_by = $2 and outbox.completed_at is null
		  and outbox.lease_expires_at >= $3 and delivery.status = 'SENDING'
		for update of outbox, delivery`, deliveryID, workerID, now).Scan(&notificationRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, delivery.ErrDeliveryLeaseLost
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock LINE delivery outbox: %w", err)
	}
	return notificationRunID, nil
}

func finalizeNotificationRun(ctx context.Context, tx pgx.Tx, notificationRunID uuid.UUID, now time.Time) error {
	var active, accepted, failed int
	if err := tx.QueryRow(ctx, `
		select
		  count(*) filter (where status in ('PENDING', 'SENDING', 'RETRY_WAIT', 'UNCERTAIN')),
		  count(*) filter (where status = 'ACCEPTED'),
		  count(*) filter (where status = 'FAILED_PERMANENT')
		from line_deliveries where notification_run_id = $1`, notificationRunID).Scan(&active, &accepted, &failed); err != nil {
		return fmt.Errorf("count notification deliveries: %w", err)
	}
	if active > 0 {
		return nil
	}
	status := "COMPLETED"
	if failed > 0 && accepted > 0 {
		status = "PARTIAL_FAILED"
	} else if failed > 0 {
		status = "FAILED"
	} else {
		var partial bool
		if err := tx.QueryRow(ctx, `select coalesce(safe_error_code = 'REPORT_PARTIAL_FAILURE', false) from notification_runs where id = $1`, notificationRunID).Scan(&partial); err != nil {
			return fmt.Errorf("read notification partial status: %w", err)
		}
		if partial {
			status = "PARTIAL_FAILED"
		}
	}
	if _, err := tx.Exec(ctx, `
		update notification_runs set status = $2, finished_at = $3, updated_at = $3 where id = $1`, notificationRunID, status, now); err != nil {
		return fmt.Errorf("finalize notification run: %w", err)
	}
	return nil
}
