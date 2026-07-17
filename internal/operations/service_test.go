package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type fakeStore struct {
	deliveries DeliveryPage
	reportRuns ReportRunPage
	detail     ReportRunDetail
}

func (store fakeStore) GetLineQuota(context.Context, time.Time) (LineQuotaStatus, error) {
	return LineQuotaStatus{}, nil
}
func (store fakeStore) ListReportRuns(context.Context, ReportRunFilter) (ReportRunPage, error) {
	return store.reportRuns, nil
}
func (store fakeStore) GetReportRunDetail(context.Context, uuid.UUID, time.Time) (ReportRunDetail, error) {
	return store.detail, nil
}

func TestReportRunFailureSummaryPreservesEvidenceAndMarksLegacyFacts(t *testing.T) {
	now := time.Date(2026, 7, 16, 11, 0, 4, 0, time.UTC)
	confirmed := failure.Complete(failure.Evidence{
		Version: 1, Level: failure.LevelConfirmed, Category: failure.CategoryJavaWSConnectivity,
		Stage: failure.StageConnectJavaWS, TransportPhase: failure.PhaseBeforeRequestSent,
		OccurredAt: now, SafeErrorCode: failure.CodeSMLUnreachable,
	})
	legacyStarted := now.Add(-3 * time.Second)
	service := NewService(fakeStore{reportRuns: ReportRunPage{Data: []ReportRun{
		{Run: report.Run{SafeErrorCode: failure.CodeSMLUnreachable, FailureEvidence: &confirmed}},
		{Run: report.Run{SafeErrorCode: failure.CodeSMLUnreachable, StartedAt: &legacyStarted, FinishedAt: &now, UpdatedAt: now}},
	}}}, fakeRecipientNames{})

	page, err := service.ListReportRuns(context.Background(), ReportRunFilter{})
	if err != nil || len(page.Data) != 2 {
		t.Fatalf("ListReportRuns() = %+v, %v", page, err)
	}
	if page.Data[0].FailureSummary == nil || page.Data[0].FailureSummary.TransportPhase != failure.PhaseBeforeRequestSent || page.Data[0].FailureSummary.Level != failure.LevelConfirmed {
		t.Fatalf("confirmed summary = %+v", page.Data[0].FailureSummary)
	}
	legacy := page.Data[1].FailureSummary
	if legacy == nil || legacy.Level != failure.LevelLegacyPartial || legacy.TransportPhase != "" || legacy.DurationMS == nil || *legacy.DurationMS != 3000 {
		t.Fatalf("legacy summary = %+v", legacy)
	}
}
func (store fakeStore) ListDeliveries(context.Context, DeliveryFilter) (DeliveryPage, error) {
	return store.deliveries, nil
}
func (store fakeStore) ListAudit(context.Context, AuditFilter) (AuditPage, error) {
	return AuditPage{}, nil
}

type fakeRecipientNames struct {
	name string
	err  error
}

func (resolver fakeRecipientNames) DisplayName(recipient.StoredRecipient) (string, error) {
	return resolver.name, resolver.err
}

func TestListDeliveriesAddsDecryptedRecipientDisplayName(t *testing.T) {
	deliveryID := uuid.New()
	service := NewService(fakeStore{deliveries: DeliveryPage{Data: []Delivery{{
		ID: deliveryID, StoredRecipient: recipient.StoredRecipient{ID: uuid.New()},
	}}}}, fakeRecipientNames{name: "เจ้าของร้าน"})

	page, err := service.ListDeliveries(context.Background(), DeliveryFilter{PageSize: 25})

	if err != nil || len(page.Data) != 1 || page.Data[0].RecipientName != "เจ้าของร้าน" {
		t.Fatalf("ListDeliveries() = %+v, %v", page, err)
	}
}

func TestListDeliveriesFailsClosedWhenRecipientNameCannotBeDecrypted(t *testing.T) {
	service := NewService(fakeStore{deliveries: DeliveryPage{Data: []Delivery{{
		ID: uuid.New(), StoredRecipient: recipient.StoredRecipient{ID: uuid.New()},
	}}}}, fakeRecipientNames{err: errors.New("decrypt failed")})

	_, err := service.ListDeliveries(context.Background(), DeliveryFilter{PageSize: 25})

	if err == nil {
		t.Fatal("ListDeliveries() error = nil")
	}
}
