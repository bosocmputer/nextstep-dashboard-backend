package report

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

const (
	maximumDecimalTextLength = 256
	maximumDecimalExponent   = 10_000
)

var decimalPattern = regexp.MustCompile(`^-?([0-9]+)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

type SummaryResult struct {
	Metrics        map[string]string
	Rows           []map[string]string
	RowCount       int
	Reconciliation map[string]any
	Dashboard      *Dashboard
}

func Summarize(key Key, steps map[string][]map[string]string) (SummaryResult, error) {
	if _, ok := DefinitionFor(key); !ok {
		return SummaryResult{}, errors.New("report key is not approved")
	}
	result := SummaryResult{
		Metrics:        make(map[string]string),
		Rows:           flattenRows(key, steps),
		Reconciliation: map[string]any{"status": "OK"},
	}
	result.RowCount = len(result.Rows)
	if value, ok := summaryMetric(steps, "row_count"); ok {
		if count, parseErr := strconv.Atoi(integerText(value)); parseErr == nil {
			result.RowCount = count
		}
	}
	rows := steps["rows"]
	var err error
	switch key {
	case SalesGoodsServices, PurchaseGoodsPayables:
		headers := steps["headers"]
		result.Metrics["document_count"] = summaryMetricOr(steps, "document_count", strconv.Itoa(len(realSummaryRows(headers))))
		total, sumErr := sumField(realSummaryRows(headers), "total_amount")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Metrics["total_amount"] = moneyMetricOr(steps, "total_amount", total)
		detailTotal, sumErr := sumField(realSummaryRows(steps["details"]), "sum_amount")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		metricTotal, parseErr := decimal(result.Metrics["total_amount"])
		if parseErr != nil {
			return SummaryResult{}, fieldDecimalError("_metric_total_amount", parseErr)
		}
		if value, ok := summaryMetric(steps, "detail_total"); ok {
			detailTotal, parseErr = decimal(value)
			if parseErr != nil {
				return SummaryResult{}, fieldDecimalError("_metric_detail_total", parseErr)
			}
		}
		result.Reconciliation["headerTotal"] = money(metricTotal)
		result.Reconciliation["detailTotal"] = money(detailTotal)
		result.Reconciliation["difference"] = money(new(big.Rat).Sub(metricTotal, detailTotal))
	case GrossProfitByProduct, GrossProfitByARCustomer:
		amountSale, e1 := summaryDecimalOr(steps, "amount_sale", rows, "amount_sale")
		costSale, e2 := summaryDecimalOr(steps, "cost_sale", rows, "cost_sale")
		amountReturn, e3 := summaryDecimalOr(steps, "amount_sale_return", rows, "amount_sale_return")
		costReturn, e4 := summaryDecimalOr(steps, "cost_sale_return", rows, "cost_sale_return")
		if err = firstError(e1, e2, e3, e4); err != nil {
			return SummaryResult{}, err
		}
		netAmount := new(big.Rat).Sub(amountSale, amountReturn)
		netCost := new(big.Rat).Sub(costSale, costReturn)
		grossProfit := new(big.Rat).Sub(netAmount, netCost)
		result.Metrics["gross_profit_amount"] = money(grossProfit)
		if netAmount.Sign() == 0 {
			result.Metrics["gross_margin_percent"] = "0.00"
		} else {
			margin := new(big.Rat).Mul(grossProfit, big.NewRat(100, 1))
			margin.Quo(margin, netAmount)
			result.Metrics["gross_margin_percent"] = margin.FloatString(2)
		}
		result.Reconciliation["netAmount"] = money(netAmount)
		result.Reconciliation["netCost"] = money(netCost)
	case StockBalance:
		result.Metrics["item_count"] = summaryMetricOr(steps, "item_count", strconv.Itoa(len(realSummaryRows(rows))))
		balance, sumErr := summaryDecimalOr(steps, "balance_amount", rows, "balance_amount")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Metrics["balance_amount"] = money(balance)
		result.Metrics["amount_in"] = decimalMetricOrZero(steps, "amount_in")
		result.Metrics["amount_out"] = decimalMetricOrZero(steps, "amount_out")
	case StockReorder:
		result.Metrics["reorder_item_count"] = summaryMetricOr(steps, "reorder_item_count", strconv.Itoa(len(realSummaryRows(rows))))
		if value, ok := summaryMetric(steps, "shortage_qty"); ok {
			parsed, parseErr := decimal(value)
			if parseErr != nil {
				return SummaryResult{}, fieldDecimalError("_metric_shortage_qty", parseErr)
			}
			result.Metrics["shortage_qty"] = parsed.FloatString(4)
			break
		}
		shortage := new(big.Rat)
		for _, row := range realSummaryRows(rows) {
			purchasePoint, parseErr := decimal(row["purchase_point"])
			if parseErr != nil {
				return SummaryResult{}, fieldDecimalError("purchase_point", parseErr)
			}
			balance, parseErr := decimal(row["balance_qty"])
			if parseErr != nil {
				return SummaryResult{}, fieldDecimalError("balance_qty", parseErr)
			}
			difference := new(big.Rat).Sub(purchasePoint, balance)
			if difference.Sign() > 0 {
				shortage.Add(shortage, difference)
			}
		}
		result.Metrics["shortage_qty"] = shortage.FloatString(4)
	case ARCustomerMovement:
		if customerCount, ok := summaryMetric(steps, "customer_count"); ok {
			result.Metrics["customer_count"] = integerText(customerCount)
			result.Metrics["net_movement_amount"] = moneyMetricOr(steps, "net_movement_amount", new(big.Rat))
			result.Metrics["debit_amount"] = moneyMetricOr(steps, "debit_amount", new(big.Rat))
			result.Metrics["credit_amount"] = moneyMetricOr(steps, "credit_amount", new(big.Rat))
			break
		}
		customers := make(map[string]struct{})
		netMovement := new(big.Rat)
		for _, row := range rows {
			customers[row["cust_code"]] = struct{}{}
			amount, parseErr := decimal(row["amount"])
			if parseErr != nil {
				return SummaryResult{}, fieldDecimalError("amount", parseErr)
			}
			if row["doc_sort"] == "2" || row["doc_sort"] == "3" {
				netMovement.Sub(netMovement, amount)
			} else {
				netMovement.Add(netMovement, amount)
			}
		}
		result.Metrics["customer_count"] = strconv.Itoa(len(customers))
		result.Metrics["net_movement_amount"] = money(netMovement)
	case ARDebtReceipt:
		result.Metrics["receipt_count"] = summaryMetricOr(steps, "receipt_count", strconv.Itoa(len(realSummaryRows(rows))))
		total, sumErr := summaryDecimalOr(steps, "total_amount", rows, "total_net_value")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Metrics["total_received_amount"] = money(total)
		result.Metrics["payment_split_missing_count"] = summaryMetricOr(steps, "payment_split_missing_count", strconv.Itoa(countTrue(realSummaryRows(rows), "payment_split_missing")))
	case CashBankReceipts, CashBankPayments:
		result.Metrics["document_count"] = summaryMetricOr(steps, "document_count", strconv.Itoa(len(realSummaryRows(rows))))
		total, sumErr := summaryDecimalOr(steps, "total_amount", rows, "total_amount")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Metrics["total_amount"] = money(total)
	}
	result.Reconciliation["rowCount"] = result.RowCount
	return result, nil
}

func flattenRows(key Key, steps map[string][]map[string]string) []map[string]string {
	orderedNames := []string{"rows"}
	if key == SalesGoodsServices || key == PurchaseGoodsPayables {
		orderedNames = []string{"headers", "details"}
	}
	rowCount := 0
	for _, name := range orderedNames {
		rowCount += len(realSummaryRows(steps[name]))
	}
	flattened := make([]map[string]string, 0, rowCount)
	for _, name := range orderedNames {
		for _, source := range realSummaryRows(steps[name]) {
			row := make(map[string]string, len(source)+1)
			for key, value := range source {
				row[key] = value
			}
			if len(orderedNames) > 1 {
				row["_section"] = name
			}
			flattened = append(flattened, row)
		}
	}
	return flattened
}

func realSummaryRows(rows []map[string]string) []map[string]string {
	filtered := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		if row["_summary_metric_row"] == "true" || row["_summary_metric_row"] == "1" {
			continue
		}
		filtered = append(filtered, row)
	}
	return filtered
}

func summaryMetric(steps map[string][]map[string]string, key string) (string, bool) {
	field := "_metric_" + key
	for _, rows := range steps {
		for _, row := range rows {
			if value, exists := row[field]; exists && value != "" {
				return value, true
			}
		}
	}
	return "", false
}

func summaryMetricOr(steps map[string][]map[string]string, key, fallback string) string {
	if value, ok := summaryMetric(steps, key); ok {
		return value
	}
	return fallback
}

func summaryDecimalOr(steps map[string][]map[string]string, metric string, rows []map[string]string, field string) (*big.Rat, error) {
	if value, ok := summaryMetric(steps, metric); ok {
		parsed, err := decimal(value)
		if err != nil {
			return nil, fieldDecimalError("_metric_"+metric, err)
		}
		return parsed, nil
	}
	return sumField(realSummaryRows(rows), field)
}

func moneyMetricOr(steps map[string][]map[string]string, key string, fallback *big.Rat) string {
	if value, ok := summaryMetric(steps, key); ok {
		if parsed, err := decimal(value); err == nil {
			return money(parsed)
		}
	}
	return money(fallback)
}

func decimalMetricOrZero(steps map[string][]map[string]string, key string) string {
	return moneyMetricOr(steps, key, new(big.Rat))
}

func integerText(value string) string {
	parsed, err := decimal(value)
	if err != nil {
		return value
	}
	return parsed.Num().Quo(parsed.Num(), parsed.Denom()).String()
}

func sumField(rows []map[string]string, field string) (*big.Rat, error) {
	total := new(big.Rat)
	for _, row := range rows {
		value, err := decimal(row[field])
		if err != nil {
			return nil, fieldDecimalError(field, err)
		}
		total.Add(total, value)
	}
	return total, nil
}

func decimal(value string) (*big.Rat, error) {
	if value == "" {
		return new(big.Rat), nil
	}
	if len(value) > maximumDecimalTextLength {
		return nil, errors.New("decimal value exceeds the length limit")
	}
	matches := decimalPattern.FindStringSubmatch(value)
	if matches == nil {
		return nil, errors.New("value is not a decimal")
	}
	coefficient := matches[1] + matches[2]
	if strings.Trim(coefficient, "0") == "" {
		return new(big.Rat), nil
	}
	if matches[3] != "" {
		exponent, err := strconv.ParseInt(matches[3], 10, 32)
		if err != nil || exponent < -maximumDecimalExponent || exponent > maximumDecimalExponent {
			return nil, errors.New("decimal exponent exceeds the limit")
		}
	}
	parsed, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, errors.New("value could not be parsed as a decimal")
	}
	return parsed, nil
}

func fieldDecimalError(field string, err error) error {
	return fmt.Errorf("report field %s is invalid: %w", field, err)
}

func money(value *big.Rat) string { return value.FloatString(2) }

func firstError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
