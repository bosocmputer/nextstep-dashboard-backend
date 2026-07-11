package line

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

const (
	softFlexPayloadBytes    = 24 * 1024
	maximumFlexPayloadBytes = 30 * 1024
)

var ErrFlexInputInvalid = errors.New("LINE Flex input is invalid")

type FlexReport struct {
	Key       report.Key
	Metrics   map[string]string
	Dashboard *report.Dashboard
	ActionURL string
}

type FlexInput struct {
	TenantName  string
	Timezone    string
	Period      report.Period
	GeneratedAt time.Time
	ActionURL   string
	Reports     []FlexReport
}

func RenderFlex(input FlexInput) (json.RawMessage, error) {
	input.TenantName = strings.TrimSpace(input.TenantName)
	if input.TenantName == "" || utf8.RuneCountInString(input.TenantName) > 160 || len(input.Reports) < 1 || len(input.Reports) > 10 || input.GeneratedAt.IsZero() {
		return nil, ErrFlexInputInvalid
	}
	overviewURL, err := validHTTPSURL(input.ActionURL)
	periodFrom, fromErr := time.Parse(time.DateOnly, input.Period.DateFrom)
	periodTo, toErr := time.Parse(time.DateOnly, input.Period.DateTo)
	if err != nil || fromErr != nil || toErr != nil || periodTo.Before(periodFrom) {
		return nil, ErrFlexInputInvalid
	}
	timezone := strings.TrimSpace(input.Timezone)
	if timezone == "" {
		timezone = "Asia/Bangkok"
	}
	if timezone != "Asia/Bangkok" {
		return nil, ErrFlexInputInvalid
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, ErrFlexInputInvalid
	}

	presentations := make([]FlexReportPresentation, 0, len(input.Reports))
	seen := make(map[report.Key]struct{}, len(input.Reports))
	for _, item := range input.Reports {
		if _, duplicate := seen[item.Key]; duplicate {
			return nil, ErrFlexInputInvalid
		}
		seen[item.Key] = struct{}{}
		if strings.TrimSpace(item.ActionURL) == "" {
			item.ActionURL = overviewURL.String()
		}
		reportURL, parseErr := validHTTPSURL(item.ActionURL)
		if parseErr != nil || !strings.EqualFold(reportURL.Host, overviewURL.Host) {
			return nil, ErrFlexInputInvalid
		}
		presentation, presentationErr := BuildFlexReportPresentation(item)
		if presentationErr != nil {
			return nil, presentationErr
		}
		presentations = append(presentations, presentation)
	}

	bodyContents := []any{
		flexText(input.TenantName, "xl", "#0F172A", true, true),
		flexText(periodLabel(input.Period), "sm", "#64748B", false, true, "sm"),
		flexText(runeCountLabel(len(presentations)), "xs", "#94A3B8", false, true, "xs"),
	}
	lastCategory := ""
	for _, item := range presentations {
		if item.CategoryLabel != lastCategory {
			bodyContents = append(bodyContents, flexText(item.CategoryLabel, "xs", "#64748B", true, true, "lg"))
			lastCategory = item.CategoryLabel
		}
		bodyContents = append(bodyContents, reportBox(item))
	}
	localGeneratedAt := input.GeneratedAt.In(location)
	bodyContents = append(bodyContents, flexText("สร้างเมื่อ "+localGeneratedAt.Format("02/01/2006 15:04")+" เวลาไทย", "xs", "#94A3B8", false, true, "lg"))

	message := map[string]any{
		"type":    "flex",
		"altText": flexAltText(input.TenantName, input.Period, len(presentations)),
		"contents": map[string]any{
			"type": "bubble", "size": "giga",
			"header": map[string]any{
				"type": "box", "layout": "vertical", "backgroundColor": "#0F766E", "paddingAll": "16px",
				"contents": []any{
					flexText("NEXTSTEP DASHBOARD", "sm", "#FFFFFF", true, false),
					flexText("สรุปผู้บริหาร", "xs", "#CCFBF1", false, false, "xs"),
				},
			},
			"body": map[string]any{"type": "box", "layout": "vertical", "paddingAll": "18px", "contents": bodyContents},
			"footer": map[string]any{
				"type": "box", "layout": "vertical", "paddingAll": "16px",
				"contents": []any{map[string]any{
					"type": "button", "style": "primary", "color": "#0F766E", "height": "sm", "scaling": true,
					"action": map[string]any{"type": "uri", "label": "เปิดภาพรวมร้าน", "uri": overviewURL.String()},
				}},
			},
		},
	}
	payload, err := json.Marshal(message)
	if err != nil || len(payload) >= maximumFlexPayloadBytes {
		return nil, ErrFlexInputInvalid
	}
	return payload, nil
}

func reportBox(item FlexReportPresentation) map[string]any {
	contents := []any{
		map[string]any{
			"type": "box", "layout": "horizontal", "alignItems": "center",
			"contents": []any{
				map[string]any{"type": "text", "text": item.Label, "weight": "bold", "size": "sm", "color": "#0F766E", "wrap": true, "scaling": true, "flex": 9},
				map[string]any{"type": "text", "text": "›", "size": "lg", "color": "#94A3B8", "align": "end", "flex": 1},
			},
		},
		metricRow(item.Primary, true),
	}
	if item.Comparison != nil {
		contents = append(contents, flexText(item.Comparison.Text, "xs", "#64748B", false, true, "xs"))
	}
	for _, metric := range item.Supporting {
		contents = append(contents, metricRow(metric, false))
	}
	if item.Attention != nil {
		background, color := "#E2E8F0", "#475569"
		if item.Attention.Severity == FlexAttentionWarning {
			background, color = "#FEF3C7", "#92400E"
		} else if item.Attention.Severity == FlexAttentionDanger {
			background, color = "#FEE2E2", "#B91C1C"
		}
		contents = append(contents, map[string]any{
			"type": "box", "layout": "vertical", "margin": "sm", "paddingAll": "8px", "cornerRadius": "6px", "backgroundColor": background,
			"contents": []any{flexText(item.Attention.Text, "xs", color, true, true)},
		})
	}
	return map[string]any{
		"type": "box", "layout": "vertical", "margin": "sm", "paddingAll": "12px", "cornerRadius": "8px", "backgroundColor": "#F8FAFC",
		"action":   map[string]any{"type": "uri", "label": "เปิดรายละเอียดรายงาน", "uri": item.ActionURL},
		"contents": contents,
	}
}

func metricRow(metric FlexMetricPresentation, primary bool) map[string]any {
	size, labelColor, valueColor := "xs", "#64748B", "#0F172A"
	weight := "regular"
	if primary {
		size, labelColor, weight = "sm", "#475569", "bold"
	}
	return map[string]any{
		"type": "box", "layout": "baseline", "margin": "sm",
		"contents": []any{
			map[string]any{"type": "text", "text": metric.Label, "size": size, "color": labelColor, "flex": 5, "wrap": true, "scaling": true},
			map[string]any{"type": "text", "text": metric.Value, "size": size, "color": valueColor, "weight": weight, "align": "end", "flex": 5, "wrap": true, "scaling": true, "adjustMode": "shrink-to-fit"},
		},
	}
}

func flexText(text, size, color string, bold, wrap bool, margin ...string) map[string]any {
	component := map[string]any{"type": "text", "text": text, "size": size, "color": color, "wrap": wrap, "scaling": true}
	if bold {
		component["weight"] = "bold"
	}
	if len(margin) > 0 && margin[0] != "" {
		component["margin"] = margin[0]
	}
	return component
}

func validHTTPSURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, ErrFlexInputInvalid
	}
	return parsed, nil
}

func periodLabel(period report.Period) string {
	from, fromErr := time.Parse(time.DateOnly, period.DateFrom)
	to, toErr := time.Parse(time.DateOnly, period.DateTo)
	if fromErr != nil || toErr != nil || to.Before(from) {
		return "ข้อมูล " + period.DateFrom + " ถึง " + period.DateTo
	}
	if from.Equal(to) {
		return "ข้อมูลวันที่ " + to.Format("02/01/2006")
	}
	return "ข้อมูล " + from.Format("02/01/2006") + " ถึง " + to.Format("02/01/2006")
}

func runeCountLabel(count int) string { return strconv.Itoa(count) + " รายงาน" }

func flexAltText(tenantName string, period report.Period, reportCount int) string {
	text := "สรุปรายงาน " + tenantName + " — " + periodLabel(period) + " (" + runeCountLabel(reportCount) + ")"
	if utf8.RuneCountInString(text) <= 400 {
		return text
	}
	runes := []rune(text)
	return string(runes[:397]) + "..."
}
