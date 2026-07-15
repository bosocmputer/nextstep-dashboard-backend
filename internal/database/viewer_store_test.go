package database

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestViewerStoreFiltersActiveTenantAndReportPermissions(t *testing.T) {
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
	tenantID, noReportTenantID, expiredTenantID, recipientID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into tenants (id, slug, name, timezone, status, access_ends_at) values
		($1, 'viewer-active', 'Active Shop', 'Asia/Bangkok', 'ACTIVE', $4),
		($2, 'viewer-no-reports', 'Pending Reports Shop', 'Asia/Bangkok', 'ACTIVE', $4),
		($3, 'viewer-expired', 'Expired Shop', 'Asia/Bangkok', 'ACTIVE', $5)`,
		tenantID, noReportTenantID, expiredTenantID, now.AddDate(1, 0, 0), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into line_recipients (id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, display_name_ciphertext, display_name_nonce, encryption_key_id, status, verified_at)
		values ($1, decode('01', 'hex'), decode('02', 'hex'), decode('03', 'hex'), decode('04', 'hex'), decode('05', 'hex'), 'key', 'ACTIVE', $2)`,
		recipientID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into tenant_memberships (tenant_id, recipient_id, status) values
		($1, $4, 'ACTIVE'), ($2, $4, 'ACTIVE'), ($3, $4, 'ACTIVE')`,
		tenantID, noReportTenantID, expiredTenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		insert into recipient_report_permissions (tenant_id, recipient_id, report_key) values
		($1, $3, 'sales_goods_services'), ($1, $3, 'stock_balance'), ($2, $3, 'cash_bank_receipts')`,
		tenantID, expiredTenantID, recipientID); err != nil {
		t.Fatal(err)
	}
	store := NewViewerStore(pool)
	session := viewer.SessionRecord{TokenHash: []byte("viewer-token"), RecipientID: recipientID, CSRFHash: []byte("csrf"), ExpiresAt: now.Add(time.Hour)}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.FindSession(ctx, session.TokenHash, now)
	if err != nil || loaded.RecipientID != recipientID {
		t.Fatalf("FindSession() = %+v, %v", loaded, err)
	}
	tenants, err := store.ListTenants(ctx, recipientID, now)
	if err != nil || len(tenants) != 2 || tenants[0].ID != tenantID || len(tenants[0].ReportKeys) != 2 {
		t.Fatalf("ListTenants() = %+v, %v", tenants, err)
	}
	if tenants[1].ID != noReportTenantID || len(tenants[1].ReportKeys) != 0 {
		t.Fatalf("ListTenants() tenant without reports = %+v", tenants[1])
	}
	reports, err := store.ListReports(ctx, recipientID, tenantID, now)
	if err != nil || len(reports) != 2 {
		t.Fatalf("ListReports() = %+v, %v", reports, err)
	}
	allowed, err := store.CanAccessReport(ctx, recipientID, tenantID, report.StockBalance, now)
	if err != nil || !allowed {
		t.Fatalf("CanAccessReport(stock) = %v, %v", allowed, err)
	}
	allowed, err = store.CanAccessReport(ctx, recipientID, tenantID, report.CashBankPayments, now)
	if err != nil || allowed {
		t.Fatalf("CanAccessReport(cash) = %v, %v", allowed, err)
	}
	if _, err := pool.Exec(ctx, `update line_recipients set status = 'REVOKED' where id = $1`, recipientID); err != nil {
		t.Fatal(err)
	}
	if tenants, err = store.ListTenants(ctx, recipientID, now); err != nil || len(tenants) != 0 {
		t.Fatalf("ListTenants() after recipient revocation = %+v, %v", tenants, err)
	}
	if reports, err = store.ListReports(ctx, recipientID, tenantID, now); err != nil || len(reports) != 0 {
		t.Fatalf("ListReports() after recipient revocation = %+v, %v", reports, err)
	}
	if allowed, err = store.CanAccessReport(ctx, recipientID, tenantID, report.StockBalance, now); err != nil || allowed {
		t.Fatalf("CanAccessReport() after recipient revocation = %v, %v", allowed, err)
	}
	if _, err := pool.Exec(ctx, `update line_recipients set status = 'ACTIVE' where id = $1`, recipientID); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeSession(ctx, session.TokenHash, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindSession(ctx, session.TokenHash, now); err == nil {
		t.Fatal("revoked session remained valid")
	}
}
