package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/google/uuid"
)

type fakeStore struct {
	deliveries DeliveryPage
}

func (store fakeStore) GetLineQuota(context.Context, time.Time) (LineQuotaStatus, error) {
	return LineQuotaStatus{}, nil
}
func (store fakeStore) ListReportRuns(context.Context, ReportRunFilter) (ReportRunPage, error) {
	return ReportRunPage{}, nil
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
