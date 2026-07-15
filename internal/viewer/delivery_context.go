package viewer

import (
	"context"
	"errors"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

var (
	ErrDeliveryContextUnavailable       = errors.New("delivery context is unavailable")
	ErrDeliveryContextPermissionChanged = errors.New("delivery context permission changed")
)

type DeliveryOrderStatus string

const (
	DeliveryOrderExact  DeliveryOrderStatus = "EXACT"
	DeliveryOrderLegacy DeliveryOrderStatus = "LEGACY"
)

type DeliveryDataStatus string

const (
	DeliveryDataAvailable DeliveryDataStatus = "AVAILABLE"
	DeliveryDataPartial   DeliveryDataStatus = "PARTIAL_EXPIRED"
	DeliveryDataExpired   DeliveryDataStatus = "EXPIRED"
)

type DeliverySnapshotStatus string

const (
	DeliverySnapshotAvailable     DeliverySnapshotStatus = "AVAILABLE"
	DeliverySnapshotDetailExpired DeliverySnapshotStatus = "DETAIL_EXPIRED"
	DeliverySnapshotExpired       DeliverySnapshotStatus = "SNAPSHOT_EXPIRED"
	DeliverySnapshotUnavailable   DeliverySnapshotStatus = "UNAVAILABLE"
)

type DeliveryContextReport struct {
	ReportKey      report.Key             `json:"reportKey"`
	Label          string                 `json:"label"`
	Position       *int16                 `json:"position,omitempty"`
	ReportRunID    uuid.UUID              `json:"reportRunId"`
	SnapshotStatus DeliverySnapshotStatus `json:"snapshotStatus"`
	Summary        *DashboardSnapshot     `json:"summary,omitempty"`
}

type DeliveryContext struct {
	DeliveryID             uuid.UUID               `json:"deliveryId"`
	TenantID               uuid.UUID               `json:"tenantId"`
	ScheduledFor           time.Time               `json:"scheduledFor"`
	MaterializationVersion int16                   `json:"materializationVersion"`
	OrderStatus            DeliveryOrderStatus     `json:"orderStatus"`
	DataStatus             DeliveryDataStatus      `json:"dataStatus"`
	Reports                []DeliveryContextReport `json:"reports"`
}

type DeliveryReportContext struct {
	DeliveryID   uuid.UUID             `json:"deliveryId"`
	TenantID     uuid.UUID             `json:"tenantId"`
	ScheduledFor time.Time             `json:"scheduledFor"`
	OrderStatus  DeliveryOrderStatus   `json:"orderStatus"`
	Report       DeliveryContextReport `json:"report"`
}

func (service *Service) ResolveDeliveryContext(ctx context.Context, authenticated AuthenticatedViewer, rawReference string, expectedTenantID *uuid.UUID) (DeliveryContext, error) {
	if len(rawReference) < 32 || len(rawReference) > 512 {
		return DeliveryContext{}, ErrDeliveryContextUnavailable
	}
	referenceHash := service.tokens.HashToken("delivery-reference:" + rawReference)
	item, err := service.store.ResolveDeliveryReference(ctx, referenceHash, authenticated.RecipientID, expectedTenantID, service.now().UTC())
	if err != nil {
		return DeliveryContext{}, err
	}
	if expectedTenantID != nil && item.TenantID != *expectedTenantID {
		return DeliveryContext{}, ErrDeliveryContextUnavailable
	}
	return item, nil
}

func (service *Service) GetDeliveryContext(ctx context.Context, authenticated AuthenticatedViewer, tenantID, deliveryID uuid.UUID) (DeliveryContext, error) {
	return service.store.GetDeliveryContext(ctx, authenticated.RecipientID, tenantID, deliveryID, service.now().UTC())
}

func (service *Service) GetDeliveryReport(ctx context.Context, authenticated AuthenticatedViewer, tenantID, deliveryID uuid.UUID, reportKey report.Key) (DeliveryReportContext, error) {
	item, err := service.GetDeliveryContext(ctx, authenticated, tenantID, deliveryID)
	if err != nil {
		return DeliveryReportContext{}, err
	}
	for _, contextReport := range item.Reports {
		if contextReport.ReportKey == reportKey {
			return DeliveryReportContext{
				DeliveryID: item.DeliveryID, TenantID: item.TenantID, ScheduledFor: item.ScheduledFor,
				OrderStatus: item.OrderStatus, Report: contextReport,
			}, nil
		}
	}
	return DeliveryReportContext{}, ErrDeliveryContextUnavailable
}
