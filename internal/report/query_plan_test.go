package report

import (
	"strings"
	"testing"
)

func TestQueryPlanFingerprintIsStableAndProjectionSpecific(t *testing.T) {
	for _, key := range Keys() {
		detail := QueryPlanFingerprint(key, ResultDetail)
		summary := QueryPlanFingerprint(key, ResultSummary)
		if len(detail) != 64 || len(summary) != 64 {
			t.Fatalf("%s fingerprints must be sha256 hex: detail=%q summary=%q", key, detail, summary)
		}
		if detail == summary {
			t.Fatalf("%s detail and summary fingerprints must differ", key)
		}
		if repeated := QueryPlanFingerprint(key, ResultDetail); repeated != detail {
			t.Fatalf("%s detail fingerprint is not deterministic: %q != %q", key, repeated, detail)
		}
	}
	if got := QueryPlanFingerprint(Key("unknown"), ResultSummary); got != "" {
		t.Fatalf("unknown report fingerprint = %q, want empty", got)
	}
}

func TestBuildQueryPlanCoversEveryApprovedReport(t *testing.T) {
	period := Period{Preset: TodayToNow, DateFrom: "2026-07-01", DateTo: "2026-07-10"}
	for _, key := range Keys() {
		plan, err := BuildQueryPlan(key, period)
		if err != nil {
			t.Fatalf("BuildQueryPlan(%s) error = %v", key, err)
		}
		wantSteps := 1
		if key == SalesGoodsServices || key == PurchaseGoodsPayables {
			wantSteps = 2
		}
		if len(plan.Steps) != wantSteps {
			t.Errorf("%s steps = %d, want %d", key, len(plan.Steps), wantSteps)
		}
		for _, step := range plan.Steps {
			rendered, err := RenderSQL(step.Query)
			if err != nil {
				t.Errorf("%s/%s render error = %v", key, step.Name, err)
				continue
			}
			normalized := strings.ToLower(strings.TrimSpace(rendered))
			if !strings.HasPrefix(normalized, "select") && !strings.HasPrefix(normalized, "with") {
				t.Errorf("%s/%s is not read-only SQL: %s", key, step.Name, normalized)
			}
			for _, mutation := range []string{" insert ", " update ", " delete ", " drop ", " alter ", " truncate "} {
				if strings.Contains(" "+normalized+" ", mutation) {
					t.Errorf("%s/%s contains mutation %q", key, step.Name, mutation)
				}
			}
			if strings.Contains(rendered, "$1") || strings.Contains(rendered, "$2") {
				t.Errorf("%s/%s retained placeholders", key, step.Name)
			}
		}
	}
}

func TestBuildSummaryQueryPlanUsesBoundedAggregateContracts(t *testing.T) {
	period := Period{Preset: MonthToDate, DateFrom: "2026-07-01", DateTo: "2026-07-14"}
	for _, key := range Keys() {
		plan, err := BuildQueryPlanForProjection(key, period, ResultSummary)
		if err != nil {
			t.Fatalf("BuildQueryPlanForProjection(%s, SUMMARY) error = %v", key, err)
		}
		if len(plan.Steps) < 1 || len(plan.Steps) > 2 {
			t.Fatalf("%s summary steps = %d, want 1..2", key, len(plan.Steps))
		}
		for _, step := range plan.Steps {
			if !strings.Contains(step.Query.SQL, "_metric_") {
				t.Fatalf("%s/%s does not expose bounded metric metadata", key, step.Name)
			}
			rendered, renderErr := RenderSQL(step.Query)
			if renderErr != nil {
				t.Fatalf("%s/%s summary render error = %v", key, step.Name, renderErr)
			}
			if strings.Contains(rendered, "$1") || strings.Contains(rendered, "$2") {
				t.Fatalf("%s/%s retained placeholders", key, step.Name)
			}
		}
	}
}

func TestBuildQueryPlanRejectsUnknownReportAndInvalidPeriod(t *testing.T) {
	if _, err := BuildQueryPlan(Key("unknown"), Period{DateFrom: "2026-07-01", DateTo: "2026-07-10"}); err == nil {
		t.Fatal("unknown report key was accepted")
	}
	if _, err := BuildQueryPlan(SalesGoodsServices, Period{DateFrom: "", DateTo: "2026-07-10"}); err == nil {
		t.Fatal("missing period start was accepted")
	}
}
