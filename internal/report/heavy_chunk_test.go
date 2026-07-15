package report

import (
	"strings"
	"testing"
)

func TestChunkPlansConstrainApprovedHeavyUnitKeys(t *testing.T) {
	period := Period{Preset: AsOfRun, DateFrom: "2026-07-14", DateTo: "2026-07-14"}
	stock, err := BuildChunkQueryPlan(StockBalance, period, ResultSummary, []string{"001", "002"})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderSQL(stock.Steps[0].Query)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "i.code = any(array['001', '002']::text[])") || !strings.Contains(rendered, "_metric_item_count") || !strings.Contains(rendered, "limit 20") {
		t.Fatalf("stock chunk query is not bounded: %s", rendered)
	}
	ar, err := BuildChunkQueryPlan(ARCustomerMovement, period, ResultDetail, []string{"00001"})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err = RenderSQL(ar.Steps[0].Query)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(rendered, "t.cust_code = any(array['00001']::text[])"); got != 4 {
		t.Fatalf("AR chunk filters = %d, want 4", got)
	}
}

func TestMergeStockSummaryChunksPreservesCompleteMetricsAndGlobalTopRows(t *testing.T) {
	chunk := func(code, balance, total string) map[string][]map[string]string {
		return map[string][]map[string]string{"rows": {{
			"ic_code": code, "balance_amount": balance, "amount_in": "1", "amount_out": "2",
			"_metric_item_count": "1", "_metric_balance_amount": total,
			"_metric_amount_in": "1", "_metric_amount_out": "2", "_metric_row_count": "1",
		}}}
	}
	merged, err := MergeChunkedSteps(StockBalance, ResultSummary, []map[string][]map[string]string{
		chunk("A", "10", "10"), chunk("B", "100", "100"),
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Summarize(StockBalance, merged)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Metrics["item_count"] != "2" || summary.Metrics["balance_amount"] != "110.00" || summary.RowCount != 2 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestChunkKeysAreDeterministicAndKeepLeadingZeros(t *testing.T) {
	keys, err := ChunkKeys([]map[string]string{{"unit_key": "00123"}, {"unit_key": "A"}, {"unit_key": "00123"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0] != "00123" || ChunkKey(0, keys) != ChunkKey(0, append([]string(nil), keys...)) {
		t.Fatalf("keys=%v chunk=%s", keys, ChunkKey(0, keys))
	}
}

func TestEmptyHeavyManifestProducesZeroSummaryWithoutFallbackQuery(t *testing.T) {
	steps, err := MergeChunkedSteps(StockBalance, ResultSummary, nil)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Summarize(StockBalance, steps)
	if err != nil {
		t.Fatal(err)
	}
	if summary.RowCount != 0 || summary.Metrics["item_count"] != "0" || summary.Metrics["balance_amount"] != "0.00" {
		t.Fatalf("summary=%+v", summary)
	}
}
