package viewer

import (
	"regexp"
	"strings"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
)

var reportDecimalPattern = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$`)
var reportDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type rowColumnKind string

const (
	rowColumnText       rowColumnKind = "TEXT"
	rowColumnIdentifier rowColumnKind = "IDENTIFIER"
	rowColumnDate       rowColumnKind = "DATE"
	rowColumnNumber     rowColumnKind = "NUMBER"
)

var reportRowFilterColumns = map[report.Key]map[string]rowColumnKind{
	report.SalesGoodsServices: rowColumns(
		[]string{"cust_name", "item_name", "unit_code", "discount"},
		[]string{"doc_no", "cust_code", "item_code", "branch_code", "wh_code", "shelf_code"},
		[]string{"doc_date"}, []string{"qty", "sum_amount", "total_amount", "price"}),
	report.PurchaseGoodsPayables: rowColumns(
		[]string{"cust_name", "item_name", "unit_code", "discount"},
		[]string{"doc_no", "cust_code", "item_code", "branch_code", "wh_code", "shelf_code"},
		[]string{"doc_date"}, []string{"qty", "sum_amount", "total_amount", "price"}),
	report.GrossProfitByProduct:    rowColumns([]string{"name_1", "unit_name"}, []string{"code"}, nil, []string{"qty_sale", "amount_sale", "cost_sale", "qty_sale_return", "amount_sale_return", "cost_sale_return"}),
	report.GrossProfitByARCustomer: rowColumns([]string{"ar_detail"}, []string{"ar_code"}, nil, []string{"qty_sale", "amount_sale", "cost_sale", "qty_sale_return", "amount_sale_return", "cost_sale_return"}),
	report.StockBalance:            rowColumns([]string{"ic_name", "ic_unit_code"}, []string{"ic_code"}, nil, []string{"balance_qty", "balance_amount", "average_cost_end", "average_cost", "qty_in", "amount_in", "qty_out", "amount_out", "average_cost_in", "average_cost_out"}),
	report.StockReorder:            rowColumns([]string{"ic_name", "ic_unit_code"}, []string{"ic_code"}, nil, []string{"balance_qty", "purchase_point", "purchase_balance_qty"}),
	report.ARCustomerMovement:      rowColumns([]string{"cust_name"}, []string{"doc_no", "cust_code", "tax_doc_no", "doc_ref"}, []string{"doc_date"}, []string{"amount", "credit_day"}),
	report.ARDebtReceipt:           rowColumns([]string{"cust_name"}, []string{"doc_no", "cust_code"}, []string{"doc_date", "billing_date"}, []string{"total_net_value", "cash_amount", "transfer_amount"}),
	report.CashBankReceipts:        rowColumns([]string{"ap_ar_name", "trans_flag_label"}, []string{"doc_no", "ap_ar_code", "trans_flag_code"}, []string{"doc_date"}, []string{"total_amount", "cash_amount", "transfer_amount", "card_amount", "chq_amount", "coupon_amount"}),
	report.CashBankPayments:        rowColumns([]string{"ap_ar_name", "trans_flag_label"}, []string{"doc_no", "ap_ar_code", "trans_flag_code"}, []string{"doc_date"}, []string{"total_amount", "cash_amount", "transfer_amount", "card_amount", "chq_amount", "petty_cash_amount"}),
}

func rowColumns(text, identifiers, dates, numbers []string) map[string]rowColumnKind {
	result := make(map[string]rowColumnKind, len(text)+len(identifiers)+len(dates)+len(numbers))
	for _, key := range text {
		result[key] = rowColumnText
	}
	for _, key := range identifiers {
		result[key] = rowColumnIdentifier
	}
	for _, key := range dates {
		result[key] = rowColumnDate
	}
	for _, key := range numbers {
		result[key] = rowColumnNumber
	}
	return result
}

func validateReportRowFilters(reportKey report.Key, filters []report.RowFilter) error {
	columns, ok := reportRowFilterColumns[reportKey]
	if !ok {
		return ErrReportInputInvalid
	}
	seen := make(map[string]struct{}, len(filters))
	for index := range filters {
		filter := &filters[index]
		filter.ColumnKey = strings.TrimSpace(filter.ColumnKey)
		filter.Value = strings.TrimSpace(filter.Value)
		kind, exists := columns[filter.ColumnKey]
		if !exists || filter.Value == "" || len(filter.Value) > 160 {
			return ErrReportInputInvalid
		}
		if _, duplicate := seen[filter.ColumnKey]; duplicate {
			return ErrReportInputInvalid
		}
		seen[filter.ColumnKey] = struct{}{}
		filter.ValueType = string(kind)
		switch kind {
		case rowColumnText:
			if filter.Operator != report.RowFilterContains && filter.Operator != report.RowFilterEquals {
				return ErrReportInputInvalid
			}
		case rowColumnIdentifier:
			if filter.Operator != report.RowFilterContains && filter.Operator != report.RowFilterEquals {
				return ErrReportInputInvalid
			}
		case rowColumnDate:
			if filter.Operator != report.RowFilterEquals && filter.Operator != report.RowFilterGTE && filter.Operator != report.RowFilterLTE {
				return ErrReportInputInvalid
			}
			if !reportDatePattern.MatchString(filter.Value) {
				return ErrReportInputInvalid
			}
		case rowColumnNumber:
			if filter.Operator != report.RowFilterEquals && filter.Operator != report.RowFilterGTE && filter.Operator != report.RowFilterLTE {
				return ErrReportInputInvalid
			}
			if !reportDecimalPattern.MatchString(filter.Value) {
				return ErrReportInputInvalid
			}
		}
	}
	return nil
}
