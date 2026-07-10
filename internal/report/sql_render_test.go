package report

import (
	"strings"
	"testing"
)

func TestRenderSQLQuotesInjectionLikeValuesAsData(t *testing.T) {
	query := Query{SQL: "select * from docs where doc_date between $1::date and $2::date and code = $3", Args: []any{
		"2026-07-01",
		"2026-07-10",
		"x'; drop table tenants; --",
	}}

	rendered, err := RenderSQL(query)
	if err != nil {
		t.Fatalf("RenderSQL() error = %v", err)
	}
	if !strings.Contains(rendered, `'x''; drop table tenants; --'`) {
		t.Fatalf("value was not safely quoted: %s", rendered)
	}
	if strings.Count(rendered, "drop table") != 1 {
		t.Fatalf("unexpected rendered SQL: %s", rendered)
	}
}

func TestRenderSQLRejectsMissingInvalidAndUnsupportedParameters(t *testing.T) {
	for _, query := range []Query{
		{SQL: "select $2", Args: []any{"only-one"}},
		{SQL: "select $0", Args: []any{"invalid-index"}},
		{SQL: "select $1", Args: []any{map[string]string{"unsupported": "value"}}},
	} {
		if _, err := RenderSQL(query); err == nil {
			t.Fatalf("query %+v was accepted", query)
		}
	}
}

func TestRenderSQLDoesNotConfusePlaceholderPrefixes(t *testing.T) {
	rendered, err := RenderSQL(Query{SQL: "select $1, $10", Args: []any{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}})
	if err != nil {
		t.Fatalf("RenderSQL() error = %v", err)
	}
	if rendered != "select 1, 10" {
		t.Fatalf("rendered = %q", rendered)
	}
}
