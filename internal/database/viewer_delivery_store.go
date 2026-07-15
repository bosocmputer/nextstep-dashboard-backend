package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (store *ViewerStore) ResolveDeliveryReference(ctx context.Context, referenceHash []byte, recipientID uuid.UUID, expectedTenantID *uuid.UUID, now time.Time) (viewer.DeliveryContext, error) {
	return store.loadDeliveryContext(ctx, referenceHash, recipientID, expectedTenantID, nil, now)
}

func (store *ViewerStore) GetDeliveryContext(ctx context.Context, recipientID, tenantID, deliveryID uuid.UUID, now time.Time) (viewer.DeliveryContext, error) {
	return store.loadDeliveryContext(ctx, nil, recipientID, &tenantID, &deliveryID, now)
}

func (store *ViewerStore) loadDeliveryContext(ctx context.Context, referenceHash []byte, recipientID uuid.UUID, expectedTenantID, deliveryID *uuid.UUID, now time.Time) (viewer.DeliveryContext, error) {
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return viewer.DeliveryContext{}, fmt.Errorf("begin delivery context: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var item viewer.DeliveryContext
	var notificationRunID uuid.UUID
	err = tx.QueryRow(ctx, `
		select delivery.id, delivery.tenant_id, delivery.notification_run_id,
		       notification_run.scheduled_for, notification_run.materialization_version
		from delivery_access_links access_link
		join line_deliveries delivery
		  on delivery.id = access_link.delivery_id
		 and delivery.tenant_id = access_link.tenant_id
		 and delivery.recipient_id = access_link.recipient_id
		join notification_runs notification_run
		  on notification_run.id = delivery.notification_run_id
		 and notification_run.tenant_id = delivery.tenant_id
		join tenant_memberships membership
		  on membership.tenant_id = delivery.tenant_id
		 and membership.recipient_id = delivery.recipient_id
		 and membership.status = 'ACTIVE'
		join line_recipients recipient
		  on recipient.id = delivery.recipient_id and recipient.status = 'ACTIVE'
		join tenants tenant
		  on tenant.id = delivery.tenant_id and tenant.status = 'ACTIVE' and tenant.access_ends_at > $5
		where access_link.recipient_id = $3
		  and access_link.expires_at > $5 and delivery.expires_at > $5
		  and delivery.status = 'ACCEPTED'
		  and ($1::bytea is null or access_link.reference_hash = $1)
		  and ($2::uuid is null or delivery.id = $2)
		  and ($4::uuid is null or delivery.tenant_id = $4)`, referenceHash, deliveryID, recipientID, expectedTenantID, now).Scan(
		&item.DeliveryID, &item.TenantID, &notificationRunID, &item.ScheduledFor, &item.MaterializationVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return viewer.DeliveryContext{}, viewer.ErrDeliveryContextUnavailable
	}
	if err != nil {
		return viewer.DeliveryContext{}, fmt.Errorf("load delivery context header: %w", err)
	}

	rows, err := tx.Query(ctx, `
		select linked.report_key, coalesce(definition.label_th, linked.report_key), linked.position,
		       linked.report_run_id,
		       report_run.id is not null
		         and report_run.tenant_id = $2
		         and report_run.report_key = linked.report_key as integrity_ok,
		       exists (
		         select 1 from recipient_report_permissions permission
		         where permission.tenant_id = $2 and permission.recipient_id = $3
		           and permission.report_key = linked.report_key
		       ) as permission_ok,
		       report_run.status, report_run.dashboard_json,
		       coalesce(report_run.period_from::text, ''), coalesce(report_run.period_to::text, ''),
		       coalesce(report_run.source_started_at, report_run.started_at),
		       coalesce(report_run.source_finished_at, report_run.finished_at),
		       coalesce(report_run.report_definition_version, ''), coalesce(report_run.data_source_version, 0),
		       coalesce(report_run.query_plan_fingerprint, ''), report_run.source_consistency,
		       report_run.result_kind, report_run.expires_at
		from notification_run_reports linked
		left join report_runs report_run on report_run.id = linked.report_run_id
		left join report_definitions definition on definition.report_key = linked.report_key
		where linked.notification_run_id = $1
		order by linked.position nulls last, linked.report_key`, notificationRunID, item.TenantID, recipientID)
	if err != nil {
		return viewer.DeliveryContext{}, fmt.Errorf("load delivery context reports: %w", err)
	}
	defer rows.Close()

	positions := make([]*int16, 0, 10)
	availableCount := 0
	for rows.Next() {
		var contextReport viewer.DeliveryContextReport
		var integrityOK, permissionOK bool
		var status *report.RunStatus
		var dashboardJSON []byte
		var resultKind *report.ResultKind
		var expiresAt *time.Time
		var sourceConsistency *report.SourceConsistency
		var snapshot viewer.DashboardSnapshot
		if err := rows.Scan(
			&contextReport.ReportKey, &contextReport.Label, &contextReport.Position, &contextReport.ReportRunID,
			&integrityOK, &permissionOK, &status, &dashboardJSON, &snapshot.PeriodFrom, &snapshot.PeriodTo,
			&snapshot.SourceStartedAt, &snapshot.SourceFinishedAt, &snapshot.ReportDefinitionVersion,
			&snapshot.DataSourceVersion, &snapshot.QueryPlanFingerprint, &sourceConsistency, &resultKind, &expiresAt,
		); err != nil {
			return viewer.DeliveryContext{}, fmt.Errorf("scan delivery context report: %w", err)
		}
		if !integrityOK {
			return viewer.DeliveryContext{}, viewer.ErrDeliveryContextUnavailable
		}
		if !permissionOK {
			return viewer.DeliveryContext{}, viewer.ErrDeliveryContextPermissionChanged
		}
		positions = append(positions, contextReport.Position)
		snapshot.RunID = contextReport.ReportRunID
		if sourceConsistency != nil {
			snapshot.SourceConsistency = *sourceConsistency
		}
		if resultKind != nil && *resultKind == report.ResultDetail && expiresAt != nil {
			snapshot.DetailsExpiresAt = expiresAt
			snapshot.DetailsAvailable = expiresAt.After(now)
		}
		switch {
		case status == nil:
			contextReport.SnapshotStatus = viewer.DeliverySnapshotUnavailable
		case *status != report.StatusSucceeded:
			contextReport.SnapshotStatus = viewer.DeliverySnapshotUnavailable
		case len(dashboardJSON) == 0 || string(dashboardJSON) == "{}":
			contextReport.SnapshotStatus = viewer.DeliverySnapshotExpired
		default:
			if err := json.Unmarshal(dashboardJSON, &snapshot.Dashboard); err != nil || snapshot.Dashboard.ReportKey != contextReport.ReportKey {
				return viewer.DeliveryContext{}, viewer.ErrDeliveryContextUnavailable
			}
			if resultKind != nil && *resultKind == report.ResultDetail && !snapshot.DetailsAvailable {
				contextReport.SnapshotStatus = viewer.DeliverySnapshotDetailExpired
			} else {
				contextReport.SnapshotStatus = viewer.DeliverySnapshotAvailable
			}
			contextReport.Summary = &snapshot
			availableCount++
		}
		item.Reports = append(item.Reports, contextReport)
	}
	if err := rows.Err(); err != nil {
		return viewer.DeliveryContext{}, fmt.Errorf("iterate delivery context reports: %w", err)
	}
	legacyOrder, err := validateMaterializedPositions(item.MaterializationVersion, positions)
	if err != nil {
		return viewer.DeliveryContext{}, viewer.ErrDeliveryContextUnavailable
	}
	if legacyOrder {
		item.OrderStatus = viewer.DeliveryOrderLegacy
	} else {
		item.OrderStatus = viewer.DeliveryOrderExact
	}
	switch {
	case availableCount == len(item.Reports) && availableCount > 0:
		item.DataStatus = viewer.DeliveryDataAvailable
	case availableCount > 0:
		item.DataStatus = viewer.DeliveryDataPartial
	default:
		item.DataStatus = viewer.DeliveryDataExpired
	}
	if err := tx.Commit(ctx); err != nil {
		return viewer.DeliveryContext{}, fmt.Errorf("commit delivery context: %w", err)
	}
	return item, nil
}
