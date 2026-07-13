package schedule

import (
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

func TestResolveEffectivePeriodByReportMode(t *testing.T) {
	location, _ := time.LoadLocation("Asia/Bangkok")
	runAt := time.Date(2027, 1, 1, 8, 0, 0, 0, location)
	tests := []struct {
		preset report.Preset
		mode   report.ParameterKind
		want   report.Period
	}{
		{report.Yesterday, report.DateRange, report.Period{Preset: report.Yesterday, DateFrom: "2026-12-31", DateTo: "2026-12-31"}},
		{report.TodayToNow, report.DateRange, report.Period{Preset: report.TodayToNow, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.MonthToDate, report.DateRange, report.Period{Preset: report.MonthToDate, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.AsOfRun, report.DateRange, report.Period{Preset: report.TodayToNow, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.Yesterday, report.AsOfDate, report.Period{Preset: report.Yesterday, DateFrom: "2026-12-31", DateTo: "2026-12-31"}},
		{report.TodayToNow, report.AsOfDate, report.Period{Preset: report.AsOfRun, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.MonthToDate, report.AsOfDate, report.Period{Preset: report.AsOfRun, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.AsOfRun, report.AsOfDate, report.Period{Preset: report.AsOfRun, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.Yesterday, report.CurrentOnly, report.Period{Preset: report.AsOfRun, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.TodayToNow, report.CurrentOnly, report.Period{Preset: report.AsOfRun, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.MonthToDate, report.CurrentOnly, report.Period{Preset: report.AsOfRun, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
		{report.AsOfRun, report.CurrentOnly, report.Period{Preset: report.AsOfRun, DateFrom: "2027-01-01", DateTo: "2027-01-01"}},
	}
	for _, test := range tests {
		got, err := ResolveEffectivePeriod(test.preset, test.mode, location, runAt)
		if err != nil || got != test.want {
			t.Errorf("ResolveEffectivePeriod(%s, %s) = %+v, %v; want %+v", test.preset, test.mode, got, err, test.want)
		}
	}
}

func TestPeriodPolicyScopesSmartPeriodsByTenant(t *testing.T) {
	tenantID := uuid.New()
	policy := NewPeriodPolicy(true, []uuid.UUID{tenantID})
	if !policy.EnabledFor(tenantID) || policy.EnabledFor(uuid.New()) {
		t.Fatalf("period policy tenant scope is incorrect")
	}
	if !NewPeriodPolicy(true, nil).EnabledFor(uuid.New()) {
		t.Fatal("an empty allowlist must enable all tenants when the flag is on")
	}
}

func TestPeriodPolicyReportsOnlyBoundedResolutionLabels(t *testing.T) {
	location, _ := time.LoadLocation("Asia/Bangkok")
	tenantID := uuid.New()
	observed := ""
	policy := NewPeriodPolicy(true, []uuid.UUID{tenantID}, func(preset report.Preset, mode report.ParameterKind, result string) {
		observed = string(preset) + ":" + string(mode) + ":" + result
	})
	if _, err := policy.Resolve(tenantID, report.Yesterday, report.DateRange, location, time.Date(2027, 1, 1, 8, 0, 0, 0, location)); err != nil {
		t.Fatal(err)
	}
	if observed != "YESTERDAY:DATE_RANGE:SMART" {
		t.Fatalf("period resolution observation = %q", observed)
	}
}
