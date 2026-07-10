package report

import "testing"

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
