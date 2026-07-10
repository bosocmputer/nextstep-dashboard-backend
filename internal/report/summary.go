package report

import (
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
)

var decimalPattern = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)

type SummaryResult struct {
	Metrics        map[string]string
	Rows           []map[string]string
	RowCount       int
	Reconciliation map[string]any
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
	rows := steps["rows"]
	var err error
	switch key {
	case SalesGoodsServices, PurchaseGoodsPayables:
		headers := steps["headers"]
		result.Metrics["document_count"] = strconv.Itoa(len(headers))
		total, sumErr := sumField(headers, "total_amount")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Metrics["total_amount"] = money(total)
		detailTotal, sumErr := sumField(steps["details"], "sum_amount")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Reconciliation["headerTotal"] = money(total)
		result.Reconciliation["detailTotal"] = money(detailTotal)
		result.Reconciliation["difference"] = money(new(big.Rat).Sub(total, detailTotal))
	case GrossProfitByProduct, GrossProfitByARCustomer:
		amountSale, e1 := sumField(rows, "amount_sale")
		costSale, e2 := sumField(rows, "cost_sale")
		amountReturn, e3 := sumField(rows, "amount_sale_return")
		costReturn, e4 := sumField(rows, "cost_sale_return")
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
		result.Metrics["item_count"] = strconv.Itoa(len(rows))
		balance, sumErr := sumField(rows, "balance_amount")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Metrics["balance_amount"] = money(balance)
	case StockReorder:
		result.Metrics["reorder_item_count"] = strconv.Itoa(len(rows))
		shortage := new(big.Rat)
		for _, row := range rows {
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
		result.Metrics["receipt_count"] = strconv.Itoa(len(rows))
		total, sumErr := sumField(rows, "total_net_value")
		if sumErr != nil {
			return SummaryResult{}, sumErr
		}
		result.Metrics["total_received_amount"] = money(total)
	case CashBankReceipts, CashBankPayments:
		result.Metrics["document_count"] = strconv.Itoa(len(rows))
		total, sumErr := sumField(rows, "total_amount")
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
		rowCount += len(steps[name])
	}
	flattened := make([]map[string]string, 0, rowCount)
	for _, name := range orderedNames {
		for _, source := range steps[name] {
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
	if !decimalPattern.MatchString(value) {
		return nil, errors.New("value is not a decimal")
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
