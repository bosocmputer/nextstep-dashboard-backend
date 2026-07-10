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
	Key     report.Key          `json:"key"`
	Label   string              `json:"label"`
	Metrics []FlexPreviewMetric `json:"metrics"`
}

type FlexPreview struct {
	AltText      string              `json:"altText"`
	TenantName   string              `json:"tenantName"`
	Period       report.Period       `json:"period"`
	PeriodLabel  string              `json:"periodLabel"`
	GeneratedAt  time.Time           `json:"generatedAt"`
	ActionURL    string              `json:"actionUrl"`
	Reports      []FlexPreviewReport `json:"reports"`
	PayloadBytes int                 `json:"payloadBytes"`
	Message      json.RawMessage     `json:"message"`
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
	actionURL.Path = strings.TrimRight(actionURL.Path, "/") + "/app"
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
		reports = append(reports, FlexPreviewReport{Key: key, Label: definition.LabelTH, Metrics: metrics})
		renderReports = append(renderReports, FlexReport{Key: key, Metrics: renderMetrics})
	}
	message, err := RenderFlex(FlexInput{
		TenantName: item.Name, Period: period, GeneratedAt: generatedAt, ActionURL: actionURL.String(), Reports: renderReports,
	})
	if err != nil {
		return FlexPreview{}, err
	}
	return FlexPreview{
		AltText: flexAltText(item.Name, period), TenantName: item.Name, Period: period, PeriodLabel: periodLabel(period),
		GeneratedAt: generatedAt, ActionURL: actionURL.String(), Reports: reports, PayloadBytes: len(message), Message: message,
	}, nil
}

func validateFlexPreviewInput(input FlexPreviewInput) (FlexPreviewInput, error) {
	switch input.PeriodPreset {
	case report.Yesterday, report.TodayToNow, report.MonthToDate, report.AsOfRun:
	default:
		return FlexPreviewInput{}, &FlexPreviewValidationError{Field: "periodPreset", Code: "INVALID_PERIOD_PRESET"}
	}
	if len(input.ReportKeys) < 1 || len(input.ReportKeys) > 5 {
		return FlexPreviewInput{}, &FlexPreviewValidationError{Field: "reportKeys", Code: "INVALID_REPORTS"}
	}
	seen := make(map[report.Key]struct{}, len(input.ReportKeys))
	for _, key := range input.ReportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
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
