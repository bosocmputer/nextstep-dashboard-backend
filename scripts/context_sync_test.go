package main

import (
	"strings"
	"testing"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

func TestRenderReportCatalogUsesDefinitions(t *testing.T) {
	definitions := report.Definitions()
	rendered := renderReportCatalog(definitions)
	if got := strings.Count(rendered, "\n| `"); got != len(definitions) {
		t.Fatalf("rendered rows = %d, want %d", got, len(definitions))
	}
	for _, want := range []string{"`stock_balance`", "AS_OF_DATE", "HEAVY", "1.0.0"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered catalog missing %q", want)
		}
	}
}

func TestReplaceGeneratedBlockChangesOnlyMarkerContents(t *testing.T) {
	original := "before\n<!-- BEGIN GENERATED: REPORT_CATALOG -->\nold\n<!-- END GENERATED: REPORT_CATALOG -->\nafter\n"
	updated, err := replaceGeneratedBlock(original, "new")
	if err != nil {
		t.Fatal(err)
	}
	want := "before\n<!-- BEGIN GENERATED: REPORT_CATALOG -->\nnew\n<!-- END GENERATED: REPORT_CATALOG -->\nafter\n"
	if updated != want {
		t.Fatalf("updated block = %q, want %q", updated, want)
	}
	if _, err := replaceGeneratedBlock("no markers", "new"); err == nil {
		t.Fatal("expected missing marker error")
	}
}
