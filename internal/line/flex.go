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
	FlexPresentationVersion = "executive-navy-v1"
)

const (
	flexHeaderColor   = "#0B2347"
	flexActionColor   = "#175CD3"
	flexTitleColor    = "#123B6D"
	flexTextColor     = "#0F172A"
	flexMutedColor    = "#5B6B82"
	flexSurfaceColor  = "#F5F8FC"
	flexBorderColor   = "#E3EBF5"
	flexSubtitleColor = "#D6E4FF"
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

type FlexRenderResult struct {
	Message             json.RawMessage
	PresentationVersion string
	PayloadBytes        int
	ReportCount         int
	ZeroReportCount     int
	Duration            time.Duration
}

func RenderFlex(input FlexInput) (json.RawMessage, error) {
	result, err := RenderFlexWithStats(input)
	return result.Message, err
}

func RenderFlexWithStats(input FlexInput) (result FlexRenderResult, err error) {
	startedAt := time.Now()
	defer func() { result.Duration = time.Since(startedAt) }()
	input.TenantName = strings.TrimSpace(input.TenantName)
	if input.TenantName == "" || utf8.RuneCountInString(input.TenantName) > 160 || len(input.Reports) < 1 || len(input.Reports) > 10 || input.GeneratedAt.IsZero() {
		return result, ErrFlexInputInvalid
	}
	overviewURL, err := validHTTPSURL(input.ActionURL)
	periodFrom, fromErr := time.Parse(time.DateOnly, input.Period.DateFrom)
	periodTo, toErr := time.Parse(time.DateOnly, input.Period.DateTo)
	if err != nil || fromErr != nil || toErr != nil || periodTo.Before(periodFrom) {
		return result, ErrFlexInputInvalid
	}
	timezone := strings.TrimSpace(input.Timezone)
	if timezone == "" {
		timezone = "Asia/Bangkok"
	}
	if timezone != "Asia/Bangkok" {
		return result, ErrFlexInputInvalid
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return result, ErrFlexInputInvalid
	}

	presentations := make([]FlexReportPresentation, 0, len(input.Reports))
	seen := make(map[report.Key]struct{}, len(input.Reports))
	for _, item := range input.Reports {
		if _, duplicate := seen[item.Key]; duplicate {
			return result, ErrFlexInputInvalid
		}
		seen[item.Key] = struct{}{}
		if strings.TrimSpace(item.ActionURL) == "" {
			item.ActionURL = overviewURL.String()
		}
		reportURL, parseErr := validHTTPSURL(item.ActionURL)
		if parseErr != nil || !strings.EqualFold(reportURL.Host, overviewURL.Host) {
			return result, ErrFlexInputInvalid
		}
		presentation, presentationErr := BuildFlexReportPresentation(item)
		if presentationErr != nil {
			return result, presentationErr
		}
		presentations = append(presentations, presentation)
	}

	zeroReportCount := 0
	bodyContents := []any{
		map[string]any{"type": "text", "text": input.TenantName, "size": "xl", "color": flexTextColor, "weight": "bold", "wrap": true, "maxLines": 2, "adjustMode": "shrink-to-fit", "scaling": true},
		flexText(periodLabel(input.Period), "sm", flexMutedColor, false, true, "sm"),
		flexText(runeCountLabel(len(presentations)), "xs", "#94A3B8", false, true, "xs"),
	}
	if input.Period.Preset == report.TodayToNow {
		bodyContents = append(bodyContents, flexText("วันนี้ยังไม่มีช่วงเวลาเปรียบเทียบที่เท่ากัน", "xs", flexMutedColor, false, true, "sm"))
	}
	lastCategory := ""
	for _, item := range presentations {
		if item.CategoryLabel != lastCategory {
			bodyContents = append(bodyContents, flexText(item.CategoryLabel, "xs", flexMutedColor, true, true, "lg"))
			lastCategory = item.CategoryLabel
		}
		if item.DataState == FlexDataZero {
			zeroReportCount++
		}
		bodyContents = append(bodyContents, reportBox(item))
	}
	localGeneratedAt := input.GeneratedAt.In(location)
	bodyContents = append(bodyContents, flexText("สร้างเมื่อ "+thaiShortDate(localGeneratedAt)+" · "+localGeneratedAt.Format("15:04")+" น. เวลาไทย", "xs", "#94A3B8", false, true, "lg"))

	message := map[string]any{
		"type":    "flex",
		"altText": flexAltText(input.TenantName, input.Period, len(presentations)),
		"contents": map[string]any{
			"type": "bubble", "size": "giga",
			"header": map[string]any{
				"type": "box", "layout": "vertical", "backgroundColor": flexHeaderColor, "paddingAll": "16px",
				"contents": []any{
					flexText("NEXTSTEP DASHBOARD", "sm", "#FFFFFF", true, false),
					flexText("สรุปผู้บริหาร", "xs", flexSubtitleColor, false, false, "xs"),
				},
			},
			"body": map[string]any{"type": "box", "layout": "vertical", "paddingAll": "18px", "contents": bodyContents},
			"footer": map[string]any{
				"type": "box", "layout": "vertical", "paddingAll": "16px",
				"contents": []any{map[string]any{
					"type": "button", "style": "primary", "color": flexActionColor, "height": "sm", "scaling": true,
					"action": map[string]any{"type": "uri", "label": "ดูภาพรวมร้าน", "uri": overviewURL.String()},
				}},
			},
		},
	}
	payload, err := json.Marshal(message)
	if err != nil || len(payload) >= maximumFlexPayloadBytes {
		return result, ErrFlexInputInvalid
	}
	result.Message = payload
	result.PresentationVersion = FlexPresentationVersion
	result.PayloadBytes = len(payload)
	result.ReportCount = len(presentations)
	result.ZeroReportCount = zeroReportCount
	return result, nil
}

func reportBox(item FlexReportPresentation) map[string]any {
	contents := []any{
		map[string]any{
			"type": "box", "layout": "horizontal", "alignItems": "center",
			"contents": []any{
				map[string]any{"type": "text", "text": item.Label, "weight": "bold", "size": "sm", "color": flexTitleColor, "wrap": true, "maxLines": 2, "adjustMode": "shrink-to-fit", "scaling": true, "flex": 9},
				map[string]any{"type": "text", "text": "›", "size": "lg", "color": "#94A3B8", "align": "end", "flex": 1},
			},
		},
	}
	if item.DataState == FlexDataZero {
		contents = append(contents, flexText(item.StateText, "xs", flexMutedColor, false, true, "sm"))
	} else {
		contents = append(contents, metricRow(item.Primary, true))
	}
	if item.Comparison != nil {
		contents = append(contents, flexText(item.Comparison.Text, "xs", "#64748B", false, true, "xs"))
	}
	if item.DataState != FlexDataZero {
		for _, metric := range item.Supporting {
			contents = append(contents, metricRow(metric, false))
		}
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
		"type": "box", "layout": "vertical", "margin": "sm", "paddingAll": "12px", "cornerRadius": "8px", "backgroundColor": flexSurfaceColor,
		"borderWidth": "1px", "borderColor": flexBorderColor,
		"action":   map[string]any{"type": "uri", "label": "เปิดรายละเอียดรายงาน", "uri": item.ActionURL},
		"contents": contents,
	}
}

func metricRow(metric FlexMetricPresentation, primary bool) map[string]any {
	labelSize, valueSize, labelColor, valueColor := "xs", "sm", flexMutedColor, flexTextColor
	weight := "regular"
	labelFlex, valueFlex := 5, 5
	if primary {
		valueSize, weight, labelFlex, valueFlex = "md", "bold", 4, 6
	}
	return map[string]any{
		"type": "box", "layout": "baseline", "margin": "sm",
		"contents": []any{
			map[string]any{"type": "text", "text": metric.Label, "size": labelSize, "color": labelColor, "flex": labelFlex, "wrap": true, "scaling": true},
			map[string]any{"type": "text", "text": metric.Value, "size": valueSize, "color": valueColor, "weight": weight, "align": "end", "flex": valueFlex, "wrap": false, "scaling": true, "adjustMode": "shrink-to-fit"},
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
		return "ข้อมูล ณ " + thaiShortDate(to)
	}
	if from.Year() == to.Year() && from.Month() == to.Month() {
		return "ข้อมูล " + strconv.Itoa(from.Day()) + "–" + thaiShortDate(to)
	}
	if from.Year() == to.Year() {
		return "ข้อมูล " + thaiDayMonth(from) + "–" + thaiShortDate(to)
	}
	return "ข้อมูล " + thaiShortDate(from) + "–" + thaiShortDate(to)
}

var thaiShortMonths = [...]string{"ม.ค.", "ก.พ.", "มี.ค.", "เม.ย.", "พ.ค.", "มิ.ย.", "ก.ค.", "ส.ค.", "ก.ย.", "ต.ค.", "พ.ย.", "ธ.ค."}

func thaiDayMonth(value time.Time) string {
	return strconv.Itoa(value.Day()) + " " + thaiShortMonths[value.Month()-1]
}

func thaiShortDate(value time.Time) string {
	return thaiDayMonth(value) + " " + strconv.Itoa(value.Year()+543)
}

func runeCountLabel(count int) string { return strconv.Itoa(count) + " รายงาน" }

func flexAltText(tenantName string, period report.Period, reportCount int) string {
	text := "สรุปรายงาน " + tenantName + ": " + periodLabel(period) + " (" + runeCountLabel(reportCount) + ")"
	if utf8.RuneCountInString(text) <= 400 {
		return text
	}
	runes := []rune(text)
	return string(runes[:397]) + "..."
}
