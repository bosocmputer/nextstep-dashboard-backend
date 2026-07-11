package line

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

func flexReport(key report.Key) FlexReport {
	definition, _ := report.DefinitionFor(key)
	metrics := make(map[string]string, len(definition.LineMetrics))
	for index, metric := range definition.LineMetrics {
		metrics[metric.Key] = []string{"12", "1,234.56"}[index]
	}
	return FlexReport{Key: key, Metrics: metrics}
}

func TestRenderFlexBuildsOneCompactPermissionFilteredBubble(t *testing.T) {
	reportURL := "https://dashboard.nextstep-soft.com/app/tenant/00000000-0000-0000-0000-000000000001/report/sales_goods_services?snapshotRunId=00000000-0000-0000-0000-000000000002&deliveryRef=opaque-reference-value"
	sales := flexReport(report.SalesGoodsServices)
	sales.ActionURL = reportURL
	input := FlexInput{
		TenantName: "ร้านตัวอย่าง", Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
		GeneratedAt: time.Date(2026, 7, 10, 15, 30, 0, 0, time.UTC), Timezone: "Asia/Bangkok",
		ActionURL: "https://dashboard.nextstep-soft.com/app?deliveryRef=opaque-reference-value",
		Reports:   []FlexReport{sales, flexReport(report.StockBalance)},
	}
	payload, err := RenderFlex(input)
	if err != nil {
		t.Fatalf("RenderFlex() error = %v", err)
	}
	if len(payload) > maximumFlexPayloadBytes || strings.Count(string(payload), `"type":"bubble"`) != 1 || !strings.Contains(string(payload), "เปิดภาพรวมร้าน") || !strings.Contains(string(payload), "สรุปผู้บริหาร") || !strings.Contains(string(payload), "ยอดขาย") {
		t.Fatalf("unexpected payload (%d bytes): %s", len(payload), payload)
	}
	if !strings.Contains(string(payload), `"size":"giga"`) || !strings.Contains(string(payload), "snapshotRunId=00000000-0000-0000-0000-000000000002") || !strings.Contains(string(payload), "22:30 เวลาไทย") || strings.Contains(string(payload), "UTC") {
		t.Fatalf("executive layout, deep link, or timezone missing: %s", payload)
	}
	if !strings.Contains(string(payload), "#1D4ED8") || strings.Contains(string(payload), "#0F766E") || strings.Contains(string(payload), "฿") {
		t.Fatalf("blue palette or symbol-free values missing: %s", payload)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is invalid JSON: %v", err)
	}
}

func TestRenderFlexSupportsTenReportsButRejectsElevenOrIncompleteMetrics(t *testing.T) {
	keys := report.Keys()
	input := FlexInput{
		TenantName: "Shop", Period: report.Period{DateFrom: "2026-07-01", DateTo: "2026-07-10"}, GeneratedAt: time.Now(),
		ActionURL: "https://dashboard.nextstep-soft.com/app?deliveryRef=opaque",
	}
	for _, key := range keys {
		item := flexReport(key)
		dashboard := previewDashboard(key, input.Period)
		item.Dashboard = &dashboard
		item.ActionURL = "https://dashboard.nextstep-soft.com/app/tenant/00000000-0000-0000-0000-000000000001/report/" + string(key)
		input.Reports = append(input.Reports, item)
	}
	if _, err := RenderFlex(input); err != nil {
		t.Fatalf("ten reports rejected: %v", err)
	}
	if payload, err := RenderFlex(input); err != nil || len(payload) > softFlexPayloadBytes {
		t.Fatalf("ten-report payload = %d bytes, err = %v", len(payload), err)
	}
	input.Reports = append(input.Reports, flexReport(keys[0]))
	if _, err := RenderFlex(input); err == nil {
		t.Fatal("eleven reports accepted in one bubble")
	}
	input.Reports = []FlexReport{{Key: report.SalesGoodsServices, Metrics: map[string]string{"document_count": "1"}}}
	if _, err := RenderFlex(input); err == nil {
		t.Fatal("incomplete approved metrics accepted")
	}
}

func BenchmarkRenderFlexTenReports(b *testing.B) {
	input := FlexInput{
		TenantName: "ร้านตัวอย่าง", Timezone: "Asia/Bangkok",
		Period:      report.Period{Preset: report.MonthToDate, DateFrom: "2026-07-01", DateTo: "2026-07-10"},
		GeneratedAt: time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC), ActionURL: "https://dashboard.nextstep-soft.com/app",
	}
	for _, key := range report.Keys() {
		item := flexReport(key)
		dashboard := previewDashboard(key, input.Period)
		item.Dashboard = &dashboard
		item.ActionURL = "https://dashboard.nextstep-soft.com/app/tenant/00000000-0000-0000-0000-000000000001/report/" + string(key)
		input.Reports = append(input.Reports, item)
	}
	b.ReportAllocs()
	for range b.N {
		if _, err := RenderFlex(input); err != nil {
			b.Fatal(err)
		}
	}
}

func TestRenderFlexRejectsNonHTTPSAction(t *testing.T) {
	input := FlexInput{
		TenantName: "Shop", Period: report.Period{DateFrom: "2026-07-10", DateTo: "2026-07-10"}, GeneratedAt: time.Now(),
		ActionURL: "http://dashboard.nextstep-soft.com/app", Reports: []FlexReport{flexReport(report.SalesGoodsServices)},
	}
	if _, err := RenderFlex(input); err == nil {
		t.Fatal("non-HTTPS action accepted")
	}
}

func TestRenderFlexRejectsReportActionOutsideConfiguredDashboardHost(t *testing.T) {
	item := flexReport(report.SalesGoodsServices)
	item.ActionURL = "https://example.com/app/tenant/t/report/sales_goods_services"
	input := FlexInput{
		TenantName: "Shop", Period: report.Period{DateFrom: "2026-07-10", DateTo: "2026-07-10"}, GeneratedAt: time.Now(),
		ActionURL: "https://dashboard.nextstep-soft.com/app", Reports: []FlexReport{item},
	}
	if _, err := RenderFlex(input); err == nil {
		t.Fatal("cross-host report action accepted")
	}
}
