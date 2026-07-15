package viewer

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type identityVerifierFunc func(context.Context, string) (line.Identity, error)

func (verify identityVerifierFunc) Verify(ctx context.Context, token string) (line.Identity, error) {
	return verify(ctx, token)
}

type fakeRecipientResolver struct {
	recipient recipient.Recipient
	resolved  int
	getErr    error
}

func (resolver *fakeRecipientResolver) ResolveIdentity(context.Context, line.Identity, string) (recipient.Recipient, error) {
	resolver.resolved++
	return resolver.recipient, nil
}

func (resolver *fakeRecipientResolver) Get(context.Context, uuid.UUID) (recipient.Recipient, error) {
	return resolver.recipient, resolver.getErr
}

type memoryViewerStore struct {
	session         SessionRecord
	deliveryContext DeliveryContext
	deliveryErr     error
	tenants         []TenantAccess
	reports         []ReportAccess
	findSessionErr  error
}

func (store *memoryViewerStore) CreateSession(_ context.Context, session SessionRecord) error {
	store.session = session
	return nil
}

func (store *memoryViewerStore) FindSession(_ context.Context, tokenHash []byte, now time.Time) (SessionRecord, error) {
	if store.findSessionErr != nil {
		return SessionRecord{}, store.findSessionErr
	}
	if !bytes.Equal(tokenHash, store.session.TokenHash) || !store.session.ExpiresAt.After(now) || store.session.RevokedAt != nil {
		return SessionRecord{}, ErrSessionInvalid
	}
	return store.session, nil
}

func TestServiceDoesNotMisreportDependencyFailureAsInvalidSession(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	dependencyErr := errors.New("database unavailable")
	service := NewService(nil, &fakeRecipientResolver{}, &memoryViewerStore{findSessionErr: dependencyErr}, tokens, func() time.Time { return now })

	_, err := service.Authenticate(context.Background(), "opaque-session")
	if !errors.Is(err, dependencyErr) || errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("Authenticate() error = %v", err)
	}
}

func (store *memoryViewerStore) RevokeSession(_ context.Context, _ []byte, now time.Time) error {
	store.session.RevokedAt = &now
	return nil
}

func (store *memoryViewerStore) ResolveDeliveryReference(context.Context, []byte, uuid.UUID, *uuid.UUID, time.Time) (DeliveryContext, error) {
	return store.deliveryContext, store.deliveryErr
}

func (store *memoryViewerStore) GetDeliveryContext(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) (DeliveryContext, error) {
	return store.deliveryContext, store.deliveryErr
}

func (store *memoryViewerStore) ListTenants(context.Context, uuid.UUID, time.Time) ([]TenantAccess, error) {
	return store.tenants, nil
}

func (store *memoryViewerStore) ListReports(context.Context, uuid.UUID, uuid.UUID, time.Time) ([]ReportAccess, error) {
	return store.reports, nil
}

func (store *memoryViewerStore) CanAccessReport(context.Context, uuid.UUID, uuid.UUID, report.Key, time.Time) (bool, error) {
	return true, nil
}

func TestServiceExchangesVerifiedIdentityForOpaqueSession(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	recipientID := uuid.New()
	resolver := &fakeRecipientResolver{recipient: recipient.Recipient{ID: recipientID, Status: recipient.StatusActive, DisplayName: "Owner"}}
	store := &memoryViewerStore{}
	service := NewService(identityVerifierFunc(func(_ context.Context, token string) (line.Identity, error) {
		if token != "opaque-line-id-token-value" {
			t.Fatalf("token = %q", token)
		}
		return line.Identity{Subject: "U123", DisplayName: "Owner"}, nil
	}), resolver, store, tokens, func() time.Time { return now })

	result, err := service.Exchange(context.Background(), "opaque-line-id-token-value", "invitation-reference-value-123456", "", nil)
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if result.RawToken == "" || result.CSRFToken == "" || result.RecipientID != recipientID || store.session.RecipientID != recipientID || resolver.resolved != 1 {
		t.Fatalf("exchange result = %+v session=%+v", result, store.session)
	}
	authenticated, err := service.Authenticate(context.Background(), result.RawToken)
	if err != nil || authenticated.RecipientID != recipientID {
		t.Fatalf("Authenticate() = %+v, %v", authenticated, err)
	}
	if err := service.ValidateCSRF(authenticated, result.CSRFToken); err != nil {
		t.Fatalf("ValidateCSRF() error = %v", err)
	}
}

func TestServiceCreatesSessionButFailsClosedForCopiedDeliveryReference(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	resolver := &fakeRecipientResolver{recipient: recipient.Recipient{ID: uuid.New(), Status: recipient.StatusActive}}
	store := &memoryViewerStore{deliveryErr: ErrDeliveryContextUnavailable}
	service := NewService(identityVerifierFunc(func(context.Context, string) (line.Identity, error) {
		return line.Identity{Subject: "U-other"}, nil
	}), resolver, store, tokens, func() time.Time { return now })

	result, err := service.Exchange(context.Background(), "opaque-line-id-token-value", "", "copied-delivery-reference-value", nil)
	if err != nil || result.DeliveryContextErrorCode != "DELIVERY_CONTEXT_UNAVAILABLE" || len(store.session.TokenHash) == 0 {
		t.Fatalf("Exchange() = %+v, %v session=%+v", result, err, store.session)
	}
}

func TestServiceRejectsDeliveryReferenceWhoseTenantDoesNotMatchExplicitRoute(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	recipientID, krabiID, wawaID := uuid.New(), uuid.New(), uuid.New()
	resolver := &fakeRecipientResolver{recipient: recipient.Recipient{ID: recipientID, Status: recipient.StatusActive}}
	store := &memoryViewerStore{deliveryContext: DeliveryContext{TenantID: wawaID}}
	service := NewService(identityVerifierFunc(func(context.Context, string) (line.Identity, error) {
		return line.Identity{Subject: "U-owner"}, nil
	}), resolver, store, tokens, func() time.Time { return now })

	result, err := service.Exchange(context.Background(), "opaque-line-id-token-value", "", "valid-delivery-reference-value-1234", &krabiID)
	if err != nil || result.DeliveryContext != nil || result.DeliveryContextErrorCode != "DELIVERY_CONTEXT_UNAVAILABLE" {
		t.Fatalf("Exchange() = %+v, %v", result, err)
	}
}

func TestServiceReturnsPermissionFilteredNavigation(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 64)), func() time.Time { return now })
	recipientID, tenantID := uuid.New(), uuid.New()
	store := &memoryViewerStore{
		session: SessionRecord{TokenHash: []byte("set-later"), RecipientID: recipientID, CSRFHash: []byte("csrf"), ExpiresAt: now.Add(time.Hour)},
		tenants: []TenantAccess{{ID: tenantID, Name: "Shop", Timezone: "Asia/Bangkok", ReportKeys: []report.Key{report.SalesGoodsServices}}},
		reports: []ReportAccess{{Key: report.SalesGoodsServices, Label: "รายงานขายสินค้าและบริการ"}},
	}
	service := NewService(nil, &fakeRecipientResolver{}, store, tokens, func() time.Time { return now })
	tenantList, err := service.ListTenants(context.Background(), recipientID)
	if err != nil || len(tenantList) != 1 || tenantList[0].ID != tenantID {
		t.Fatalf("ListTenants() = %+v, %v", tenantList, err)
	}
	reportList, err := service.ListReports(context.Background(), recipientID, tenantID)
	if err != nil || len(reportList) != 1 || reportList[0].Key != report.SalesGoodsServices || reportList[0].PeriodMode != report.DateRange {
		t.Fatalf("ListReports() = %+v, %v", reportList, err)
	}
}
