package report

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

const (
	DefaultStockChunkSize = 500
	DefaultARChunkSize    = 300
	MinimumChunkSize      = 50
)

// BuildChunkManifestQuery returns a deterministic unit manifest. Persisting
// these keys before executing any chunk makes a run repeatable after a worker
// restart and prevents the boundaries from changing mid-run.
func BuildChunkManifestQuery(key Key, period Period) (Query, int, error) {
	switch key {
	case StockBalance:
		return Query{SQL: `select code as unit_key from ic_inventory where coalesce(item_type, 0) not in (1, 3) order by code`}, DefaultStockChunkSize, nil
	case ARCustomerMovement:
		return Query{SQL: fmt.Sprintf(`with manifest_source as (%s) select distinct cust_code as unit_key from manifest_source order by cust_code`, trimFinalOrderBy(arCustomerMovementSQL)), Args: []any{period.DateTo}}, DefaultARChunkSize, nil
	default:
		return Query{}, 0, fmt.Errorf("report %s is not chunk-safe", key)
	}
}

// BuildChunkQueryPlan applies an approved unit boundary to the existing query
// rather than accepting SQL from callers. Summary plans remain bounded.
func BuildChunkQueryPlan(key Key, period Period, projection ResultKind, unitKeys []string) (QueryPlan, error) {
	if len(unitKeys) < 1 {
		return QueryPlan{}, fmt.Errorf("chunk has no units")
	}
	plan := QueryPlan{ReportKey: key, Period: period}
	switch key {
	case StockBalance:
		base := strings.Replace(stockBalanceSQL,
			"where coalesce(i.item_type, 0) not in (1, 3)",
			"where i.code = any($3) and coalesce(i.item_type, 0) not in (1, 3)", 1)
		if base == stockBalanceSQL {
			return QueryPlan{}, fmt.Errorf("stock chunk boundary could not be applied")
		}
		if projection == ResultSummary {
			base = summaryStockSQL(base)
		}
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: base, Args: []any{period.DateFrom, period.DateTo, unitKeys}}}}
	case ARCustomerMovement:
		needle := "where t.last_status = 0 and t.doc_date <= $1::date"
		base := strings.ReplaceAll(arCustomerMovementSQL, needle, needle+" and t.cust_code = any($2)")
		if base == arCustomerMovementSQL {
			return QueryPlan{}, fmt.Errorf("AR chunk boundary could not be applied")
		}
		if projection == ResultSummary {
			base = summaryARMovementSQL(base)
		}
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: base, Args: []any{period.DateTo, unitKeys}}}}
	default:
		return QueryPlan{}, fmt.Errorf("report %s is not chunk-safe", key)
	}
	return plan, nil
}

// MergeChunkedSteps merges only approved heavy report shapes. Each chunk owns
// a disjoint unit key, so additive metrics can be summed without double count.
func MergeChunkedSteps(key Key, projection ResultKind, chunks []map[string][]map[string]string) (map[string][]map[string]string, error) {
	if len(chunks) == 0 {
		if projection == ResultDetail {
			return map[string][]map[string]string{"rows": {}}, nil
		}
		switch key {
		case StockBalance:
			return map[string][]map[string]string{"rows": attachChunkMetrics(nil, map[string]string{
				"item_count": "0", "balance_amount": "0", "amount_in": "0", "amount_out": "0", "row_count": "0",
			})}, nil
		case ARCustomerMovement:
			return map[string][]map[string]string{"rows": attachChunkMetrics(nil, map[string]string{
				"customer_count": "0", "net_movement_amount": "0", "debit_amount": "0", "credit_amount": "0", "row_count": "0",
			})}, nil
		default:
			return nil, fmt.Errorf("empty chunks are not supported for %s", key)
		}
	}
	if projection == ResultDetail {
		merged := map[string][]map[string]string{"rows": {}}
		for _, chunk := range chunks {
			merged["rows"] = append(merged["rows"], chunk["rows"]...)
		}
		return merged, nil
	}
	switch key {
	case StockBalance:
		return mergeStockSummaryChunks(chunks)
	case ARCustomerMovement:
		return mergeARSummaryChunks(chunks)
	default:
		return nil, fmt.Errorf("summary chunks are not supported for %s", key)
	}
}

func mergeStockSummaryChunks(chunks []map[string][]map[string]string) (map[string][]map[string]string, error) {
	metricKeys := []string{"item_count", "balance_amount", "amount_in", "amount_out", "row_count"}
	metrics, rows, err := mergeChunkMetrics(chunks, metricKeys)
	if err != nil {
		return nil, err
	}
	type ranked struct {
		row               map[string]string
		balance, movement *big.Rat
	}
	rankedRows := make([]ranked, 0, len(rows))
	for _, row := range rows {
		balance, parseErr := decimal(row["balance_amount"])
		if parseErr != nil {
			return nil, fieldDecimalError("balance_amount", parseErr)
		}
		amountIn, parseErr := decimal(row["amount_in"])
		if parseErr != nil {
			return nil, fieldDecimalError("amount_in", parseErr)
		}
		amountOut, parseErr := decimal(row["amount_out"])
		if parseErr != nil {
			return nil, fieldDecimalError("amount_out", parseErr)
		}
		rankedRows = append(rankedRows, ranked{row: row, balance: absRat(balance), movement: new(big.Rat).Add(absRat(amountIn), absRat(amountOut))})
	}
	selected := make(map[string]map[string]string)
	sort.SliceStable(rankedRows, func(i, j int) bool {
		return compareRank(rankedRows[i].balance, rankedRows[j].balance, rankedRows[i].row["ic_code"], rankedRows[j].row["ic_code"])
	})
	for index := 0; index < len(rankedRows) && index < 10; index++ {
		selected[rankedRows[index].row["ic_code"]] = rankedRows[index].row
	}
	sort.SliceStable(rankedRows, func(i, j int) bool {
		return compareRank(rankedRows[i].movement, rankedRows[j].movement, rankedRows[i].row["ic_code"], rankedRows[j].row["ic_code"])
	})
	for index := 0; index < len(rankedRows) && index < 10; index++ {
		selected[rankedRows[index].row["ic_code"]] = rankedRows[index].row
	}
	output := make([]map[string]string, 0, len(selected))
	for _, item := range selected {
		output = append(output, item)
	}
	sort.Slice(output, func(i, j int) bool { return output[i]["ic_code"] < output[j]["ic_code"] })
	return map[string][]map[string]string{"rows": attachChunkMetrics(output, metrics)}, nil
}

func mergeARSummaryChunks(chunks []map[string][]map[string]string) (map[string][]map[string]string, error) {
	metricKeys := []string{"customer_count", "net_movement_amount", "debit_amount", "credit_amount", "row_count"}
	metrics, rows, err := mergeChunkMetrics(chunks, metricKeys)
	if err != nil {
		return nil, err
	}
	type customer struct {
		code, name    string
		debit, credit *big.Rat
	}
	customers := make(map[string]*customer)
	for _, row := range rows {
		item := customers[row["cust_code"]]
		if item == nil {
			item = &customer{code: row["cust_code"], name: row["cust_name"], debit: new(big.Rat), credit: new(big.Rat)}
			customers[item.code] = item
		}
		amount, parseErr := decimal(row["amount"])
		if parseErr != nil {
			return nil, fieldDecimalError("amount", parseErr)
		}
		if row["doc_sort"] == "2" || row["doc_sort"] == "3" {
			item.credit.Add(item.credit, amount)
		} else {
			item.debit.Add(item.debit, amount)
		}
	}
	items := make([]*customer, 0, len(customers))
	for _, item := range customers {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		left := absRat(new(big.Rat).Sub(items[i].debit, items[i].credit))
		right := absRat(new(big.Rat).Sub(items[j].debit, items[j].credit))
		return compareRank(left, right, items[i].code, items[j].code)
	})
	if len(items) > 10 {
		items = items[:10]
	}
	output := make([]map[string]string, 0, len(items)*2)
	for _, item := range items {
		output = append(output,
			map[string]string{"cust_code": item.code, "cust_name": item.name, "doc_sort": "1", "amount": item.debit.FloatString(4)},
			map[string]string{"cust_code": item.code, "cust_name": item.name, "doc_sort": "2", "amount": item.credit.FloatString(4)})
	}
	return map[string][]map[string]string{"rows": attachChunkMetrics(output, metrics)}, nil
}

func mergeChunkMetrics(chunks []map[string][]map[string]string, keys []string) (map[string]string, []map[string]string, error) {
	totals := make(map[string]*big.Rat, len(keys))
	for _, key := range keys {
		totals[key] = new(big.Rat)
	}
	rows := make([]map[string]string, 0)
	for _, chunk := range chunks {
		for _, key := range keys {
			value, ok := summaryMetric(chunk, key)
			if !ok {
				return nil, nil, fmt.Errorf("chunk metric %s is missing", key)
			}
			parsed, err := decimal(value)
			if err != nil {
				return nil, nil, fieldDecimalError("_metric_"+key, err)
			}
			totals[key].Add(totals[key], parsed)
		}
		for _, row := range realSummaryRows(chunk["rows"]) {
			copyRow := make(map[string]string, len(row))
			for key, value := range row {
				if !strings.HasPrefix(key, "_metric_") && key != "_summary_metric_row" && key != "balance_rank" && key != "movement_rank" && key != "gain_rank" && key != "loss_rank" {
					copyRow[key] = value
				}
			}
			rows = append(rows, copyRow)
		}
	}
	metrics := make(map[string]string, len(keys))
	for _, key := range keys {
		if key == "item_count" || key == "customer_count" || key == "row_count" {
			metrics[key] = integerText(totals[key].FloatString(0))
		} else {
			metrics[key] = totals[key].FloatString(4)
		}
	}
	return metrics, rows, nil
}

func attachChunkMetrics(rows []map[string]string, metrics map[string]string) []map[string]string {
	if len(rows) == 0 {
		rows = []map[string]string{{"_summary_metric_row": "true"}}
	}
	for _, row := range rows {
		for key, value := range metrics {
			row["_metric_"+key] = value
		}
	}
	return rows
}

func absRat(value *big.Rat) *big.Rat {
	if value.Sign() < 0 {
		return new(big.Rat).Neg(value)
	}
	return new(big.Rat).Set(value)
}
func compareRank(left, right *big.Rat, leftKey, rightKey string) bool {
	if cmp := left.Cmp(right); cmp != 0 {
		return cmp > 0
	}
	return leftKey < rightKey
}

func ChunkKeys(rows []map[string]string) ([]string, error) {
	keys := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		key := row["unit_key"]
		if _, exists := seen[key]; exists {
			continue
		}
		if len(key) > 512 {
			return nil, fmt.Errorf("chunk unit key is too long")
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func ChunkKey(index int, keys []string) string {
	sum := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
	return fmt.Sprintf("%06d:%s", index+1, hex.EncodeToString(sum[:8]))
}
