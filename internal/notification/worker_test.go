package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type memoryNotificationStore struct {
	work       Work
	deferred   bool
	failedCode string
	published  []PreparedDelivery
	partial    bool
}

func (store *memoryNotificationStore) Claim(context.Context, string, time.Duration, time.Time) (Work, error) {
	return store.work, nil
}

func (store *memoryNotificationStore) Defer(context.Context, uuid.UUID, string, time.Time, time.Time) error {
	store.deferred = true
	return nil
}

func (store *memoryNotificationStore) Fail(_ context.Context, _ uuid.UUID, _, safeCode string, _ time.Time) error {
	store.failedCode = safeCode
	return nil
}

func (store *memoryNotificationStore) Publish(_ context.Context, _ uuid.UUID, _ string, deliveries []PreparedDelivery, partial bool, _ time.Time) error {
	store.published = deliveries
	store.partial = partial
	return nil
}

func testNotificationWorker(t *testing.T, store Store, now time.Time) *Worker {
	t.Helper()
	tokens, err := auth.NewSessionManager(bytes.Repeat([]byte{1}, 32), bytes.NewReader(bytes.Repeat([]byte{2}, 256)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	baseURL, _ := url.Parse("https://dashboard.nextstep-soft.com")
	return NewWorker(store, line.RenderFlex, tokens, bytes.NewReader(bytes.Repeat([]byte{3}, 256)), baseURL, "notification-worker", func() time.Time { return now })
}

func TestWorkerDefersWhileReportsAreRunning(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &memoryNotificationStore{work: Work{ID: uuid.New(), Pending: true}}
	if err := testNotificationWorker(t, store, now).ProcessOne(context.Background()); err != nil || !store.deferred || len(store.published) != 0 {
		t.Fatalf("ProcessOne() error=%v deferred=%v published=%d", err, store.deferred, len(store.published))
	}
}

func TestWorkerPublishesOneCompleteCardPerRecipient(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	recipientID := uuid.New()
	period := report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"}
	tenantID := uuid.New()
	salesRunID, stockRunID := uuid.New(), uuid.New()
	store := &memoryNotificationStore{work: Work{
		ID: uuid.New(), TenantID: tenantID, TenantName: "Shop", Timezone: "Asia/Bangkok", Partial: true,
		Reports: []ReportResult{
			{RunID: salesRunID, Key: report.SalesGoodsServices, Period: period, FinishedAt: now, Metrics: map[string]string{"document_count": "2", "total_amount": "30.00"}},
			{RunID: stockRunID, Key: report.StockBalance, Period: period, FinishedAt: now, Metrics: map[string]string{"item_count": "10", "balance_amount": "99.00"}},
		},
		Targets: []Target{{RecipientID: recipientID, ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance}}},
	}}
	if err := testNotificationWorker(t, store, now).ProcessOne(context.Background()); err != nil {
		t.Fatalf("ProcessOne() error = %v", err)
	}
	if len(store.published) != 1 || !store.partial || store.published[0].RecipientID != recipientID || len(store.published[0].ReferenceHash) == 0 {
		t.Fatalf("published = %+v partial=%v", store.published, store.partial)
	}
	payload := string(store.published[0].Payload)
	if !strings.Contains(payload, "รายงานสต็อกคงเหลือ") || !strings.Contains(payload, "ยอดขาย") || !strings.Contains(payload, "deliveryRef") || !strings.Contains(payload, salesRunID.String()) || !strings.Contains(payload, "/app/tenant/"+tenantID.String()) || !strings.Contains(payload, "10 ก.ค. 2569 · 15:00 น. เวลาไทย") || strings.Contains(payload, "UTC") {
		t.Fatalf("complete card rendering failed: %s", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(store.published[0].Payload, &decoded); err != nil {
		t.Fatalf("payload invalid: %v", err)
	}
}

func TestWorkerRefusesAReportSubset(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	period := report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"}
	store := &memoryNotificationStore{work: Work{
		ID: uuid.New(), TenantName: "Shop",
		Reports: []ReportResult{{RunID: uuid.New(), Key: report.SalesGoodsServices, Period: period, FinishedAt: now, Metrics: map[string]string{"document_count": "2", "total_amount": "30.00"}}},
		Targets: []Target{{RecipientID: uuid.New(), ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance}}},
	}}
	if err := testNotificationWorker(t, store, now).ProcessOne(context.Background()); err != nil || store.failedCode != "NO_ELIGIBLE_RECIPIENTS" || len(store.published) != 0 {
		t.Fatalf("ProcessOne() error=%v failedCode=%q published=%d", err, store.failedCode, len(store.published))
	}
}

func TestWorkerRefusesMismatchedReportPeriods(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &memoryNotificationStore{work: Work{
		ID: uuid.New(), TenantID: uuid.New(), TenantName: "Shop", Timezone: "Asia/Bangkok",
		Reports: []ReportResult{
			{RunID: uuid.New(), Key: report.SalesGoodsServices, Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"}, FinishedAt: now, Metrics: map[string]string{"document_count": "2", "total_amount": "30.00"}},
			{RunID: uuid.New(), Key: report.StockBalance, Period: report.Period{Preset: report.AsOfRun, DateFrom: "2026-07-10", DateTo: "2026-07-10"}, FinishedAt: now, Metrics: map[string]string{"item_count": "10", "balance_amount": "99.00"}},
		},
		Targets: []Target{{RecipientID: uuid.New(), ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance}}},
	}}
	if err := testNotificationWorker(t, store, now).ProcessOne(context.Background()); err != nil || store.failedCode != "FLEX_REPORT_CONTEXT_INVALID" || len(store.published) != 0 {
		t.Fatalf("ProcessOne() error=%v failedCode=%q published=%d", err, store.failedCode, len(store.published))
	}
}

func TestWorkerFailsSafelyWhenNoReportOrRecipientCanBeDelivered(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		work Work
		code string
	}{
		{work: Work{ID: uuid.New()}, code: "ALL_REPORTS_FAILED"},
		{work: Work{ID: uuid.New(), Reports: []ReportResult{{Key: report.SalesGoodsServices}}}, code: "NO_ELIGIBLE_RECIPIENTS"},
	} {
		store := &memoryNotificationStore{work: test.work}
		if err := testNotificationWorker(t, store, now).ProcessOne(context.Background()); err != nil || store.failedCode != test.code {
			t.Fatalf("ProcessOne() error=%v failedCode=%q", err, store.failedCode)
		}
	}
}
