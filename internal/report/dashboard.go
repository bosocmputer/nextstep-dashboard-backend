package report

import "time"

type MetricUnit string

const (
	UnitTHB      MetricUnit = "THB"
	UnitCount    MetricUnit = "COUNT"
	UnitPercent  MetricUnit = "PERCENT"
	UnitQuantity MetricUnit = "QUANTITY"
	UnitRatio    MetricUnit = "RATIO"
)

type ComparisonAvailability string

const (
	ComparisonAvailable   ComparisonAvailability = "AVAILABLE"
	ComparisonUnavailable ComparisonAvailability = "UNAVAILABLE"
)

type ComparisonDirection string

const (
	DirectionUp   ComparisonDirection = "UP"
	DirectionDown ComparisonDirection = "DOWN"
	DirectionSame ComparisonDirection = "SAME"
)

type MetricComparison struct {
	Availability  ComparisonAvailability `json:"availability"`
	PreviousValue string                 `json:"previousValue,omitempty"`
	Delta         string                 `json:"delta,omitempty"`
	Percent       string                 `json:"percent,omitempty"`
	Direction     ComparisonDirection    `json:"direction,omitempty"`
}

type DashboardMetric struct {
	Key        string           `json:"key"`
	Label      string           `json:"label"`
	Value      string           `json:"value"`
	Unit       MetricUnit       `json:"unit"`
	Comparison MetricComparison `json:"comparison"`
}

type VisualizationIntent string

const (
	IntentTrend       VisualizationIntent = "TREND"
	IntentRanking     VisualizationIntent = "RANKING"
	IntentComposition VisualizationIntent = "COMPOSITION"
	IntentException   VisualizationIntent = "EXCEPTION"
)

type VisualizationSeries struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Values      []string `json:"values"`
	PointLabels []string `json:"pointLabels,omitempty"`
}

type DashboardVisualization struct {
	Key        string                `json:"key"`
	Title      string                `json:"title"`
	Intent     VisualizationIntent   `json:"intent"`
	Unit       MetricUnit            `json:"unit"`
	Categories []string              `json:"categories"`
	Series     []VisualizationSeries `json:"series"`
	Note       string                `json:"note,omitempty"`
}

type DashboardQuality struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings"`
}

type Dashboard struct {
	ReportKey        Key                      `json:"reportKey"`
	Version          string                   `json:"version"`
	Period           Period                   `json:"period"`
	ComparisonPeriod Period                   `json:"comparisonPeriod"`
	Timezone         string                   `json:"timezone"`
	GeneratedAt      time.Time                `json:"generatedAt"`
	KPIs             []DashboardMetric        `json:"kpis"`
	Visualizations   []DashboardVisualization `json:"visualizations"`
	Quality          DashboardQuality         `json:"quality"`
}
