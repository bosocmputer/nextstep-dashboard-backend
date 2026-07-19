package recipient

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/google/uuid"
)

type memoryRecipientStore struct {
	stored     StoredRecipient
	inviteHash []byte
	redeemed   StoredRecipient
	revoked    bool
}

func (store *memoryRecipientStore) ReissueInvitation(_ context.Context, _ []byte, _ string, tenantID, recipientID uuid.UUID, inviteHash []byte, _ time.Time, _ time.Time) (StoredRecipient, error) {
	if store.stored.TenantID != tenantID || store.stored.ID != recipientID || store.stored.Status != StatusPending {
		return StoredRecipient{}, ErrRecipientNotFound
	}
	store.inviteHash = append([]byte(nil), inviteHash...)
	return store.stored, nil
}

func (store *memoryRecipientStore) CreateInvitation(_ context.Context, _ []byte, _, _ string, _ []byte, pending StoredRecipient, inviteHash []byte, _ time.Time, _ time.Time) (StoredRecipient, error) {
	store.stored = pending
	store.inviteHash = append([]byte(nil), inviteHash...)
	return pending, nil
}

func (store *memoryRecipientStore) List(context.Context, uuid.UUID, int, string) (Page, error) {
	return Page{Stored: []StoredRecipient{store.stored}}, nil
}

func (store *memoryRecipientStore) PermissionDependencies(context.Context, uuid.UUID, uuid.UUID) (PermissionDependencies, error) {
	return PermissionDependencies{RecipientID: store.stored.ID, PermissionsVersion: store.stored.PermissionsVersion, Items: []PermissionDependency{}}, nil
}

func (store *memoryRecipientStore) ListScheduleCandidates(context.Context, uuid.UUID, int) ([]StoredRecipient, error) {
	return []StoredRecipient{store.stored}, nil
}

func (store *memoryRecipientStore) ReplacePermissions(_ context.Context, _ []byte, _ string, _ uuid.UUID, _ uuid.UUID, keys []report.Key, version int, _ time.Time) (StoredRecipient, error) {
	if store.stored.PermissionsVersion != version {
		return StoredRecipient{}, ErrVersionConflict
	}
	store.stored.ReportKeys = append([]report.Key(nil), keys...)
	store.stored.PermissionsVersion++
	return store.stored, nil
}

func (store *memoryRecipientStore) Revoke(_ context.Context, _ []byte, _ string, tenantID, recipientID uuid.UUID, _ time.Time) error {
	if store.stored.TenantID != tenantID || store.stored.ID != recipientID {
		return ErrRecipientNotFound
	}
	store.revoked = true
	return nil
}

func (store *memoryRecipientStore) RedeemInvitation(_ context.Context, inviteHash, _ []byte, identity StoredRecipient, _ time.Time) (StoredRecipient, error) {
	if !bytes.Equal(inviteHash, store.inviteHash) {
		return StoredRecipient{}, ErrInvitationInvalid
	}
	store.redeemed = identity
	return identity, nil
}

func (store *memoryRecipientStore) FindByLineHash(_ context.Context, lineHash []byte) (StoredRecipient, error) {
	if !bytes.Equal(lineHash, store.redeemed.LineUserIDHash) {
		return StoredRecipient{}, ErrRecipientNotFound
	}
	return store.redeemed, nil
}

func (store *memoryRecipientStore) GetByID(_ context.Context, recipientID uuid.UUID) (StoredRecipient, error) {
	if store.redeemed.ID == recipientID {
		return store.redeemed, nil
	}
	return StoredRecipient{}, ErrRecipientNotFound
}

func (store *memoryRecipientStore) GetForTenant(_ context.Context, tenantID, recipientID uuid.UUID) (StoredRecipient, error) {
	if store.stored.TenantID == tenantID && store.stored.ID == recipientID {
		return store.stored, nil
	}
	return StoredRecipient{}, ErrRecipientNotFound
}

func TestServiceCreatesOpaqueInvitationWithoutStoringLabelPlaintext(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 12)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	store := &memoryRecipientStore{}
	service := NewService(store, box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })
	tenantID := uuid.New()

	created, err := service.CreateInvitation(context.Background(), []byte("admin"), "request-1", "recipient-create-001", tenantID, "เจ้าของร้าน")
	if err != nil {
		t.Fatalf("CreateInvitation() error = %v", err)
	}
	if created.Status != StatusPending || !strings.HasPrefix(created.InvitationURL, "https://dashboard.nextstep-soft.com/app/invite?ref=") {
		t.Fatalf("created recipient = %+v", created)
	}
	if strings.Contains(string(store.stored.DisplayName.Ciphertext), "เจ้าของร้าน") || len(store.inviteHash) != 32 {
		t.Fatal("pending recipient stored plaintext label or raw invitation")
	}
}

func TestServiceReturnsEmptyReportKeyArraysForNewRecipients(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 12)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	store := &memoryRecipientStore{}
	service := NewService(store, box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })
	tenantID := uuid.New()

	created, err := service.CreateInvitation(context.Background(), []byte("admin"), "request-1", "recipient-create-empty-permissions", tenantID, "Owner")
	if err != nil {
		t.Fatalf("CreateInvitation() error = %v", err)
	}
	if created.ReportKeys == nil || len(created.ReportKeys) != 0 {
		t.Fatalf("CreateInvitation() reportKeys = %#v, want non-nil empty array", created.ReportKeys)
	}

	page, err := service.List(context.Background(), tenantID, 25, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ReportKeys == nil || len(page.Data[0].ReportKeys) != 0 {
		t.Fatalf("List() reportKeys = %#v, want non-nil empty array", page.Data)
	}
}

func TestServiceQueriesEncryptedRecipientNamesWithExactPagination(t *testing.T) {
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 12)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	store := &memoryRecipientStore{}
	service := NewService(store, box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })
	tenantID := uuid.New()
	if _, err := service.CreateInvitation(context.Background(), []byte("admin"), "request-1", "recipient-query-test", tenantID, "ผู้บริหารร้าน"); err != nil {
		t.Fatal(err)
	}
	result, err := service.Query(context.Background(), tenantID, QueryInput{Search: "บริหาร", Status: StatusPending, PermissionState: "WITHOUT_REPORTS", Page: 0, PageSize: 25})
	if err != nil || result.Total != 1 || len(result.Data) != 1 || result.Data[0].DisplayName != "ผู้บริหารร้าน" {
		t.Fatalf("Query() = %+v, %v", result, err)
	}
	empty, err := service.Query(context.Background(), tenantID, QueryInput{Status: StatusActive, Page: 0, PageSize: 25})
	if err != nil || empty.Total != 0 || len(empty.Data) != 0 {
		t.Fatalf("active Query() = %+v, %v", empty, err)
	}
}

func TestServiceReissuesPendingInvitationAndInvalidatesPreviousReference(t *testing.T) {
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 60)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	store := &memoryRecipientStore{}
	service := NewService(store, box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })
	tenantID := uuid.New()
	created, err := service.CreateInvitation(context.Background(), []byte("admin"), "request-1", "recipient-create-reissue", tenantID, "kurayami")
	if err != nil {
		t.Fatal(err)
	}
	oldReference := strings.TrimPrefix(created.InvitationURL, "https://dashboard.nextstep-soft.com/app/invite?ref=")

	reissued, err := service.ReissueInvitation(context.Background(), []byte("admin"), "request-2", "recipient-reissue-001", tenantID, created.ID)
	if err != nil {
		t.Fatalf("ReissueInvitation() error = %v", err)
	}
	if reissued.InvitationURL == "" || reissued.InvitationURL == created.InvitationURL {
		t.Fatalf("ReissueInvitation() URL = %q, want a new URL", reissued.InvitationURL)
	}
	if _, err := service.ResolveIdentity(context.Background(), line.Identity{Subject: "U123", DisplayName: "Kurayami"}, oldReference); !errors.Is(err, ErrInvitationInvalid) {
		t.Fatalf("old invitation error = %v, want ErrInvitationInvalid", err)
	}
	newReference := strings.TrimPrefix(reissued.InvitationURL, "https://dashboard.nextstep-soft.com/app/invite?ref=")
	if _, err := service.ResolveIdentity(context.Background(), line.Identity{Subject: "U123", DisplayName: "Kurayami"}, newReference); err != nil {
		t.Fatalf("new invitation redemption error = %v", err)
	}
}

func TestServiceUsesOptimisticPermissionVersioning(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 12)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	store := &memoryRecipientStore{}
	service := NewService(store, box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })
	created, err := service.CreateInvitation(context.Background(), []byte("admin"), "request-1", "recipient-versioning", uuid.New(), "Owner")
	if err != nil {
		t.Fatal(err)
	}
	updated, err := service.ReplacePermissions(context.Background(), []byte("admin"), "request-2", store.stored.TenantID, created.ID, []report.Key{report.SalesGoodsServices}, created.PermissionsVersion)
	if err != nil || updated.PermissionsVersion != created.PermissionsVersion+1 {
		t.Fatalf("ReplacePermissions() = %+v, %v", updated, err)
	}
	if _, err := service.ReplacePermissions(context.Background(), []byte("admin"), "request-3", store.stored.TenantID, created.ID, nil, created.PermissionsVersion); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale permission version error = %v", err)
	}
}

func TestServiceRevokesRecipientWithinTenantScope(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 12)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	store := &memoryRecipientStore{}
	service := NewService(store, box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })
	created, err := service.CreateInvitation(context.Background(), []byte("admin"), "request-1", "recipient-revoke-001", uuid.New(), "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Revoke(context.Background(), []byte("admin"), "request-2", store.stored.TenantID, created.ID); err != nil || !store.revoked {
		t.Fatalf("Revoke() error=%v revoked=%v", err, store.revoked)
	}
	if err := service.Revoke(context.Background(), []byte("admin"), "request-3", uuid.New(), created.ID); !errors.Is(err, ErrRecipientNotFound) {
		t.Fatalf("cross-tenant Revoke() error=%v", err)
	}
}

func TestServiceRedeemsVerifiedIdentityAndSupportsSubsequentLogin(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 36)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	store := &memoryRecipientStore{}
	service := NewService(store, box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })
	tenantID := uuid.New()
	created, err := service.CreateInvitation(context.Background(), []byte("admin"), "request-1", "recipient-create-002", tenantID, "Owner")
	if err != nil {
		t.Fatal(err)
	}
	reference := strings.TrimPrefix(created.InvitationURL, "https://dashboard.nextstep-soft.com/app/invite?ref=")

	bound, err := service.ResolveIdentity(context.Background(), line.Identity{Subject: "U123", DisplayName: "Verified Owner"}, reference)
	if err != nil {
		t.Fatalf("ResolveIdentity() redeem error = %v", err)
	}
	if bound.Status != StatusActive || bound.DisplayName != "Verified Owner" {
		t.Fatalf("bound recipient = %+v", bound)
	}
	loggedIn, err := service.ResolveIdentity(context.Background(), line.Identity{Subject: "U123", DisplayName: "Verified Owner"}, "")
	if err != nil || loggedIn.ID != bound.ID {
		t.Fatalf("ResolveIdentity() login = %+v, %v", loggedIn, err)
	}
	outboundID, err := service.OutboundLineUserID(context.Background(), bound.ID)
	if err != nil || outboundID != "U123" {
		t.Fatalf("OutboundLineUserID() = %q, %v", outboundID, err)
	}
}
