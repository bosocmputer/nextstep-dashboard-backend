package report

import (
	"strings"
	"testing"
)

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

func TestBuildQueryPlanRejectsUnknownReportAndInvalidPeriod(t *testing.T) {
	if _, err := BuildQueryPlan(Key("unknown"), Period{DateFrom: "2026-07-01", DateTo: "2026-07-10"}); err == nil {
		t.Fatal("unknown report key was accepted")
	}
	if _, err := BuildQueryPlan(SalesGoodsServices, Period{DateFrom: "", DateTo: "2026-07-10"}); err == nil {
		t.Fatal("missing period start was accepted")
	}
}
