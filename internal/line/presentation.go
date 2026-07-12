package line

import (
	"math/big"
	"strings"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

type FlexMetricPresentation struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type FlexComparisonPresentation struct {
	Text      string                     `json:"text"`
	Direction report.ComparisonDirection `json:"direction"`
}

type FlexAttentionSeverity string

const (
	FlexAttentionInfo    FlexAttentionSeverity = "INFO"
	FlexAttentionWarning FlexAttentionSeverity = "WARNING"
	FlexAttentionDanger  FlexAttentionSeverity = "DANGER"
)

type FlexAttentionPresentation struct {
	Severity FlexAttentionSeverity `json:"severity"`
	Text     string                `json:"text"`
}

type FlexDataState string

const (
	FlexDataData FlexDataState = "DATA"
	FlexDataZero FlexDataState = "ZERO"
)

type FlexReportPresentation struct {
	Key           report.Key                  `json:"key"`
	Label         string                      `json:"label"`
	CategoryLabel string                      `json:"categoryLabel"`
	Primary       FlexMetricPresentation      `json:"primary"`
	Supporting    []FlexMetricPresentation    `json:"supporting"`
	Comparison    *FlexComparisonPresentation `json:"comparison,omitempty"`
	Attention     *FlexAttentionPresentation  `json:"attention,omitempty"`
	DataState     FlexDataState               `json:"dataState"`
	StateText     string                      `json:"stateText,omitempty"`
	ActionURL     string                      `json:"actionUrl"`
}

type flexPresentationDefinition struct {
	primary    string
	supporting []string
	zeroText   string
}

var flexPresentationDefinitions = map[report.Key]flexPresentationDefinition{
	report.SalesGoodsServices:      {primary: "total_amount", supporting: []string{"document_count", "average_per_document"}, zeroText: "ไม่มีรายการขายในช่วงนี้"},
	report.PurchaseGoodsPayables:   {primary: "total_amount", supporting: []string{"document_count", "average_per_document"}, zeroText: "ไม่มีรายการซื้อในช่วงนี้"},
	report.GrossProfitByProduct:    {primary: "gross_profit_amount", supporting: []string{"gross_margin_percent", "net_amount"}, zeroText: "ไม่มีรายการขายสำหรับคำนวณกำไร"},
	report.GrossProfitByARCustomer: {primary: "gross_profit_amount", supporting: []string{"gross_margin_percent", "net_amount"}, zeroText: "ไม่มีรายการขายสำหรับคำนวณกำไร"},
	report.StockBalance:            {primary: "balance_amount", supporting: []string{"item_count"}, zeroText: "ไม่พบสินค้าคงเหลือ"},
	report.StockReorder:            {primary: "reorder_item_count", supporting: []string{"shortage_qty"}, zeroText: "ไม่มีสินค้าต่ำกว่าจุดสั่งซื้อ"},
	report.ARCustomerMovement:      {primary: "net_movement_amount", supporting: []string{"customer_count"}, zeroText: "ไม่มีความเคลื่อนไหวลูกหนี้"},
	report.ARDebtReceipt:           {primary: "total_received_amount", supporting: []string{"receipt_count", "average_per_receipt"}, zeroText: "ไม่มีรายการรับชำระหนี้"},
	report.CashBankReceipts:        {primary: "total_amount", supporting: []string{"document_count", "average_per_document"}, zeroText: "ไม่มีรายการรับเงิน"},
	report.CashBankPayments:        {primary: "total_amount", supporting: []string{"document_count", "average_per_document"}, zeroText: "ไม่มีรายการจ่ายเงิน"},
}

func BuildFlexReportPresentation(input FlexReport) (FlexReportPresentation, error) {
	definition, ok := report.DefinitionFor(input.Key)
	presentationDefinition, configured := flexPresentationDefinitions[input.Key]
	if !ok || !configured {
		return FlexReportPresentation{}, ErrFlexInputInvalid
	}
	presentation := FlexReportPresentation{
		Key: input.Key, Label: definition.LabelTH, CategoryLabel: definition.CategoryLabelTH,
		Supporting: []FlexMetricPresentation{}, DataState: FlexDataData, ActionURL: input.ActionURL,
	}

	if input.Dashboard == nil {
		metric, metricOK := legacyMetric(definition, presentationDefinition.primary, input.Metrics)
		if !metricOK {
			return FlexReportPresentation{}, ErrFlexInputInvalid
		}
		formatted, err := presentMetric(metric)
		if err != nil {
			return FlexReportPresentation{}, err
		}
		presentation.Primary = formatted
		for _, key := range presentationDefinition.supporting {
			metric, exists := legacyMetric(definition, key, input.Metrics)
			if !exists {
				continue
			}
			formatted, err := presentMetric(metric)
			if err != nil {
				return FlexReportPresentation{}, err
			}
			presentation.Supporting = append(presentation.Supporting, formatted)
		}
		presentation.Attention = &FlexAttentionPresentation{Severity: FlexAttentionInfo, Text: "ไม่มีข้อมูลเปรียบเทียบ"}
		return presentation, nil
	}
	if input.Dashboard.ReportKey != input.Key {
		return FlexReportPresentation{}, ErrFlexInputInvalid
	}

	metrics := make(map[string]report.DashboardMetric, len(input.Dashboard.KPIs))
	for _, metric := range input.Dashboard.KPIs {
		metrics[metric.Key] = metric
	}
	primary, exists := metrics[presentationDefinition.primary]
	if !exists {
		return FlexReportPresentation{}, ErrFlexInputInvalid
	}
	formattedPrimary, err := presentMetric(primary)
	if err != nil {
		return FlexReportPresentation{}, err
	}
	presentation.Primary = formattedPrimary
	presentation.Comparison, err = presentComparison(primary)
	if err != nil {
		return FlexReportPresentation{}, err
	}
	for _, key := range presentationDefinition.supporting {
		metric, exists := metrics[key]
		if !exists {
			return FlexReportPresentation{}, ErrFlexInputInvalid
		}
		formatted, err := presentMetric(metric)
		if err != nil {
			return FlexReportPresentation{}, err
		}
		presentation.Supporting = append(presentation.Supporting, formatted)
	}
	presentation.Attention = attentionFor(input.Key, metrics, input.Dashboard.Visualizations, input.Dashboard.Quality)
	if trustedZeroDashboard(input.Dashboard, presentationDefinition, metrics) {
		presentation.DataState = FlexDataZero
		presentation.StateText = presentationDefinition.zeroText
		if !comparisonHasChange(primary.Comparison) {
			presentation.Comparison = nil
		}
	}
	return presentation, nil
}

func trustedZeroDashboard(dashboard *report.Dashboard, definition flexPresentationDefinition, metrics map[string]report.DashboardMetric) bool {
	if dashboard == nil || dashboard.Quality.Status != "OK" || len(dashboard.Quality.Warnings) != 0 {
		return false
	}
	keys := append([]string{definition.primary}, definition.supporting...)
	for _, key := range keys {
		metric, exists := metrics[key]
		if !exists {
			return false
		}
		number, valid := new(big.Rat).SetString(normalizeNumber(metric.Value))
		if !valid || number.Sign() != 0 {
			return false
		}
	}
	return true
}

func comparisonHasChange(comparison report.MetricComparison) bool {
	if comparison.Availability != report.ComparisonAvailable {
		return false
	}
	for _, value := range []string{comparison.Percent, comparison.Delta} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		number, valid := new(big.Rat).SetString(normalizeNumber(value))
		if valid && number.Sign() != 0 {
			return true
		}
	}
	return false
}

func legacyMetric(definition report.Definition, key string, metrics map[string]string) (report.DashboardMetric, bool) {
	value, exists := metrics[key]
	if !exists || strings.TrimSpace(value) == "" {
		return report.DashboardMetric{}, false
	}
	label := key
	for _, item := range definition.LineMetrics {
		if item.Key == key {
			label = item.LabelTH
			break
		}
	}
	return report.DashboardMetric{Key: key, Label: label, Value: value, Unit: unitForMetricKey(key)}, true
}

func unitForMetricKey(key string) report.MetricUnit {
	if strings.Contains(key, "percent") {
		return report.UnitPercent
	}
	if strings.Contains(key, "count") {
		return report.UnitCount
	}
	if strings.Contains(key, "qty") {
		return report.UnitQuantity
	}
	return report.UnitTHB
}

func presentMetric(metric report.DashboardMetric) (FlexMetricPresentation, error) {
	value, err := formatMetricValue(metric.Value, metric.Unit)
	if err != nil {
		return FlexMetricPresentation{}, ErrFlexInputInvalid
	}
	return FlexMetricPresentation{Label: metric.Label, Value: value}, nil
}

func presentComparison(metric report.DashboardMetric) (*FlexComparisonPresentation, error) {
	comparison := metric.Comparison
	if comparison.Availability != report.ComparisonAvailable {
		return nil, nil
	}
	arrow := "→"
	if comparison.Direction == report.DirectionUp {
		arrow = "↑"
	} else if comparison.Direction == report.DirectionDown {
		arrow = "↓"
	}
	value := comparison.Percent
	unit := report.UnitPercent
	if strings.TrimSpace(value) == "" {
		value, unit = comparison.Delta, metric.Unit
	}
	formatted, err := formatMetricValue(value, unit)
	if err != nil {
		return nil, ErrFlexInputInvalid
	}
	formatted = strings.TrimPrefix(strings.TrimPrefix(formatted, "−"), "-")
	return &FlexComparisonPresentation{Text: arrow + " " + formatted + " จากช่วงก่อน", Direction: comparison.Direction}, nil
}

func attentionFor(key report.Key, metrics map[string]report.DashboardMetric, visualizations []report.DashboardVisualization, quality report.DashboardQuality) *FlexAttentionPresentation {
	if key == report.GrossProfitByProduct || key == report.GrossProfitByARCustomer {
		if metricSign(metrics["gross_profit_amount"].Value) < 0 {
			return &FlexAttentionPresentation{Severity: FlexAttentionDanger, Text: "ขาดทุนขั้นต้น"}
		}
		visualizationKey, label := "loss_products", "พบสินค้าที่ขาดทุน"
		if key == report.GrossProfitByARCustomer {
			visualizationKey, label = "loss_customers", "พบลูกค้าที่ขาดทุน"
		}
		if hasVisualizationData(visualizations, visualizationKey) {
			return &FlexAttentionPresentation{Severity: FlexAttentionWarning, Text: label}
		}
	}
	if key == report.StockReorder && metricSign(metrics["reorder_item_count"].Value) > 0 {
		return &FlexAttentionPresentation{Severity: FlexAttentionWarning, Text: "ต่ำกว่าจุดสั่งซื้อ"}
	}
	if key == report.ARDebtReceipt && metricSign(metrics["payment_split_missing_count"].Value) > 0 {
		return &FlexAttentionPresentation{Severity: FlexAttentionWarning, Text: "ข้อมูลวิธีรับชำระไม่ครบ"}
	}
	for _, warning := range quality.Warnings {
		if warning == "COMPARISON_QUERY_FAILED" {
			return &FlexAttentionPresentation{Severity: FlexAttentionInfo, Text: "ข้อมูลเปรียบเทียบไม่พร้อม"}
		}
	}
	return nil
}

func hasVisualizationData(items []report.DashboardVisualization, key string) bool {
	for _, item := range items {
		if item.Key == key && len(item.Categories) > 0 && len(item.Series) > 0 {
			return true
		}
	}
	return false
}

func metricSign(value string) int {
	number, ok := new(big.Rat).SetString(normalizeNumber(value))
	if !ok {
		return 0
	}
	return number.Sign()
}

func formatMetricValue(value string, unit report.MetricUnit) (string, error) {
	number, ok := new(big.Rat).SetString(normalizeNumber(value))
	if !ok {
		return "", ErrFlexInputInvalid
	}
	digits := 2
	if unit == report.UnitCount {
		digits = 0
	}
	raw := number.FloatString(digits)
	negative := strings.HasPrefix(raw, "-")
	raw = strings.TrimPrefix(raw, "-")
	parts := strings.SplitN(raw, ".", 2)
	parts[0] = groupThousands(parts[0])
	formatted := strings.Join(parts, ".")
	suffix := ""
	if unit == report.UnitPercent {
		suffix = "%"
	}
	if negative {
		return "−" + formatted + suffix, nil
	}
	return formatted + suffix, nil
}

func normalizeNumber(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(value), ",", ""), "−", "-")
}

func groupThousands(value string) string {
	if len(value) <= 3 {
		return value
	}
	first := len(value) % 3
	if first == 0 {
		first = 3
	}
	var result strings.Builder
	result.WriteString(value[:first])
	for index := first; index < len(value); index += 3 {
		result.WriteByte(',')
		result.WriteString(value[index : index+3])
	}
	return result.String()
}
