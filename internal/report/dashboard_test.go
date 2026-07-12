package report

import (
	"testing"
)

func TestResolveComparisonPeriodUsesBusinessEquivalentPeriods(t *testing.T) {
	tests := []struct {
		name string
		in   Period
		want Period
	}{
		{
			name: "yesterday compares the preceding day",
			in:   Period{Preset: Yesterday, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
			want: Period{Preset: Custom, DateFrom: "2026-07-08", DateTo: "2026-07-08"},
		},
		{
			name: "today compares the preceding day",
			in:   Period{Preset: TodayToNow, DateFrom: "2026-07-10", DateTo: "2026-07-10"},
			want: Period{Preset: Custom, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
		},
		{
			name: "month to date compares the same ordinal days in the prior month",
			in:   Period{Preset: MonthToDate, DateFrom: "2026-07-01", DateTo: "2026-07-10"},
			want: Period{Preset: Custom, DateFrom: "2026-06-01", DateTo: "2026-06-10"},
		},
		{
			name: "month to date clamps to the prior month end",
			in:   Period{Preset: MonthToDate, DateFrom: "2026-03-01", DateTo: "2026-03-31"},
			want: Period{Preset: Custom, DateFrom: "2026-02-01", DateTo: "2026-02-28"},
		},
		{
			name: "custom compares the immediately preceding equal range",
			in:   Period{Preset: Custom, DateFrom: "2026-07-01", DateTo: "2026-07-10"},
			want: Period{Preset: Custom, DateFrom: "2026-06-21", DateTo: "2026-06-30"},
		},
		{
			name: "as of compares the preceding day",
			in:   Period{Preset: AsOfRun, DateFrom: "2026-07-10", DateTo: "2026-07-10"},
			want: Period{Preset: Custom, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveComparisonPeriod(test.in)
			if err != nil {
				t.Fatalf("ResolveComparisonPeriod() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ResolveComparisonPeriod() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestResolveComparisonPeriodRejectsInvalidPeriod(t *testing.T) {
	for _, period := range []Period{
		{Preset: Custom, DateFrom: "", DateTo: "2026-07-10"},
		{Preset: Custom, DateFrom: "2026-07-11", DateTo: "2026-07-10"},
		{Preset: Preset("UNKNOWN"), DateFrom: "2026-07-10", DateTo: "2026-07-10"},
	} {
		if _, err := ResolveComparisonPeriod(period); err == nil {
			t.Fatalf("ResolveComparisonPeriod(%+v) expected error", period)
		}
	}
}

func TestBuildDashboardCoversEveryApprovedReport(t *testing.T) {
	period := Period{Preset: Custom, DateFrom: "2026-07-09", DateTo: "2026-07-10"}
	comparison := Period{Preset: Custom, DateFrom: "2026-07-07", DateTo: "2026-07-08"}

	for _, key := range Keys() {
		t.Run(string(key), func(t *testing.T) {
			current, previous := dashboardFixture(key)
			dashboard, err := BuildDashboard(key, period, comparison, current, previous)
			if err != nil {
				t.Fatalf("BuildDashboard() error = %v", err)
			}
			if dashboard.ReportKey != key || dashboard.Version != "1.0.0" {
				t.Fatalf("dashboard identity = %+v", dashboard)
			}
			if dashboard.Period != period || dashboard.ComparisonPeriod != comparison {
				t.Fatalf("dashboard periods = %+v / %+v", dashboard.Period, dashboard.ComparisonPeriod)
			}
			if len(dashboard.KPIs) < 2 {
				t.Fatalf("dashboard KPIs = %+v", dashboard.KPIs)
			}
			for _, metric := range dashboard.KPIs {
				if metric.Key == "" || metric.Label == "" || metric.Unit == "" || metric.Value == "" {
					t.Fatalf("invalid KPI = %+v", metric)
				}
				if key == StockReorder {
					if metric.Comparison.Availability != ComparisonUnavailable || metric.Comparison.PreviousValue != "" {
						t.Fatalf("unsupported comparison leaked for KPI = %+v", metric)
					}
				} else if metric.Comparison.Availability != ComparisonAvailable || metric.Comparison.PreviousValue == "" {
					t.Fatalf("comparison missing for KPI = %+v", metric)
				}
			}
			if len(dashboard.Visualizations) == 0 {
				t.Fatal("dashboard must contain at least one visualization")
			}
			for _, visualization := range dashboard.Visualizations {
				if visualization.Key == "" || visualization.Title == "" || visualization.Intent == "" || len(visualization.Series) == 0 {
					t.Fatalf("invalid visualization = %+v", visualization)
				}
				if len(visualization.Categories) > 92 {
					t.Fatalf("visualization has %d categories", len(visualization.Categories))
				}
				for _, series := range visualization.Series {
					if len(series.Values) != len(visualization.Categories) {
						t.Fatalf("series/category mismatch = %+v", visualization)
					}
				}
			}
		})
	}
}

func TestComparisonSupportedUsesOnlyComparablePeriods(t *testing.T) {
	tests := []struct {
		name   string
		key    Key
		preset Preset
		want   bool
	}{
		{name: "yesterday sales", key: SalesGoodsServices, preset: Yesterday, want: true},
		{name: "month to date profit", key: GrossProfitByProduct, preset: MonthToDate, want: true},
		{name: "custom cash", key: CashBankReceipts, preset: Custom, want: true},
		{name: "today partial period", key: SalesGoodsServices, preset: TodayToNow, want: false},
		{name: "as of stock balance", key: StockBalance, preset: AsOfRun, want: true},
		{name: "as of receivable movement", key: ARCustomerMovement, preset: AsOfRun, want: true},
		{name: "as of date range report", key: SalesGoodsServices, preset: AsOfRun, want: false},
		{name: "reorder has no historical date", key: StockReorder, preset: Yesterday, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ComparisonSupported(test.key, Period{Preset: test.preset}); got != test.want {
				t.Fatalf("ComparisonSupported(%s, %s) = %v, want %v", test.key, test.preset, got, test.want)
			}
		})
	}
}

func TestBuildDashboardRemovesUnsupportedComparisonFromMetricsAndCharts(t *testing.T) {
	current, previous := dashboardFixture(SalesGoodsServices)
	dashboard, err := BuildDashboard(
		SalesGoodsServices,
		Period{Preset: TodayToNow, DateFrom: "2026-07-10", DateTo: "2026-07-10"},
		Period{Preset: Custom, DateFrom: "2026-07-09", DateTo: "2026-07-09"},
		current,
		previous,
	)
	if err != nil {
		t.Fatalf("BuildDashboard() error = %v", err)
	}
	for _, metric := range dashboard.KPIs {
		if metric.Comparison.Availability != ComparisonUnavailable || metric.Comparison.PreviousValue != "" || metric.Comparison.Delta != "" || metric.Comparison.Percent != "" || metric.Comparison.Direction != "" {
			t.Fatalf("unsupported comparison leaked through KPI = %+v", metric)
		}
	}
	for _, visualization := range dashboard.Visualizations {
		for _, series := range visualization.Series {
			if series.Key == "previous" {
				t.Fatalf("unsupported comparison leaked through visualization = %+v", visualization)
			}
		}
	}
	if dashboard.Quality.Status != "OK" || len(dashboard.Quality.Warnings) != 0 {
		t.Fatalf("expected comparison policy was treated as a data-quality warning = %+v", dashboard.Quality)
	}
}

func TestSetComparisonUnavailableClearsFailedComparison(t *testing.T) {
	dashboard := Dashboard{
		KPIs:           []DashboardMetric{{Comparison: MetricComparison{Availability: ComparisonAvailable, PreviousValue: "10.00", Delta: "5.00", Percent: "50.00", Direction: DirectionUp}}},
		Visualizations: []DashboardVisualization{{Series: []VisualizationSeries{{Key: "current"}, {Key: "previous"}}}},
		Quality:        DashboardQuality{Status: "OK", Warnings: []string{}},
	}
	SetComparisonUnavailable(&dashboard, "COMPARISON_QUERY_FAILED")
	if dashboard.KPIs[0].Comparison.Availability != ComparisonUnavailable || dashboard.KPIs[0].Comparison.PreviousValue != "" || len(dashboard.Visualizations[0].Series) != 1 {
		t.Fatalf("comparison was not cleared = %+v", dashboard)
	}
	if dashboard.Quality.Status != "WARNING" || len(dashboard.Quality.Warnings) != 1 || dashboard.Quality.Warnings[0] != "COMPARISON_QUERY_FAILED" {
		t.Fatalf("quality warning missing = %+v", dashboard.Quality)
	}
}

func dashboardFixture(key Key) (map[string][]map[string]string, map[string][]map[string]string) {
	dateRows := func(amounts ...string) []map[string]string {
		rows := make([]map[string]string, 0, len(amounts))
		for index, amount := range amounts {
			date := "2026-07-09"
			if index > 0 {
				date = "2026-07-10"
			}
			rows = append(rows, map[string]string{"doc_date": date, "doc_no": "D" + amount, "total_amount": amount})
		}
		return rows
	}

	switch key {
	case SalesGoodsServices, PurchaseGoodsPayables:
		currentHeaders := dateRows("100.00", "200.00")
		previousHeaders := []map[string]string{{"doc_date": "2026-07-07", "doc_no": "P1", "total_amount": "50.00"}, {"doc_date": "2026-07-08", "doc_no": "P2", "total_amount": "100.00"}}
		if key == PurchaseGoodsPayables {
			currentHeaders[0]["cust_code"], currentHeaders[0]["cust_name"] = "S1", "ผู้จำหน่ายหนึ่ง"
			currentHeaders[1]["cust_code"], currentHeaders[1]["cust_name"] = "S2", "ผู้จำหน่ายสอง"
			previousHeaders[0]["cust_code"], previousHeaders[0]["cust_name"] = "S1", "ผู้จำหน่ายหนึ่ง"
			previousHeaders[1]["cust_code"], previousHeaders[1]["cust_name"] = "S2", "ผู้จำหน่ายสอง"
		}
		current := map[string][]map[string]string{
			"headers": currentHeaders,
			"details": {{"doc_date": "2026-07-09", "item_code": "I1", "item_name": "สินค้า 1", "sum_amount": "100.00"}, {"doc_date": "2026-07-10", "item_code": "I2", "item_name": "สินค้า 2", "sum_amount": "200.00"}},
		}
		previous := map[string][]map[string]string{
			"headers": previousHeaders,
			"details": {{"doc_date": "2026-07-07", "item_code": "I1", "item_name": "สินค้า 1", "sum_amount": "50.00"}, {"doc_date": "2026-07-08", "item_code": "I2", "item_name": "สินค้า 2", "sum_amount": "100.00"}},
		}
		return current, previous
	case GrossProfitByProduct:
		return grossProfitFixture("code", "name_1", "สินค้า"), grossProfitFixture("code", "name_1", "สินค้าเดิม")
	case GrossProfitByARCustomer:
		return grossProfitFixture("ar_code", "ar_detail", "ลูกค้า"), grossProfitFixture("ar_code", "ar_detail", "ลูกค้าเดิม")
	case StockBalance:
		return map[string][]map[string]string{"rows": {{"ic_code": "I1", "ic_name": "สินค้า 1", "balance_amount": "300.00", "amount_in": "120.00", "amount_out": "70.00"}, {"ic_code": "I2", "ic_name": "สินค้า 2", "balance_amount": "200.00", "amount_in": "80.00", "amount_out": "50.00"}}},
			map[string][]map[string]string{"rows": {{"ic_code": "I1", "ic_name": "สินค้า 1", "balance_amount": "250.00", "amount_in": "100.00", "amount_out": "60.00"}}}
	case StockReorder:
		return map[string][]map[string]string{"rows": {{"ic_code": "I1", "ic_name": "สินค้า 1", "ic_unit_code": "ชิ้น", "balance_qty": "2", "purchase_point": "10", "purchase_balance_qty": "3"}}},
			map[string][]map[string]string{"rows": {{"ic_code": "I1", "ic_name": "สินค้า 1", "ic_unit_code": "ชิ้น", "balance_qty": "4", "purchase_point": "10", "purchase_balance_qty": "2"}}}
	case ARCustomerMovement:
		return map[string][]map[string]string{"rows": {{"cust_code": "C1", "cust_name": "ลูกค้า 1", "doc_sort": "1", "amount": "300.00"}, {"cust_code": "C1", "cust_name": "ลูกค้า 1", "doc_sort": "2", "amount": "100.00"}}},
			map[string][]map[string]string{"rows": {{"cust_code": "C1", "cust_name": "ลูกค้า 1", "doc_sort": "1", "amount": "150.00"}}}
	case ARDebtReceipt:
		return map[string][]map[string]string{"rows": {{"doc_date": "2026-07-09", "doc_no": "R1", "cash_amount": "100.00", "transfer_amount": "20.00", "total_net_value": "120.00", "payment_split_missing": "false"}, {"doc_date": "2026-07-10", "doc_no": "R2", "cash_amount": "0", "transfer_amount": "80.00", "total_net_value": "80.00", "payment_split_missing": "false"}}},
			map[string][]map[string]string{"rows": {{"doc_date": "2026-07-07", "doc_no": "R0", "cash_amount": "60.00", "transfer_amount": "40.00", "total_net_value": "100.00", "payment_split_missing": "false"}}}
	case CashBankReceipts, CashBankPayments:
		return cashFixture("2026-07-09", "100.00", "50.00"), cashFixture("2026-07-07", "50.00", "25.00")
	default:
		return nil, nil
	}
}

func grossProfitFixture(codeField, nameField, name string) map[string][]map[string]string {
	return map[string][]map[string]string{"rows": {
		{codeField: "A", nameField: name + " A", "amount_sale": "500.00", "cost_sale": "300.00", "amount_sale_return": "0", "cost_sale_return": "0"},
		{codeField: "B", nameField: name + " B", "amount_sale": "100.00", "cost_sale": "150.00", "amount_sale_return": "0", "cost_sale_return": "0"},
	}}
}

func cashFixture(date, first, second string) map[string][]map[string]string {
	return map[string][]map[string]string{"rows": {
		{"doc_date": date, "doc_no": "CB1", "cash_amount": first, "card_amount": "10.00", "chq_amount": "5.00", "transfer_amount": second, "total_income_amount": "0", "coupon_amount": "0", "petty_cash_amount": "0", "total_amount": "165.00"},
	}}
}
