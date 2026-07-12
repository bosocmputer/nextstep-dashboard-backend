package line

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/google/uuid"
)

type FlexPreviewValidationError struct {
	Field string
	Code  string
}

func (err *FlexPreviewValidationError) Error() string { return err.Field + ": " + err.Code }

type FlexPreviewInput struct {
	PeriodPreset report.Preset `json:"periodPreset"`
	ReportKeys   []report.Key  `json:"reportKeys"`
}

type FlexPreviewMetric struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

type FlexPreviewReport struct {
	Key           report.Key                  `json:"key"`
	Label         string                      `json:"label"`
	CategoryLabel string                      `json:"categoryLabel"`
	Metrics       []FlexPreviewMetric         `json:"metrics"`
	Primary       FlexMetricPresentation      `json:"primary"`
	Supporting    []FlexMetricPresentation    `json:"supporting"`
	Comparison    *FlexComparisonPresentation `json:"comparison,omitempty"`
	Attention     *FlexAttentionPresentation  `json:"attention,omitempty"`
	DataState     FlexDataState               `json:"dataState,omitempty"`
	StateText     string                      `json:"stateText,omitempty"`
	ActionURL     string                      `json:"actionUrl"`
}

type FlexPreview struct {
	PresentationVersion string              `json:"presentationVersion,omitempty"`
	AltText             string              `json:"altText"`
	TenantName          string              `json:"tenantName"`
	Period              report.Period       `json:"period"`
	PeriodLabel         string              `json:"periodLabel"`
	GeneratedAt         time.Time           `json:"generatedAt"`
	ActionURL           string              `json:"actionUrl"`
	Reports             []FlexPreviewReport `json:"reports"`
	PayloadBytes        int                 `json:"payloadBytes"`
	Message             json.RawMessage     `json:"message"`
}

type FlexPreviewTenantReader interface {
	Get(context.Context, uuid.UUID) (tenant.Tenant, error)
}

type FlexPreviewService struct {
	tenants       FlexPreviewTenantReader
	publicBaseURL *url.URL
	now           func() time.Time
}

func NewFlexPreviewService(tenants FlexPreviewTenantReader, publicBaseURL *url.URL, now func() time.Time) *FlexPreviewService {
	return &FlexPreviewService{tenants: tenants, publicBaseURL: publicBaseURL, now: now}
}

func (service *FlexPreviewService) Preview(ctx context.Context, tenantID uuid.UUID, input FlexPreviewInput) (FlexPreview, error) {
	validated, err := validateFlexPreviewInput(input)
	if err != nil {
		return FlexPreview{}, err
	}
	item, err := service.tenants.Get(ctx, tenantID)
	if err != nil {
		return FlexPreview{}, err
	}
	location, err := time.LoadLocation(item.Timezone)
	if err != nil {
		return FlexPreview{}, errors.New("tenant timezone is invalid")
	}
	generatedAt := service.now().In(location)
	period, err := report.ResolvePeriod(validated.PeriodPreset, location, generatedAt, nil, nil)
	if err != nil {
		return FlexPreview{}, err
	}
	actionURL := *service.publicBaseURL
	actionURL.Path = strings.TrimRight(actionURL.Path, "/") + "/app/tenant/" + tenantID.String()
	actionURL.RawQuery = ""
	actionURL.Fragment = ""

	reports := make([]FlexPreviewReport, 0, len(validated.ReportKeys))
	renderReports := make([]FlexReport, 0, len(validated.ReportKeys))
	for _, key := range validated.ReportKeys {
		definition, _ := report.DefinitionFor(key)
		metrics := make([]FlexPreviewMetric, 0, len(definition.LineMetrics))
		renderMetrics := make(map[string]string, len(definition.LineMetrics))
		for index, metric := range definition.LineMetrics {
			value := previewMetricValue(metric.Key, index)
			metrics = append(metrics, FlexPreviewMetric{Label: metric.LabelTH, Value: value})
			renderMetrics[metric.Key] = value
		}
		reportURL := *service.publicBaseURL
		reportURL.Path = strings.TrimRight(reportURL.Path, "/") + "/app/tenant/" + tenantID.String() + "/report/" + string(key)
		reportURL.RawQuery = ""
		reportURL.Fragment = ""
		dashboard := previewDashboard(key, period)
		renderReport := FlexReport{Key: key, Metrics: renderMetrics, Dashboard: &dashboard, ActionURL: reportURL.String()}
		presentation, err := BuildFlexReportPresentation(renderReport)
		if err != nil {
			return FlexPreview{}, err
		}
		reports = append(reports, FlexPreviewReport{
			Key: key, Label: definition.LabelTH, CategoryLabel: presentation.CategoryLabel, Metrics: metrics,
			Primary: presentation.Primary, Supporting: presentation.Supporting, Comparison: presentation.Comparison,
			Attention: presentation.Attention, DataState: presentation.DataState, StateText: presentation.StateText, ActionURL: presentation.ActionURL,
		})
		renderReports = append(renderReports, renderReport)
	}
	rendered, err := RenderFlexWithStats(FlexInput{
		TenantName: item.Name, Timezone: item.Timezone, Period: period, GeneratedAt: generatedAt, ActionURL: actionURL.String(), Reports: renderReports,
	})
	if err != nil {
		return FlexPreview{}, err
	}
	return FlexPreview{
		PresentationVersion: rendered.PresentationVersion,
		AltText:             flexAltText(item.Name, period, len(reports)), TenantName: item.Name, Period: period, PeriodLabel: periodLabel(period),
		GeneratedAt: generatedAt, ActionURL: actionURL.String(), Reports: reports, PayloadBytes: rendered.PayloadBytes, Message: rendered.Message,
	}, nil
}

func previewDashboard(key report.Key, period report.Period) report.Dashboard {
	definition := flexPresentationDefinitions[key]
	metricKeys := append([]string{definition.primary}, definition.supporting...)
	metrics := make([]report.DashboardMetric, 0, len(metricKeys))
	for index, metricKey := range metricKeys {
		label, unit := previewMetricMetadata(key, metricKey)
		comparison := report.MetricComparison{Availability: report.ComparisonUnavailable}
		if report.ComparisonSupported(key, period) {
			comparison = report.MetricComparison{Availability: report.ComparisonAvailable, PreviousValue: "100000.00", Delta: "-7800.00", Percent: "-7.82", Direction: report.DirectionDown}
		}
		metrics = append(metrics, report.DashboardMetric{Key: metricKey, Label: label, Value: previewDashboardValue(unit, index), Unit: unit, Comparison: comparison})
	}
	if key == report.ARDebtReceipt {
		metrics = append(metrics, report.DashboardMetric{Key: "payment_split_missing_count", Label: "เอกสารแยกวิธีชำระไม่ครบ", Value: "0", Unit: report.UnitCount})
	}
	return report.Dashboard{ReportKey: key, Version: "1.0.0", Period: period, Timezone: "Asia/Bangkok", KPIs: metrics, Visualizations: []report.DashboardVisualization{}, Quality: report.DashboardQuality{Status: "OK", Warnings: []string{}}}
}

func previewMetricMetadata(reportKey report.Key, key string) (string, report.MetricUnit) {
	metadata := map[string]struct {
		label string
		unit  report.MetricUnit
	}{
		"total_amount":          {"ยอดรวม", report.UnitTHB},
		"document_count":        {"จำนวนเอกสาร", report.UnitCount},
		"average_per_document":  {"ยอดเฉลี่ยต่อเอกสาร", report.UnitTHB},
		"gross_profit_amount":   {"กำไรขั้นต้น", report.UnitTHB},
		"gross_margin_percent":  {"อัตรากำไรขั้นต้น", report.UnitPercent},
		"net_amount":            {"ยอดขายสุทธิ", report.UnitTHB},
		"balance_amount":        {"มูลค่าสต็อกคงเหลือ", report.UnitTHB},
		"item_count":            {"จำนวนสินค้า", report.UnitCount},
		"reorder_item_count":    {"สินค้าที่ต้องสั่ง", report.UnitCount},
		"shortage_qty":          {"จำนวนขาดรวม", report.UnitQuantity},
		"net_movement_amount":   {"ยอดเคลื่อนไหวสุทธิ", report.UnitTHB},
		"customer_count":        {"จำนวนลูกหนี้", report.UnitCount},
		"total_received_amount": {"ยอดรับชำระ", report.UnitTHB},
		"receipt_count":         {"จำนวนเอกสาร", report.UnitCount},
		"average_per_receipt":   {"ยอดเฉลี่ยต่อเอกสาร", report.UnitTHB},
	}
	if item, ok := metadata[key]; ok {
		if key == "total_amount" {
			switch reportKey {
			case report.SalesGoodsServices:
				item.label = "ยอดขาย"
			case report.PurchaseGoodsPayables:
				item.label = "ยอดซื้อ"
			case report.CashBankReceipts:
				item.label = "ยอดรับเงิน"
			case report.CashBankPayments:
				item.label = "ยอดจ่ายเงิน"
			}
		}
		return item.label, item.unit
	}
	return key, unitForMetricKey(key)
}

func previewDashboardValue(unit report.MetricUnit, index int) string {
	switch unit {
	case report.UnitCount:
		return "128"
	case report.UnitPercent:
		return "24.75"
	case report.UnitQuantity:
		return "1250.1234"
	default:
		if index == 0 {
			return "125000.00"
		}
		return "1234567.89"
	}
}

func validateFlexPreviewInput(input FlexPreviewInput) (FlexPreviewInput, error) {
	switch input.PeriodPreset {
	case report.Yesterday, report.TodayToNow, report.MonthToDate, report.AsOfRun:
	default:
		return FlexPreviewInput{}, &FlexPreviewValidationError{Field: "periodPreset", Code: "INVALID_PERIOD_PRESET"}
	}
	if len(input.ReportKeys) < 1 || len(input.ReportKeys) > 10 {
		return FlexPreviewInput{}, &FlexPreviewValidationError{Field: "reportKeys", Code: "INVALID_REPORTS"}
	}
	seen := make(map[report.Key]struct{}, len(input.ReportKeys))
	for _, key := range input.ReportKeys {
		definition, ok := report.DefinitionFor(key)
		if !ok || !report.CanSelect(definition, false) {
			return FlexPreviewInput{}, &FlexPreviewValidationError{Field: "reportKeys", Code: "INVALID_REPORTS"}
		}
		if _, duplicate := seen[key]; duplicate {
			return FlexPreviewInput{}, &FlexPreviewValidationError{Field: "reportKeys", Code: "DUPLICATE_REPORT"}
		}
		seen[key] = struct{}{}
	}
	input.ReportKeys = append([]report.Key(nil), input.ReportKeys...)
	return input, nil
}

func previewMetricValue(key string, index int) string {
	if strings.Contains(key, "percent") {
		return "24.75%"
	}
	if strings.Contains(key, "count") {
		return "128"
	}
	if strings.Contains(key, "qty") {
		return "1,250.0000"
	}
	if index == 0 {
		return "125,000.00"
	}
	return "1,234,567.89"
}
