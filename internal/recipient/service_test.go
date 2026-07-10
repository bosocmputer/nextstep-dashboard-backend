package recipient

import (
	"bytes"
	"context"
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
}

func (store *memoryRecipientStore) CreateInvitation(_ context.Context, _ []byte, _, _ string, _ []byte, pending StoredRecipient, inviteHash []byte, _ time.Time, _ time.Time) (StoredRecipient, error) {
	store.stored = pending
	store.inviteHash = append([]byte(nil), inviteHash...)
	return pending, nil
}

func (store *memoryRecipientStore) List(context.Context, uuid.UUID, int, string) (Page, error) {
	return Page{Stored: []StoredRecipient{store.stored}}, nil
}

func (store *memoryRecipientStore) ReplacePermissions(context.Context, []byte, string, uuid.UUID, uuid.UUID, []report.Key, time.Time) error {
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
