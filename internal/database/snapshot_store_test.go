package database

import (
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

func TestSnapshotExecutionKeyUsesResolvedPeriodModeAndVersions(t *testing.T) {
	tenantID := uuid.New()
	monthToDate := report.Period{Preset: report.MonthToDate, DateFrom: "2026-07-01", DateTo: "2026-07-12"}
	custom := report.Period{Preset: report.Custom, DateFrom: "2026-07-01", DateTo: "2026-07-12"}

	fingerprint := report.QueryPlanFingerprint(report.SalesGoodsServices, report.ResultSummary)
	base := snapshotExecutionKey(tenantID, report.SalesGoodsServices, monthToDate, report.DateRange, "1.0.0", 6, fingerprint, report.ResultSummary)
	if equivalent := snapshotExecutionKey(tenantID, report.SalesGoodsServices, custom, report.DateRange, "1.0.0", 6, fingerprint, report.ResultSummary); equivalent != base {
		t.Fatalf("equivalent resolved periods must share a key: %s != %s", equivalent, base)
	}
	for name, changed := range map[string]string{
		"period mode":         snapshotExecutionKey(tenantID, report.SalesGoodsServices, custom, report.AsOfDate, "1.0.0", 6, fingerprint, report.ResultSummary),
		"definition version":  snapshotExecutionKey(tenantID, report.SalesGoodsServices, custom, report.DateRange, "2.0.0", 6, fingerprint, report.ResultSummary),
		"data source version": snapshotExecutionKey(tenantID, report.SalesGoodsServices, custom, report.DateRange, "1.0.0", 7, fingerprint, report.ResultSummary),
		"query plan":          snapshotExecutionKey(tenantID, report.SalesGoodsServices, custom, report.DateRange, "1.0.0", 6, strings.Repeat("f", 64), report.ResultSummary),
		"projection":          snapshotExecutionKey(tenantID, report.SalesGoodsServices, custom, report.DateRange, "1.0.0", 6, fingerprint, report.ResultDetail),
	} {
		if changed == base {
			t.Fatalf("%s must invalidate the cache key", name)
		}
	}
}
