package database

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func BenchmarkReportStoreComplete10kRows(benchmark *testing.B) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		benchmark.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		benchmark.Fatal(err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool); err != nil {
		benchmark.Fatal(err)
	}
	store := NewReportStore(pool)
	rows := make([]map[string]string, 10_000)
	for index := range rows {
		rows[index] = map[string]string{"doc_no": fmt.Sprintf("DOC-%06d", index), "total_amount": "123.45"}
	}

	benchmark.ResetTimer()
	for iteration := 0; iteration < benchmark.N; iteration++ {
		benchmark.StopTimer()
		now := time.Now().UTC()
		tenantID := uuid.New()
		recipientID := uuid.New()
		slug := "bench-" + tenantID.String()
		if _, err := pool.Exec(ctx, `insert into tenants (id, slug, name, timezone, status, access_ends_at) values ($1, $2, 'Benchmark', 'Asia/Bangkok', 'ACTIVE', $3)`, tenantID, slug, now.Add(time.Hour)); err != nil {
			benchmark.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			insert into line_recipients (id, line_user_id_hash, line_user_id_ciphertext, line_user_id_nonce, display_name_ciphertext, display_name_nonce, encryption_key_id, status, verified_at)
			values ($1, $3, decode('22', 'hex'), decode('23', 'hex'), decode('24', 'hex'), decode('25', 'hex'), 'key', 'ACTIVE', $2)`, recipientID, now, recipientID[:]); err != nil {
			benchmark.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into tenant_memberships (tenant_id, recipient_id, status) values ($1, $2, 'ACTIVE')`, tenantID, recipientID); err != nil {
			benchmark.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `insert into recipient_report_permissions (tenant_id, recipient_id, report_key) values ($1, $2, 'cash_bank_receipts')`, tenantID, recipientID); err != nil {
			benchmark.Fatal(err)
		}
		run, err := store.Enqueue(ctx, report.EnqueueInput{TenantID: tenantID, ReportKey: report.CashBankReceipts, Source: report.SourceDashboard, IdempotencyKey: "benchmark-" + uuid.NewString(), Period: report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-10"}, RequestedByRecipient: &recipientID}, now)
		if err != nil {
			benchmark.Fatal(err)
		}
		claimed, err := store.Claim(ctx, "benchmark-worker", time.Minute, now)
		if err != nil || claimed.ID != run.ID {
			benchmark.Fatalf("claim = %s, %v", claimed.ID, err)
		}
		if err := store.MarkRunning(ctx, run.ID, "benchmark-worker", time.Minute, now); err != nil {
			benchmark.Fatal(err)
		}
		benchmark.StartTimer()
		if err := store.Complete(ctx, run.ID, "benchmark-worker", report.SummaryResult{Rows: rows, RowCount: len(rows), Metrics: map[string]string{"total_amount": "1234500.00"}, Reconciliation: map[string]any{"status": "OK"}}, true, now); err != nil {
			benchmark.Fatal(err)
		}
	}
}
