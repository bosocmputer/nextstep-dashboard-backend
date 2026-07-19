package operations

import (
	"context"
	"fmt"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
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
	page, err := service.store.ListReportRuns(ctx, filter)
	if err != nil {
		return ReportRunPage{}, err
	}
	for index := range page.Data {
		page.Data[index].FailureSummary = failureSummary(page.Data[index].Run)
	}
	return page, nil
}

func (service *Service) GetReportRunDetail(ctx context.Context, runID uuid.UUID, now time.Time) (ReportRunDetail, error) {
	detail, err := service.store.GetReportRunDetail(ctx, runID, now)
	if err != nil {
		return ReportRunDetail{}, err
	}
	detail.FailureSummary = failureSummary(detail.Run)
	return detail, nil
}

func failureSummary(run report.Run) *failure.Evidence {
	if run.SafeErrorCode == "" {
		return nil
	}
	if run.FailureEvidence != nil {
		evidence := failure.Complete(*run.FailureEvidence)
		return &evidence
	}
	occurredAt := run.UpdatedAt
	if run.FinishedAt != nil {
		occurredAt = *run.FinishedAt
	}
	evidence := failure.EvidenceForCode(run.SafeErrorCode)
	evidence.Version = 0
	evidence.Level = failure.LevelLegacyPartial
	evidence.OccurredAt = occurredAt
	evidence.StartedAt = run.StartedAt
	evidence.FinishedAt = run.FinishedAt
	if run.StartedAt != nil && run.FinishedAt != nil {
		duration := run.FinishedAt.Sub(*run.StartedAt).Milliseconds()
		if duration >= 0 {
			evidence.DurationMS = &duration
		}
	}
	attempt := run.Attempt
	evidence.Attempt = &attempt
	evidence.Retryable = false
	evidence.RemoteStateUnknown = false
	returnEvidence := failure.Complete(evidence)
	return &returnEvidence
}

// SummarizeFailure builds a safe, presentation-ready failure summary without
// exposing raw SML output. Table query endpoints share this exact behavior with
// the legacy cursor endpoint.
func SummarizeFailure(run report.Run) *failure.Evidence { return failureSummary(run) }

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
