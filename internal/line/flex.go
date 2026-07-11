package line

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

const maximumFlexPayloadBytes = 30 * 1024

var ErrFlexInputInvalid = errors.New("LINE Flex input is invalid")

type FlexReport struct {
	Key     report.Key
	Metrics map[string]string
}

type FlexInput struct {
	TenantName  string
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
	actionURL, err := url.Parse(input.ActionURL)
	if err != nil || actionURL.Host == "" || actionURL.Scheme != "https" || input.Period.DateFrom == "" || input.Period.DateTo == "" {
		return nil, ErrFlexInputInvalid
	}
	bodyContents := []any{
		map[string]any{"type": "text", "text": input.TenantName, "weight": "bold", "size": "xl", "color": "#0F172A", "wrap": true},
		map[string]any{"type": "text", "text": periodLabel(input.Period), "size": "sm", "color": "#64748B", "margin": "sm", "wrap": true},
	}
	seen := make(map[report.Key]struct{}, len(input.Reports))
	for _, item := range input.Reports {
		definition, ok := report.DefinitionFor(item.Key)
		if !ok {
			return nil, ErrFlexInputInvalid
		}
		if _, duplicate := seen[item.Key]; duplicate {
			return nil, ErrFlexInputInvalid
		}
		seen[item.Key] = struct{}{}
		metricRows := make([]any, 0, len(definition.LineMetrics))
		for _, metric := range definition.LineMetrics {
			value, exists := item.Metrics[metric.Key]
			value = strings.TrimSpace(value)
			if !exists || value == "" || utf8.RuneCountInString(value) > 64 {
				return nil, ErrFlexInputInvalid
			}
			metricRows = append(metricRows, map[string]any{
				"type": "box", "layout": "horizontal", "margin": "sm",
				"contents": []any{
					map[string]any{"type": "text", "text": metric.LabelTH, "size": "sm", "color": "#475569", "flex": 5, "wrap": true},
					map[string]any{"type": "text", "text": value, "size": "sm", "color": "#0F172A", "weight": "bold", "align": "end", "flex": 4, "wrap": true},
				},
			})
		}
		bodyContents = append(bodyContents,
			map[string]any{"type": "separator", "margin": "lg", "color": "#E2E8F0"},
			map[string]any{
				"type": "box", "layout": "vertical", "margin": "lg",
				"contents": append([]any{map[string]any{"type": "text", "text": definition.LabelTH, "weight": "bold", "size": "md", "color": "#0F766E", "wrap": true}}, metricRows...),
			},
		)
	}
	bodyContents = append(bodyContents, map[string]any{
		"type": "text", "text": "สร้างเมื่อ " + input.GeneratedAt.Format("02/01/2006 15:04 MST"),
		"size": "xs", "color": "#94A3B8", "margin": "lg", "wrap": true,
	})
	message := map[string]any{
		"type":    "flex",
		"altText": flexAltText(input.TenantName, input.Period),
		"contents": map[string]any{
			"type": "bubble", "size": "kilo",
			"header": map[string]any{
				"type": "box", "layout": "vertical", "backgroundColor": "#0F766E", "paddingAll": "16px",
				"contents": []any{map[string]any{"type": "text", "text": "NEXTSTEP DASHBOARD", "color": "#FFFFFF", "weight": "bold", "size": "sm"}},
			},
			"body": map[string]any{"type": "box", "layout": "vertical", "paddingAll": "18px", "contents": bodyContents},
			"footer": map[string]any{
				"type": "box", "layout": "vertical", "paddingAll": "16px",
				"contents": []any{map[string]any{
					"type": "button", "style": "primary", "color": "#0F766E", "height": "sm",
					"action": map[string]any{"type": "uri", "label": "เปิดรายงาน", "uri": input.ActionURL},
				}},
			},
		},
	}
	payload, err := json.Marshal(message)
	if err != nil || len(payload) > maximumFlexPayloadBytes {
		return nil, ErrFlexInputInvalid
	}
	return payload, nil
}

func periodLabel(period report.Period) string {
	if period.DateFrom == period.DateTo {
		return "ข้อมูลวันที่ " + period.DateTo
	}
	return "ข้อมูล " + period.DateFrom + " ถึง " + period.DateTo
}

func flexAltText(tenantName string, period report.Period) string {
	text := "รายงาน " + tenantName + " — " + periodLabel(period)
	if utf8.RuneCountInString(text) <= 400 {
		return text
	}
	runes := []rune(text)
	return string(runes[:397]) + "..."
}
