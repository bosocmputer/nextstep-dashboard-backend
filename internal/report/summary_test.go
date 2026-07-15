package report

import (
	"strings"
	"testing"
)

func TestSummarizeProducesStableLineMetricsForAllReports(t *testing.T) {
	tests := []struct {
		key     Key
		steps   map[string][]map[string]string
		metrics map[string]string
	}{
		{SalesGoodsServices, map[string][]map[string]string{"headers": {{"doc_no": "S1", "total_amount": "0.10"}, {"doc_no": "S2", "total_amount": "0.20"}}, "details": {{"sum_amount": "0.30"}}}, map[string]string{"document_count": "2", "total_amount": "0.30"}},
		{PurchaseGoodsPayables, map[string][]map[string]string{"headers": {{"doc_no": "P1", "total_amount": "100.00"}}, "details": {}}, map[string]string{"document_count": "1", "total_amount": "100.00"}},
		{GrossProfitByProduct, map[string][]map[string]string{"rows": {{"amount_sale": "100", "cost_sale": "60", "amount_sale_return": "10", "cost_sale_return": "5"}}}, map[string]string{"gross_profit_amount": "35.00", "gross_margin_percent": "38.89"}},
		{GrossProfitByARCustomer, map[string][]map[string]string{"rows": {{"amount_sale": "200", "cost_sale": "150", "amount_sale_return": "0", "cost_sale_return": "0"}}}, map[string]string{"gross_profit_amount": "50.00", "gross_margin_percent": "25.00"}},
		{StockBalance, map[string][]map[string]string{"rows": {{"ic_code": "A", "balance_amount": "12.34"}, {"ic_code": "B", "balance_amount": "7.66"}}}, map[string]string{"item_count": "2", "balance_amount": "20.00"}},
		{StockReorder, map[string][]map[string]string{"rows": {{"ic_code": "A", "balance_qty": "2", "purchase_point": "5"}, {"ic_code": "B", "balance_qty": "-1", "purchase_point": "2"}}}, map[string]string{"reorder_item_count": "2", "shortage_qty": "6.0000"}},
		{ARCustomerMovement, map[string][]map[string]string{"rows": {{"cust_code": "C1", "doc_sort": "1", "amount": "100"}, {"cust_code": "C1", "doc_sort": "2", "amount": "10"}, {"cust_code": "C2", "doc_sort": "3", "amount": "20"}}}, map[string]string{"customer_count": "2", "net_movement_amount": "70.00"}},
		{ARDebtReceipt, map[string][]map[string]string{"rows": {{"doc_no": "R1", "total_net_value": "40"}, {"doc_no": "R2", "total_net_value": "60"}}}, map[string]string{"receipt_count": "2", "total_received_amount": "100.00"}},
		{CashBankReceipts, map[string][]map[string]string{"rows": {{"doc_no": "CB1", "total_amount": "25.50"}}}, map[string]string{"document_count": "1", "total_amount": "25.50"}},
		{CashBankPayments, map[string][]map[string]string{"rows": {{"doc_no": "CB2", "total_amount": "9.75"}}}, map[string]string{"document_count": "1", "total_amount": "9.75"}},
	}
	for _, test := range tests {
		t.Run(string(test.key), func(t *testing.T) {
			result, err := Summarize(test.key, test.steps)
			if err != nil {
				t.Fatalf("Summarize() error = %v", err)
			}
			for key, expected := range test.metrics {
				if got := result.Metrics[key]; got != expected {
					t.Errorf("metric %s = %q, want %q; all=%v", key, got, expected, result.Metrics)
				}
			}
			if result.RowCount == 0 || len(result.Rows) == 0 {
				t.Fatalf("result rows were not retained: %+v", result)
			}
		})
	}
}

func TestSummarizeRejectsMalformedMoneyInsteadOfSilentlyReturningZero(t *testing.T) {
	_, err := Summarize(CashBankReceipts, map[string][]map[string]string{"rows": {{"total_amount": "not-a-number"}}})
	if err == nil {
		t.Fatal("malformed monetary value was accepted")
	}
}

func TestSummarizeUsesFullSetMetricsFromBoundedSummaryRows(t *testing.T) {
	steps := map[string][]map[string]string{"rows": {{
		"ic_code": "TOP-1", "ic_name": "สินค้าอันดับหนึ่ง", "balance_amount": "100.00", "amount_in": "10", "amount_out": "5",
		"_metric_item_count": "500", "_metric_balance_amount": "10000.00",
		"_metric_amount_in": "1200.00", "_metric_amount_out": "800.00", "_metric_row_count": "500",
	}}}
	result, err := Summarize(StockBalance, steps)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 500 || result.Metrics["item_count"] != "500" || result.Metrics["balance_amount"] != "10000.00" {
		t.Fatalf("bounded summary lost full-set metrics: %+v", result)
	}
	dashboard, err := BuildDashboard(StockBalance,
		Period{Preset: AsOfRun, DateFrom: "2026-07-14", DateTo: "2026-07-14"},
		Period{Preset: Custom, DateFrom: "2026-07-13", DateTo: "2026-07-13"}, steps, map[string][]map[string]string{"rows": {}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if dashboard.KPIs[0].Value != "10000.00" || dashboard.KPIs[2].Value != "1200.00" {
		t.Fatalf("dashboard KPIs were calculated from top rows instead of full-set metrics: %+v", dashboard.KPIs)
	}
}

func TestDecimalAcceptsPostgresScientificNotationExactly(t *testing.T) {
	tests := map[string]string{
		"0E-14":     "0",
		"0E-15":     "0",
		"0E-16":     "0",
		"0E-17":     "0",
		"0E-20":     "0",
		"0E-22":     "0",
		"-5.00E-13": "-1/2000000000000",
		"1E+3":      "1000",
	}
	for value, expected := range tests {
		t.Run(value, func(t *testing.T) {
			parsed, err := decimal(value)
			if err != nil {
				t.Fatalf("decimal(%q) error = %v", value, err)
			}
			if got := parsed.RatString(); got != expected {
				t.Fatalf("decimal(%q) = %s, want %s", value, got, expected)
			}
		})
	}
}

func TestDecimalRejectsUnsafeOrMalformedValues(t *testing.T) {
	for _, value := range []string{
		"NaN", "Infinity", "-Infinity", "1/2", " 1", "1 ", "1E", "1E10001", strings.Repeat("1", 257),
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := decimal(value); err == nil {
				t.Fatalf("decimal(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestStockReportsAcceptScientificNotationReturnedByDATA1(t *testing.T) {
	period := Period{Preset: AsOfRun, DateFrom: "2026-07-14", DateTo: "2026-07-14"}
	comparison := Period{Preset: Custom, DateFrom: "2026-07-13", DateTo: "2026-07-13"}

	stockRows := map[string][]map[string]string{"rows": {
		{
			"ic_code": "A", "ic_name": "สินค้า A", "balance_amount": "12745001.62760995667801600", "balance_qty": "0E-20",
			"qty_in": "0E-14", "amount_in": "0", "qty_out": "0E-15", "amount_out": "0",
		},
		{
			"ic_code": "B", "ic_name": "สินค้า B", "balance_amount": "-5.00E-13", "balance_qty": "0E-20",
			"qty_in": "0", "amount_in": "0", "qty_out": "0", "amount_out": "0",
		},
	}}
	stockSummary, err := Summarize(StockBalance, stockRows)
	if err != nil {
		t.Fatalf("Summarize(stock_balance) error = %v", err)
	}
	if got := stockSummary.Metrics["balance_amount"]; got != "12745001.63" {
		t.Fatalf("stock balance amount = %q", got)
	}
	if _, err := BuildDashboard(StockBalance, period, comparison, stockRows, map[string][]map[string]string{"rows": {}}); err != nil {
		t.Fatalf("BuildDashboard(stock_balance) error = %v", err)
	}

	reorderRows := map[string][]map[string]string{"rows": {{
		"ic_code": "A", "ic_name": "สินค้า A", "balance_qty": "0E-22", "purchase_point": "5.0000", "purchase_balance_qty": "0E-14",
	}}}
	reorderSummary, err := Summarize(StockReorder, reorderRows)
	if err != nil {
		t.Fatalf("Summarize(stock_reorder) error = %v", err)
	}
	if got := reorderSummary.Metrics["shortage_qty"]; got != "5.0000" {
		t.Fatalf("stock reorder shortage = %q", got)
	}
	if _, err := BuildDashboard(StockReorder, period, comparison, reorderRows, map[string][]map[string]string{"rows": {}}); err != nil {
		t.Fatalf("BuildDashboard(stock_reorder) error = %v", err)
	}
}
