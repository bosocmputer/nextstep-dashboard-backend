package operations

import (
	"context"
	"fmt"
	"time"
)

type Service struct {
	store          Store
	recipientNames RecipientNameResolver
}

func NewService(store Store, recipientNames RecipientNameResolver) *Service {
	return &Service{store: store, recipientNames: recipientNames}
}

func (service *Service) GetLineQuota(ctx context.Context, now time.Time) (LineQuotaStatus, error) {
	return service.store.GetLineQuota(ctx, now)
}

func (service *Service) ListReportRuns(ctx context.Context, filter ReportRunFilter) (ReportRunPage, error) {
	return service.store.ListReportRuns(ctx, filter)
}

func (service *Service) ListDeliveries(ctx context.Context, filter DeliveryFilter) (DeliveryPage, error) {
	page, err := service.store.ListDeliveries(ctx, filter)
	if err != nil {
		return DeliveryPage{}, err
	}
	for index := range page.Data {
		name, err := service.recipientNames.DisplayName(page.Data[index].StoredRecipient)
		if err != nil {
			return DeliveryPage{}, fmt.Errorf("resolve delivery recipient display name: %w", err)
		}
		page.Data[index].RecipientName = name
	}
	return page, nil
}

func (service *Service) ListAudit(ctx context.Context, filter AuditFilter) (AuditPage, error) {
	return service.store.ListAudit(ctx, filter)
}
