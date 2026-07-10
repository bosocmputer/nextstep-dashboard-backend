package main

import (
	"strings"
	"testing"
)

func TestSmokeSQLWrapsApprovedQueryWithOneRowLimit(t *testing.T) {
	rendered := smokeSQL("select id from ic_trans order by id")
	if !strings.HasPrefix(rendered, "select * from (\nselect id") || !strings.HasSuffix(rendered, ") as nextstep_smoke limit 1") {
		t.Fatalf("smokeSQL() = %q", rendered)
	}
}

func TestSmokePeriodRequiresBoundedDates(t *testing.T) {
	if _, err := smokePeriod("2026-01-01", "2026-03-01"); err == nil {
		t.Fatal("expected an excessive smoke range to fail")
	}
	period, err := smokePeriod("2026-07-01", "2026-07-10")
	if err != nil || period.DateFrom != "2026-07-01" || period.DateTo != "2026-07-10" {
		t.Fatalf("smokePeriod() = %+v, %v", period, err)
	}
}
