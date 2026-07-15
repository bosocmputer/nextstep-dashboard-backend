package database

import (
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

func TestDashboardGenerationKeyIsOrderStableAndPeriodSpecific(t *testing.T) {
	tenantID := uuid.New()
	a := []report.EnqueueInput{
		{ReportKey: report.SalesGoodsServices, Period: report.Period{Preset: report.MonthToDate, DateFrom: "2026-07-01", DateTo: "2026-07-14"}},
		{ReportKey: report.StockBalance, Period: report.Period{Preset: report.Custom, DateFrom: "2026-07-14", DateTo: "2026-07-14"}},
	}
	b := []report.EnqueueInput{a[1], a[0]}
	left, err := describeDashboardGeneration(tenantID, a, 6)
	if err != nil {
		t.Fatal(err)
	}
	right, err := describeDashboardGeneration(tenantID, b, 6)
	if err != nil {
		t.Fatal(err)
	}
	if left.GenerationKey != right.GenerationKey || left.ReportSetHash != right.ReportSetHash {
		t.Fatalf("generation descriptors differ by input order: %+v %+v", left, right)
	}
	b[1].Period.DateFrom = "2026-07-02"
	custom, err := describeDashboardGeneration(tenantID, b, 6)
	if err != nil {
		t.Fatal(err)
	}
	if custom.GenerationKey == left.GenerationKey {
		t.Fatal("custom period reused the MTD generation key")
	}
	otherSource, _ := describeDashboardGeneration(tenantID, a, 7)
	if otherSource.GenerationKey == left.GenerationKey {
		t.Fatal("SML connection version did not invalidate generation key")
	}
}
