package database

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRecipientStoreInvitationPermissionAndIdentityMerge(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1, 'recipient-store', 'Recipient Store', 'Asia/Bangkok', 'ACTIVE', $2)`, tenantID, now.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	box, _ := secret.NewBox(bytes.Repeat([]byte{1}, 32), "key-1", bytes.NewReader(bytes.Repeat([]byte{2}, 60)))
	tokens, _ := auth.NewSessionManager(bytes.Repeat([]byte{3}, 32), bytes.NewReader(nil), func() time.Time { return now })
	service := recipient.NewService(NewRecipientStore(pool), box, tokens, bytes.NewReader(bytes.Repeat([]byte{4}, 32)), "https://dashboard.nextstep-soft.com", func() time.Time { return now })

	pending, err := service.CreateInvitation(ctx, []byte("admin"), "request-1", "recipient-store-create-001", tenantID, "Owner")
	if err != nil {
		t.Fatalf("CreateInvitation() error = %v", err)
	}
	replayed, err := service.CreateInvitation(ctx, []byte("admin"), "request-1-retry", "recipient-store-create-001", tenantID, "Owner")
	if err != nil || replayed.ID != pending.ID || replayed.InvitationURL != pending.InvitationURL {
		t.Fatalf("CreateInvitation() replay = %+v, %v", replayed, err)
	}
	updated, err := service.ReplacePermissions(ctx, []byte("admin"), "request-2", tenantID, pending.ID, []report.Key{report.SalesGoodsServices, report.StockBalance}, pending.PermissionsVersion)
	if err != nil {
		t.Fatalf("ReplacePermissions() error = %v", err)
	}
	if updated.PermissionsVersion != pending.PermissionsVersion+1 {
		t.Fatalf("permissions version = %d, want %d", updated.PermissionsVersion, pending.PermissionsVersion+1)
	}
	if _, err := service.ReplacePermissions(ctx, []byte("admin"), "request-stale", tenantID, pending.ID, []report.Key{report.SalesGoodsServices}, pending.PermissionsVersion); !errors.Is(err, recipient.ErrVersionConflict) {
		t.Fatalf("stale ReplacePermissions() error = %v", err)
	}
	if _, err := service.GetForTenant(ctx, uuid.New(), pending.ID); !errors.Is(err, recipient.ErrRecipientNotFound) {
		t.Fatalf("cross-tenant GetForTenant() error = %v", err)
	}
	page, err := service.List(ctx, tenantID, 25, "")
	if err != nil || len(page.Data) != 1 || page.Data[0].Status != recipient.StatusPending || len(page.Data[0].ReportKeys) != 2 {
		t.Fatalf("List() pending = %+v, %v", page, err)
	}
	parsedURL, _ := url.Parse(pending.InvitationURL)
	reference := parsedURL.Query().Get("ref")
	bound, err := service.ResolveIdentity(ctx, line.Identity{Subject: "U123", DisplayName: "Verified Owner"}, reference)
	if err != nil || bound.Status != recipient.StatusActive {
		t.Fatalf("ResolveIdentity() = %+v, %v", bound, err)
	}
	page, err = service.List(ctx, tenantID, 25, "")
	if err != nil || len(page.Data) != 1 || page.Data[0].ID != bound.ID || page.Data[0].DisplayName != "Verified Owner" || len(page.Data[0].ReportKeys) != 2 {
		t.Fatalf("List() active = %+v, %v", page, err)
	}

	var rawPII string
	if err := pool.QueryRow(ctx, `select coalesce(encode(line_user_id_ciphertext, 'escape'), '') || coalesce(encode(display_name_ciphertext, 'escape'), '') from line_recipients where id = $1`, bound.ID).Scan(&rawPII); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawPII, "U123") || strings.Contains(rawPII, "Verified Owner") {
		t.Fatalf("recipient PII stored in plaintext: %q", rawPII)
	}

	if err := service.Revoke(ctx, []byte("admin"), "request-revoke", tenantID, bound.ID); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	page, err = service.List(ctx, tenantID, 25, "")
	if err != nil || len(page.Data) != 0 {
		t.Fatalf("List() after revoke = %+v, %v", page, err)
	}
	if _, err := service.GetForTenant(ctx, tenantID, bound.ID); !errors.Is(err, recipient.ErrRecipientNotFound) {
		t.Fatalf("GetForTenant() after revoke error = %v", err)
	}
	var membershipStatus string
	var permissionCount, auditCount int
	if err := pool.QueryRow(ctx, `select status from tenant_memberships where tenant_id = $1 and recipient_id = $2`, tenantID, bound.ID).Scan(&membershipStatus); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from recipient_report_permissions where tenant_id = $1 and recipient_id = $2`, tenantID, bound.ID).Scan(&permissionCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from audit_logs where tenant_id = $1 and action = 'RECIPIENT_REVOKED' and resource_id = $2`, tenantID, bound.ID.String()).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if membershipStatus != string(recipient.StatusRevoked) || permissionCount != 0 || auditCount != 1 {
		t.Fatalf("revoke state status=%s permissions=%d audits=%d", membershipStatus, permissionCount, auditCount)
	}
}
