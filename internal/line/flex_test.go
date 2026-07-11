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
	input := FlexInput{
		TenantName: "ร้านตัวอย่าง", Period: report.Period{Preset: report.Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
		GeneratedAt: time.Date(2026, 7, 10, 15, 30, 0, 0, time.FixedZone("ICT", 7*60*60)),
		ActionURL:   "https://dashboard.nextstep-soft.com/app?deliveryRef=opaque-reference-value",
		Reports:     []FlexReport{flexReport(report.SalesGoodsServices), flexReport(report.StockBalance)},
	}
	payload, err := RenderFlex(input)
	if err != nil {
		t.Fatalf("RenderFlex() error = %v", err)
	}
	if len(payload) > maximumFlexPayloadBytes || strings.Count(string(payload), `"type":"bubble"`) != 1 || !strings.Contains(string(payload), "เปิดรายงาน") || !strings.Contains(string(payload), "ยอดขาย") {
		t.Fatalf("unexpected payload (%d bytes): %s", len(payload), payload)
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
		input.Reports = append(input.Reports, flexReport(key))
	}
	if _, err := RenderFlex(input); err != nil {
		t.Fatalf("ten reports rejected: %v", err)
	}
	if payload, err := RenderFlex(input); err != nil || len(payload) > 30*1024 {
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

func TestRenderFlexRejectsNonHTTPSAction(t *testing.T) {
	input := FlexInput{
		TenantName: "Shop", Period: report.Period{DateFrom: "2026-07-10", DateTo: "2026-07-10"}, GeneratedAt: time.Now(),
		ActionURL: "http://dashboard.nextstep-soft.com/app", Reports: []FlexReport{flexReport(report.SalesGoodsServices)},
	}
	if _, err := RenderFlex(input); err == nil {
		t.Fatal("non-HTTPS action accepted")
	}
}
