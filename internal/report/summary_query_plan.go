package report

import (
	"fmt"
	"strings"
	"time"
)

const summaryRowLimit = 200

// buildSummaryQueryPlan builds bounded SQL contracts for dashboards. Totals
// are calculated over the complete source set while only trend buckets and
// top-ranked rows cross JavaWS. Detail runs continue to use the original SQL.
func buildSummaryQueryPlan(key Key, period Period) (QueryPlan, error) {
	dateRangeArgs := []any{period.DateFrom, period.DateTo}
	plan := QueryPlan{ReportKey: key, Period: period}
	switch key {
	case SalesGoodsServices:
		plan.Steps = []QueryStep{
			{Name: "headers", Query: Query{SQL: summaryDocumentTrendSQL(salesHeaderSQL, period, false), Args: dateRangeArgs}},
			{Name: "details", Query: Query{SQL: summarySalesProductsSQL(), Args: dateRangeArgs}},
		}
	case PurchaseGoodsPayables:
		plan.Steps = []QueryStep{{Name: "headers", Query: Query{SQL: summaryDocumentTrendSQL(purchaseHeaderSQL, period, true), Args: dateRangeArgs}}}
	case GrossProfitByProduct:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryProfitSQL(grossProfitProductSQL, "code"), Args: dateRangeArgs}}}
	case GrossProfitByARCustomer:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryProfitSQL(grossProfitCustomerSQL, "ar_code"), Args: dateRangeArgs}}}
	case StockBalance:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryStockSQL(stockBalanceSQL), Args: dateRangeArgs}}}
	case StockReorder:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryReorderSQL()}}}
	case ARCustomerMovement:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryARMovementSQL(arCustomerMovementSQL), Args: []any{period.DateTo}}}}
	case ARDebtReceipt:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryCashFlowSQL(arDebtReceiptSQL, period, true, false), Args: dateRangeArgs}}}
	case CashBankReceipts:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryCashFlowSQL(cashBankReceiptsSQL, period, false, false), Args: dateRangeArgs}}}
	case CashBankPayments:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: summaryCashFlowSQL(cashBankPaymentsSQL, period, false, true), Args: dateRangeArgs}}}
	default:
		return QueryPlan{}, fmt.Errorf("report key %s is not approved", key)
	}
	return plan, nil
}

func trimFinalOrderBy(sql string) string {
	trimmed := strings.TrimSpace(sql)
	lower := strings.ToLower(trimmed)
	if index := strings.LastIndex(lower, "\norder by "); index >= 0 {
		trimmed = strings.TrimSpace(trimmed[:index])
	}
	return trimmed
}

func summaryBucketExpression(period Period, field string) string {
	from, fromErr := time.Parse(time.DateOnly, period.DateFrom)
	to, toErr := time.Parse(time.DateOnly, period.DateTo)
	if fromErr != nil || toErr != nil {
		return field + "::date"
	}
	days := int(to.Sub(from).Hours()/24) + 1
	if days > 186 {
		return "date_trunc('month', " + field + ")::date"
	}
	if days > 62 {
		return "$1::date + (((" + field + "::date - $1::date) / 7) * 7)"
	}
	return field + "::date"
}

func summaryDocumentTrendSQL(base string, period Period, includeSupplierRanking bool) string {
	bucket := summaryBucketExpression(period, "doc_date")
	rankingCTE := ""
	selected := "select * from trend_rows"
	if includeSupplierRanking {
		rankingCTE = `,
supplier_rows as (
  select null::date as doc_date, cust_code, max(cust_name) as cust_name,
    sum(total_amount) as total_amount, 'ranking'::text as _summary_kind
  from summary_source
  group by cust_code
  order by sum(total_amount) desc, cust_code
  limit 10
)`
		selected = "select * from trend_rows union all select * from supplier_rows"
	}
	return fmt.Sprintf(`
with summary_source as (%s),
summary_metrics as (
  select count(*) as _metric_document_count,
    coalesce(sum(total_amount), 0) as _metric_total_amount,
    coalesce(sum(total_amount), 0) as _metric_detail_total,
    count(*) as _metric_row_count
  from summary_source
),
trend_rows as (
  select %s as doc_date, ''::text as cust_code, ''::text as cust_name,
    sum(total_amount) as total_amount, 'trend'::text as _summary_kind
  from summary_source
  group by %s
  order by %s
)%s,
selected_rows as (%s)
select selected_rows.*, summary_metrics.*,
  (selected_rows._summary_kind is null)::text as _summary_metric_row
from summary_metrics left join selected_rows on true
limit %d`, trimFinalOrderBy(base), bucket, bucket, bucket, rankingCTE, selected, summaryRowLimit)
}

func summarySalesProductsSQL() string {
	return fmt.Sprintf(`
with summary_source as (%s),
summary_metrics as (
  select coalesce(sum(sum_amount), 0) as _metric_detail_total from summary_source
),
selected_rows as (
  select item_code, max(item_name) as item_name, sum(sum_amount) as sum_amount,
    'ranking'::text as _summary_kind
  from summary_source
  group by item_code
  order by sum(sum_amount) desc, item_code
  limit 10
)
select selected_rows.*, summary_metrics.*,
  (selected_rows.item_code is null)::text as _summary_metric_row
from summary_metrics left join selected_rows on true`, trimFinalOrderBy(salesDetailSQL))
}

func summaryProfitSQL(base, codeField string) string {
	return fmt.Sprintf(`
with summary_source as (%s),
summary_metrics as (
  select coalesce(sum(amount_sale), 0) as _metric_amount_sale,
    coalesce(sum(cost_sale), 0) as _metric_cost_sale,
    coalesce(sum(amount_sale_return), 0) as _metric_amount_sale_return,
    coalesce(sum(cost_sale_return), 0) as _metric_cost_sale_return,
    count(*) as _metric_row_count
  from summary_source
),
ranked as (
  select summary_source.*,
    (amount_sale - amount_sale_return) - (cost_sale - cost_sale_return) as _summary_profit,
    row_number() over (order by ((amount_sale - amount_sale_return) - (cost_sale - cost_sale_return)) desc, %s) as gain_rank,
    row_number() over (order by ((amount_sale - amount_sale_return) - (cost_sale - cost_sale_return)) asc, %s) as loss_rank
  from summary_source
),
selected_rows as (
  select * from ranked
  where (_summary_profit >= 0 and gain_rank <= 10) or (_summary_profit < 0 and loss_rank <= 10)
)
select selected_rows.*, summary_metrics.*,
  (selected_rows.%s is null)::text as _summary_metric_row
from summary_metrics left join selected_rows on true
limit 20`, trimFinalOrderBy(base), codeField, codeField, codeField)
}

func summaryStockSQL(base string) string {
	return fmt.Sprintf(`
with summary_source as (%s),
summary_metrics as (
  select count(*) as _metric_item_count,
    coalesce(sum(balance_amount), 0) as _metric_balance_amount,
    coalesce(sum(amount_in), 0) as _metric_amount_in,
    coalesce(sum(amount_out), 0) as _metric_amount_out,
    count(*) as _metric_row_count
  from summary_source
),
ranked as (
  select summary_source.*,
    row_number() over (order by abs(balance_amount) desc, ic_code) as balance_rank,
    row_number() over (order by abs(amount_in) + abs(amount_out) desc, ic_code) as movement_rank
  from summary_source
),
selected_rows as (
  select * from ranked where balance_rank <= 10 or movement_rank <= 10
)
select selected_rows.*, summary_metrics.*,
  (selected_rows.ic_code is null)::text as _summary_metric_row
from summary_metrics left join selected_rows on true
limit 20`, trimFinalOrderBy(base))
}

func summaryReorderSQL() string {
	return fmt.Sprintf(`
with summary_source as (%s),
summary_metrics as (
  select count(*) as _metric_reorder_item_count,
    coalesce(sum(greatest(purchase_point - balance_qty, 0)), 0) as _metric_shortage_qty,
    count(*) as _metric_row_count
  from summary_source
),
selected_rows as (
  select * from summary_source
  order by greatest(purchase_point - balance_qty, 0) / nullif(purchase_point, 0) desc, ic_code
  limit 10
)
select selected_rows.*, summary_metrics.*,
  (selected_rows.ic_code is null)::text as _summary_metric_row
from summary_metrics left join selected_rows on true`, trimFinalOrderBy(stockReorderSQL))
}

func summaryARMovementSQL(base string) string {
	return fmt.Sprintf(`
with summary_source as (%s),
customer_totals as (
  select cust_code, max(cust_name) as cust_name,
    coalesce(sum(case when doc_sort in (2, 3) then 0 else amount end), 0) as debit_amount,
    coalesce(sum(case when doc_sort in (2, 3) then amount else 0 end), 0) as credit_amount
  from summary_source
  group by cust_code
),
summary_metrics as (
  select count(*) as _metric_customer_count,
    coalesce(sum(debit_amount - credit_amount), 0) as _metric_net_movement_amount,
    coalesce(sum(debit_amount), 0) as _metric_debit_amount,
    coalesce(sum(credit_amount), 0) as _metric_credit_amount,
    count(*) as _metric_row_count
  from customer_totals
),
top_customers as (
  select * from customer_totals
  order by abs(debit_amount - credit_amount) desc, cust_code
  limit 10
),
selected_rows as (
  select cust_code, cust_name, 1 as doc_sort, debit_amount as amount from top_customers
  union all
  select cust_code, cust_name, 2 as doc_sort, credit_amount as amount from top_customers
)
select selected_rows.*, summary_metrics.*,
  (selected_rows.cust_code is null)::text as _summary_metric_row
from summary_metrics left join selected_rows on true
limit 20`, trimFinalOrderBy(base))
}

func summaryCashFlowSQL(base string, period Period, debtReceipt, payment bool) string {
	bucket := summaryBucketExpression(period, "doc_date")
	totalField := "total_amount"
	countMetric := "_metric_document_count"
	missingMetric := "0 as _metric_payment_split_missing_count"
	cardField := "coalesce(sum(card_amount), 0) as card_amount,"
	chequeField := "coalesce(sum(chq_amount), 0) as chq_amount,"
	incomeField := "coalesce(sum(total_income_amount), 0) as total_income_amount,"
	if debtReceipt {
		totalField = "total_net_value"
		countMetric = "_metric_receipt_count"
		missingMetric = "count(*) filter (where payment_split_missing) as _metric_payment_split_missing_count"
		cardField = "0::numeric as card_amount,"
		chequeField = "0::numeric as chq_amount,"
		incomeField = "0::numeric as total_income_amount,"
	}
	pettyField := "coalesce(sum(petty_cash_amount), 0) as petty_cash_amount,"
	if debtReceipt {
		pettyField = "0::numeric as petty_cash_amount,"
	}
	couponField := "coalesce(sum(coupon_amount), 0) as coupon_amount,"
	if payment || debtReceipt {
		couponField = "0::numeric as coupon_amount,"
	}
	return fmt.Sprintf(`
with summary_source as (%s),
summary_metrics as (
  select count(*) as %s,
    coalesce(sum(%s), 0) as _metric_total_amount,
    %s,
    count(*) as _metric_row_count
  from summary_source
),
selected_rows as (
  select %s as doc_date,
    coalesce(sum(%s), 0) as total_amount,
    coalesce(sum(%s), 0) as total_net_value,
    coalesce(sum(cash_amount), 0) as cash_amount,
    coalesce(sum(transfer_amount), 0) as transfer_amount,
    %s
    %s
    %s %s
    %s
    false as payment_split_missing
  from summary_source
  group by %s
  order by %s
)
select selected_rows.*, summary_metrics.*,
  (selected_rows.doc_date is null)::text as _summary_metric_row
from summary_metrics left join selected_rows on true
limit %d`, trimFinalOrderBy(base), countMetric, totalField, missingMetric,
		bucket, totalField, totalField, cardField, chequeField, pettyField, couponField, incomeField, bucket, bucket, summaryRowLimit)
}
