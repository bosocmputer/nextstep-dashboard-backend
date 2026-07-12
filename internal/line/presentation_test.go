package line

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

func TestBuildFlexReportPresentationCompactsTrustedZeroDataForEveryReport(t *testing.T) {
	wantText := map[report.Key]string{
		report.SalesGoodsServices:      "ไม่มีรายการขายในช่วงนี้",
		report.PurchaseGoodsPayables:   "ไม่มีรายการซื้อในช่วงนี้",
		report.GrossProfitByProduct:    "ไม่มีรายการขายสำหรับคำนวณกำไร",
		report.GrossProfitByARCustomer: "ไม่มีรายการขายสำหรับคำนวณกำไร",
		report.StockBalance:            "ไม่พบสินค้าคงเหลือ",
		report.StockReorder:            "ไม่มีสินค้าต่ำกว่าจุดสั่งซื้อ",
		report.ARCustomerMovement:      "ไม่มีความเคลื่อนไหวลูกหนี้",
		report.ARDebtReceipt:           "ไม่มีรายการรับชำระหนี้",
		report.CashBankReceipts:        "ไม่มีรายการรับเงิน",
		report.CashBankPayments:        "ไม่มีรายการจ่ายเงิน",
	}
	for _, key := range report.Keys() {
		dashboard := previewDashboard(key, report.Period{Preset: report.Yesterday, DateFrom: "2026-07-12", DateTo: "2026-07-12"})
		for index := range dashboard.KPIs {
			dashboard.KPIs[index].Value = []string{"0", "0.00", "-0.00"}[index%3]
		}
		presentation, err := BuildFlexReportPresentation(FlexReport{Key: key, Dashboard: &dashboard, ActionURL: "https://dashboard.nextstep-soft.com/app"})
		if err != nil {
			t.Fatalf("%s: BuildFlexReportPresentation() error = %v", key, err)
		}
		if presentation.DataState != FlexDataZero || presentation.StateText != wantText[key] {
			t.Errorf("%s: state=%q text=%q", key, presentation.DataState, presentation.StateText)
		}
	}
}

func TestBuildFlexReportPresentationDoesNotHideUntrustedOrNonzeroData(t *testing.T) {
	period := report.Period{Preset: report.Yesterday, DateFrom: "2026-07-12", DateTo: "2026-07-12"}
	for _, test := range []struct {
		name      string
		dashboard *report.Dashboard
	}{
		{name: "legacy", dashboard: nil},
		{name: "partial quality", dashboard: func() *report.Dashboard {
			item := previewDashboard(report.SalesGoodsServices, period)
			item.Quality.Status = "PARTIAL"
			for i := range item.KPIs {
				item.KPIs[i].Value = "0"
			}
			return &item
		}()},
		{name: "quality warning", dashboard: func() *report.Dashboard {
			item := previewDashboard(report.SalesGoodsServices, period)
			item.Quality.Warnings = []string{"COMPARISON_QUERY_FAILED"}
			for i := range item.KPIs {
				item.KPIs[i].Value = "0"
			}
			return &item
		}()},
		{name: "supporting nonzero", dashboard: func() *report.Dashboard {
			item := previewDashboard(report.SalesGoodsServices, period)
			for i := range item.KPIs {
				item.KPIs[i].Value = "0"
			}
			item.KPIs[1].Value = "1"
			return &item
		}()},
		{name: "negative", dashboard: func() *report.Dashboard {
			item := previewDashboard(report.SalesGoodsServices, period)
			for i := range item.KPIs {
				item.KPIs[i].Value = "0"
			}
			item.KPIs[0].Value = "-0.01"
			return &item
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := FlexReport{Key: report.SalesGoodsServices, Dashboard: test.dashboard, ActionURL: "https://dashboard.nextstep-soft.com/app"}
			if test.dashboard == nil {
				input.Metrics = map[string]string{"total_amount": "0", "document_count": "0"}
			}
			presentation, err := BuildFlexReportPresentation(input)
			if err != nil {
				t.Fatalf("BuildFlexReportPresentation() error = %v", err)
			}
			if presentation.DataState == FlexDataZero || presentation.StateText != "" {
				t.Fatalf("untrusted/nonzero data was hidden: %+v", presentation)
			}
		})
	}
}

func TestBuildFlexReportPresentationKeepsNonzeroComparisonForTrustedZeroData(t *testing.T) {
	period := report.Period{Preset: report.Yesterday, DateFrom: "2026-07-12", DateTo: "2026-07-12"}
	dashboard := previewDashboard(report.SalesGoodsServices, period)
	for index := range dashboard.KPIs {
		dashboard.KPIs[index].Value = "0"
	}
	dashboard.KPIs[0].Comparison = report.MetricComparison{
		Availability:  report.ComparisonAvailable,
		PreviousValue: "100.00",
		Delta:         "-100.00",
		Percent:       "-100.00",
		Direction:     report.DirectionDown,
	}
	presentation, err := BuildFlexReportPresentation(FlexReport{Key: report.SalesGoodsServices, Dashboard: &dashboard, ActionURL: "https://dashboard.nextstep-soft.com/app"})
	if err != nil {
		t.Fatalf("BuildFlexReportPresentation() error = %v", err)
	}
	if presentation.DataState != FlexDataZero || presentation.Comparison == nil || presentation.Comparison.Text != "↓ 100.00% จากช่วงก่อน" {
		t.Fatalf("zero state lost a meaningful comparison: %+v", presentation)
	}
}

func TestBuildFlexReportPresentationUsesExecutiveSalesMetrics(t *testing.T) {
	dashboard := report.Dashboard{
		ReportKey: report.SalesGoodsServices,
		KPIs: []report.DashboardMetric{
			{Key: "total_amount", Label: "ยอดขาย", Value: "604058.00", Unit: report.UnitTHB, Comparison: report.MetricComparison{Availability: report.ComparisonAvailable, Percent: "-7.82", Direction: report.DirectionDown}},
			{Key: "document_count", Label: "จำนวนเอกสาร", Value: "741", Unit: report.UnitCount},
			{Key: "average_per_document", Label: "ยอดเฉลี่ยต่อเอกสาร", Value: "815.19", Unit: report.UnitTHB},
		},
	}
	presentation, err := BuildFlexReportPresentation(FlexReport{Key: report.SalesGoodsServices, Dashboard: &dashboard, ActionURL: "https://dashboard.nextstep-soft.com/app/tenant/t/report/sales_goods_services"})
	if err != nil {
		t.Fatalf("BuildFlexReportPresentation() error = %v", err)
	}
	if presentation.Primary.Label != "ยอดขาย" || presentation.Primary.Value != "604,058.00" {
		t.Fatalf("primary = %+v", presentation.Primary)
	}
	if presentation.Comparison == nil || presentation.Comparison.Text != "↓ 7.82% จากช่วงก่อน" || presentation.Comparison.Direction != report.DirectionDown {
		t.Fatalf("comparison = %+v", presentation.Comparison)
	}
	if len(presentation.Supporting) != 2 || presentation.Supporting[0].Value != "741" || presentation.Supporting[1].Value != "815.19" {
		t.Fatalf("supporting = %+v", presentation.Supporting)
	}
	if presentation.Attention != nil {
		t.Fatalf("unexpected attention = %+v", presentation.Attention)
	}
}

func TestBuildFlexReportPresentationFlagsLossWithoutLeakingEntityNames(t *testing.T) {
	dashboard := report.Dashboard{
		ReportKey: report.GrossProfitByProduct,
		KPIs: []report.DashboardMetric{
			{Key: "gross_profit_amount", Label: "กำไรขั้นต้น", Value: "-79825.88", Unit: report.UnitTHB, Comparison: report.MetricComparison{Availability: report.ComparisonAvailable, Percent: "-111.45", Direction: report.DirectionDown}},
			{Key: "gross_margin_percent", Label: "อัตรากำไรขั้นต้น", Value: "-1.10", Unit: report.UnitPercent},
			{Key: "net_amount", Label: "ยอดขายสุทธิ", Value: "7251151.47", Unit: report.UnitTHB},
		},
		Visualizations: []report.DashboardVisualization{{Key: "loss_products", Categories: []string{"สินค้าลับ"}, Series: []report.VisualizationSeries{{Key: "value", Values: []string{"-999.00"}}}}},
	}
	presentation, err := BuildFlexReportPresentation(FlexReport{Key: report.GrossProfitByProduct, Dashboard: &dashboard, ActionURL: "https://dashboard.nextstep-soft.com/app"})
	if err != nil {
		t.Fatalf("BuildFlexReportPresentation() error = %v", err)
	}
	if presentation.Attention == nil || presentation.Attention.Severity != FlexAttentionDanger || presentation.Attention.Text != "ขาดทุนขั้นต้น" {
		t.Fatalf("attention = %+v", presentation.Attention)
	}
	if presentation.Primary.Value != "−79,825.88" || len(presentation.Supporting) != 2 || presentation.Supporting[0].Value != "−1.10%" {
		t.Fatalf("presentation = %+v", presentation)
	}
	encoded, _ := json.Marshal(presentation)
	if strings.Contains(string(encoded), "สินค้าลับ") {
		t.Fatal("presentation leaked a product name")
	}
}

func TestBuildFlexReportPresentationMakesCurrentOnlyReorderExplicit(t *testing.T) {
	dashboard := report.Dashboard{
		ReportKey: report.StockReorder,
		KPIs: []report.DashboardMetric{
			{Key: "reorder_item_count", Label: "สินค้าที่ต้องสั่ง", Value: "2985", Unit: report.UnitCount, Comparison: report.MetricComparison{Availability: report.ComparisonUnavailable}},
			{Key: "shortage_qty", Label: "จำนวนขาดรวม", Value: "137666.6290", Unit: report.UnitQuantity},
		},
	}
	presentation, err := BuildFlexReportPresentation(FlexReport{Key: report.StockReorder, Dashboard: &dashboard, ActionURL: "https://dashboard.nextstep-soft.com/app"})
	if err != nil {
		t.Fatalf("BuildFlexReportPresentation() error = %v", err)
	}
	if presentation.Comparison != nil {
		t.Fatalf("current-only report comparison = %+v", presentation.Comparison)
	}
	if presentation.Attention == nil || presentation.Attention.Severity != FlexAttentionWarning || presentation.Attention.Text != "ต่ำกว่าจุดสั่งซื้อ" {
		t.Fatalf("attention = %+v", presentation.Attention)
	}
	if len(presentation.Supporting) != 1 || presentation.Supporting[0].Value != "137,666.63" {
		t.Fatalf("supporting = %+v", presentation.Supporting)
	}
}

func TestBuildFlexReportPresentationFallsBackForLegacySummary(t *testing.T) {
	presentation, err := BuildFlexReportPresentation(FlexReport{
		Key:       report.SalesGoodsServices,
		Metrics:   map[string]string{"document_count": "12", "total_amount": "1234.50"},
		ActionURL: "https://dashboard.nextstep-soft.com/app",
	})
	if err != nil {
		t.Fatalf("BuildFlexReportPresentation() error = %v", err)
	}
	if presentation.Primary.Value != "1,234.50" || len(presentation.Supporting) != 1 || presentation.Comparison != nil || presentation.Attention == nil || presentation.Attention.Text != "ไม่มีข้อมูลเปรียบเทียบ" {
		t.Fatalf("legacy presentation = %+v", presentation)
	}
}
