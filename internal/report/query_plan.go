package report

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	dashboardBuilderContractVersion = "executive-dashboard-v1"
	formatterContractVersion        = "report-format-v1"
	summaryQueryContractVersion     = "bounded-summary-v1"
)

// Embedding the builder and formatter source makes cache invalidation follow
// implementation changes automatically. A code change cannot accidentally
// reuse output produced by the previous formula even if a human forgets to
// bump a version constant.
//
//go:embed dashboard_builder.go
var dashboardBuilderSource string

//go:embed summary.go
var summaryFormatterSource string

type QueryStep struct {
	Name  string
	Query Query
}

type QueryPlan struct {
	ReportKey Key
	Period    Period
	Steps     []QueryStep
}

// QueryPlanFingerprint invalidates cached report output when the SQL,
// projection, dashboard builder contract, or formatter contract changes.
// It intentionally excludes period values so equivalent resolved periods can
// share an execution while still separating SUMMARY from DETAIL output.
func QueryPlanFingerprint(key Key, projection ResultKind) string {
	if _, ok := DefinitionFor(key); !ok || projection != ResultSummary && projection != ResultDetail {
		return ""
	}
	queries := queryTemplates(key, projection)
	if len(queries) == 0 {
		return ""
	}
	parts := []string{
		string(key), string(projection), dashboardBuilderContractVersion, formatterContractVersion,
		normalizeSQLTemplate(dashboardBuilderSource), normalizeSQLTemplate(summaryFormatterSource),
	}
	for _, query := range queries {
		parts = append(parts, normalizeSQLTemplate(query))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func normalizeSQLTemplate(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func queryTemplates(key Key, projection ResultKind) []string {
	if projection == ResultSummary {
		// Cover every SQL bucket shape (daily, weekly and monthly). Cache keys
		// already contain the resolved period; these representative plans make
		// changes inside the SQL builder invalidate every shape automatically.
		templates := append([]string{summaryQueryContractVersion}, detailQueryTemplates(key)...)
		periods := []Period{
			{Preset: Custom, DateFrom: "2026-07-01", DateTo: "2026-07-01"},
			{Preset: Custom, DateFrom: "2026-01-01", DateTo: "2026-03-31"},
			{Preset: Custom, DateFrom: "2025-01-01", DateTo: "2025-12-31"},
		}
		for _, period := range periods {
			plan, err := buildSummaryQueryPlan(key, period)
			if err != nil {
				return nil
			}
			for _, step := range plan.Steps {
				templates = append(templates, step.Query.SQL)
			}
		}
		return templates
	}
	return detailQueryTemplates(key)
}

func detailQueryTemplates(key Key) []string {
	switch key {
	case SalesGoodsServices:
		return []string{salesHeaderSQL, salesDetailSQL}
	case PurchaseGoodsPayables:
		return []string{purchaseHeaderSQL, purchaseDetailSQL}
	case GrossProfitByProduct:
		return []string{grossProfitProductSQL}
	case GrossProfitByARCustomer:
		return []string{grossProfitCustomerSQL}
	case StockBalance:
		return []string{stockBalanceSQL}
	case StockReorder:
		return []string{stockReorderSQL}
	case ARCustomerMovement:
		return []string{arCustomerMovementSQL}
	case ARDebtReceipt:
		return []string{arDebtReceiptSQL}
	case CashBankReceipts:
		return []string{cashBankReceiptsSQL}
	case CashBankPayments:
		return []string{cashBankPaymentsSQL}
	default:
		return nil
	}
}

func BuildQueryPlan(key Key, period Period) (QueryPlan, error) {
	return BuildQueryPlanForProjection(key, period, ResultDetail)
}

func BuildQueryPlanForProjection(key Key, period Period, projection ResultKind) (QueryPlan, error) {
	if period.DateFrom == "" || period.DateTo == "" {
		return QueryPlan{}, errors.New("report query period is incomplete")
	}
	if _, ok := DefinitionFor(key); !ok {
		return QueryPlan{}, errors.New("report key is not approved")
	}
	if projection != ResultDetail && projection != ResultSummary {
		return QueryPlan{}, errors.New("report projection is not approved")
	}
	if projection == ResultSummary {
		return buildSummaryQueryPlan(key, period)
	}
	dateRangeArgs := []any{period.DateFrom, period.DateTo}
	plan := QueryPlan{ReportKey: key, Period: period}
	switch key {
	case SalesGoodsServices:
		plan.Steps = []QueryStep{
			{Name: "headers", Query: Query{SQL: salesHeaderSQL, Args: dateRangeArgs}},
			{Name: "details", Query: Query{SQL: salesDetailSQL, Args: dateRangeArgs}},
		}
	case PurchaseGoodsPayables:
		plan.Steps = []QueryStep{
			{Name: "headers", Query: Query{SQL: purchaseHeaderSQL, Args: dateRangeArgs}},
			{Name: "details", Query: Query{SQL: purchaseDetailSQL, Args: dateRangeArgs}},
		}
	case GrossProfitByProduct:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: grossProfitProductSQL, Args: dateRangeArgs}}}
	case GrossProfitByARCustomer:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: grossProfitCustomerSQL, Args: dateRangeArgs}}}
	case StockBalance:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: stockBalanceSQL, Args: dateRangeArgs}}}
	case StockReorder:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: stockReorderSQL}}}
	case ARCustomerMovement:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: arCustomerMovementSQL, Args: []any{period.DateTo}}}}
	case ARDebtReceipt:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: arDebtReceiptSQL, Args: dateRangeArgs}}}
	case CashBankReceipts:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: cashBankReceiptsSQL, Args: dateRangeArgs}}}
	case CashBankPayments:
		plan.Steps = []QueryStep{{Name: "rows", Query: Query{SQL: cashBankPaymentsSQL, Args: dateRangeArgs}}}
	}
	return plan, nil
}

const salesHeaderSQL = `
select
  h.doc_date,
  h.doc_no,
  h.doc_time,
  h.doc_ref_date,
  h.doc_ref,
  h.cust_code,
  c.name_1 as cust_name,
  h.branch_code,
  h.total_value,
  h.total_discount,
  (h.total_value - h.total_discount) as total_except_discount,
  h.total_except_vat,
  h.vat_rate,
  h.total_vat_value,
  case when h.vat_type = 0 then 'E' when h.vat_type = 1 then 'I' when h.vat_type = 2 then 'C' when h.vat_type = 3 then '3' end as vat_type,
  h.total_amount,
  h.cashier_code,
  cast(h.last_status as varchar) as last_status
from ic_trans h
left join ar_customer c on c.code = h.cust_code
where h.trans_flag in (44)
  and h.last_status = 0
  and h.doc_date between $1::date and $2::date
  and (coalesce(h.doc_ref, '') = '' or h.is_pos = 0)
  and h.is_doc_copy <> 1
order by h.doc_date, h.doc_no, h.doc_time, h.cust_code
`

const salesDetailSQL = `
with filtered_headers as (
  select h.doc_no, h.doc_date, h.doc_time, h.cust_code, c.name_1 as cust_name, h.branch_code, h.trans_flag
  from ic_trans h
  left join ar_customer c on c.code = h.cust_code
  where h.trans_flag in (44)
    and h.last_status = 0
    and h.doc_date between $1::date and $2::date
    and (coalesce(h.doc_ref, '') = '' or h.is_pos = 0)
    and h.is_doc_copy <> 1
)
select
  d.discount, d.discount_amount, d.doc_date, d.doc_no, h.doc_time,
  h.cust_code, h.cust_name,
  coalesce(nullif(h.branch_code, ''), 'no_branch') as branch_code,
  d.item_code, d.barcode, coalesce(i.name_1, d.item_name) as item_name,
  d.wh_code, d.shelf_code, d.unit_code, coalesce(u.name_1, '') as unit_name,
  d.qty, d.temp_float_1, d.temp_float_2, d.price, d.sum_amount,
  case when d.vat_type = 0 then 'E' when d.vat_type = 1 then 'I' when d.vat_type = 2 then 'C' when d.vat_type = 3 then '3' end as vat_type,
  cast(d.tax_type as varchar) as tax_type, d.ref_row, d.line_number
from ic_trans_detail d
inner join filtered_headers h on h.doc_no = d.doc_no and h.doc_date = d.doc_date and h.trans_flag = d.trans_flag
left join ic_inventory i on i.code = d.item_code
left join ic_unit u on u.code = d.unit_code
where d.trans_flag in (44)
  and d.last_status = 0
  and d.doc_date between $1::date and $2::date
order by d.doc_date, d.doc_no, d.line_number
`

const purchaseHeaderSQL = `
select
  h.doc_date, h.doc_no, h.doc_time, h.doc_ref_date, h.doc_ref,
  h.cust_code, s.name_1 as cust_name, h.branch_code,
  h.total_value, h.total_discount,
  (h.total_value - h.total_discount) as total_except_discount,
  h.total_except_vat, h.vat_rate, h.total_vat_value,
  case when h.vat_type = 0 then 'E' when h.vat_type = 1 then 'I' when h.vat_type = 2 then 'C' when h.vat_type = 3 then '3' end as vat_type,
  h.total_amount, h.cashier_code, cast(h.last_status as varchar) as last_status
from ic_trans h
left join ap_supplier s on s.code = h.cust_code
where h.trans_flag in (12)
  and h.last_status = 0
  and h.doc_date between $1::date and $2::date
  and h.is_doc_copy <> 1
order by h.doc_date, h.doc_no, h.doc_time, h.cust_code
`

const purchaseDetailSQL = `
with filtered_headers as (
  select h.doc_no, h.doc_date, h.doc_time, h.cust_code, s.name_1 as cust_name, h.branch_code, h.trans_flag
  from ic_trans h
  left join ap_supplier s on s.code = h.cust_code
  where h.trans_flag in (12)
    and h.last_status = 0
    and h.doc_date between $1::date and $2::date
    and h.is_doc_copy <> 1
)
select
  d.discount, d.discount_amount, d.doc_date, d.doc_no, h.doc_time,
  h.cust_code, h.cust_name,
  coalesce(nullif(d.branch_code, ''), nullif(h.branch_code, ''), 'no_branch') as branch_code,
  d.item_code, d.barcode, coalesce(i.name_1, d.item_name) as item_name,
  d.wh_code, d.shelf_code, d.unit_code, coalesce(u.name_1, '') as unit_name,
  d.qty, d.temp_float_1, d.temp_float_2, d.price, d.sum_amount,
  case when d.vat_type = 0 then 'E' when d.vat_type = 1 then 'I' when d.vat_type = 2 then 'C' when d.vat_type = 3 then '3' end as vat_type,
  cast(d.tax_type as varchar) as tax_type, d.ref_row, d.line_number
from ic_trans_detail d
inner join filtered_headers h on h.doc_no = d.doc_no and h.doc_date = d.doc_date and h.trans_flag = d.trans_flag
left join ic_inventory i on i.code = d.item_code
left join ic_unit u on u.code = d.unit_code
where d.trans_flag in (12)
  and d.last_status = 0
  and d.doc_date between $1::date and $2::date
order by d.doc_date, d.doc_no, d.line_number
`

const grossProfitProductSQL = `
with filtered_docs as (
  select h.doc_no, h.doc_date, h.trans_flag
  from ic_trans h
  where h.doc_date between $1::date and $2::date
    and h.trans_flag in (44, 46, 48)
),
detail_agg as (
  select
    d.item_code,
    coalesce(sum(case when (d.trans_flag in (44) or (d.trans_flag in (46) and d.inquiry_type in (0, 2))) then d.qty * (d.stand_value / nullif(d.divide_value, 0)) else 0 end), 0) as qty_sale,
    coalesce(sum(case when d.trans_flag in (44, 46) then d.sum_amount_exclude_vat else 0 end), 0) as amount_sale,
    coalesce(sum(case when d.trans_flag in (44, 46) then d.sum_of_cost else 0 end), 0) as cost_sale,
    coalesce(sum(case when d.trans_flag in (48) then d.qty * (d.stand_value / nullif(d.divide_value, 0)) else 0 end), 0) as qty_sale_return,
    coalesce(sum(case when d.trans_flag in (48) then d.sum_amount_exclude_vat else 0 end), 0) as amount_sale_return,
    coalesce(sum(case when d.trans_flag in (48) then d.sum_of_cost else 0 end), 0) as cost_sale_return
  from ic_trans_detail d
  where d.item_type <> 5 and d.item_type <> 3 and d.last_status = 0
    and d.doc_date between $1::date and $2::date
    and d.trans_flag in (44, 46, 48)
    and exists (
      select 1 from filtered_docs h
      where h.doc_no = d.doc_no and h.doc_date = d.doc_date and h.trans_flag = d.trans_flag
    )
  group by d.item_code
)
select
  i.code, i.name_1, i.unit_cost || '(' || coalesce(u.name_1, '') || ')' as unit_name,
  coalesce(a.qty_sale, 0) as qty_sale,
  coalesce(a.amount_sale, 0) as amount_sale,
  coalesce(a.cost_sale, 0) as cost_sale,
  coalesce(a.qty_sale_return, 0) as qty_sale_return,
  coalesce(a.amount_sale_return, 0) as amount_sale_return,
  coalesce(a.cost_sale_return, 0) as cost_sale_return
from ic_inventory i
left join detail_agg a on a.item_code = i.code
left join ic_unit u on u.code = i.unit_cost
where i.item_type <> 5
  and (coalesce(a.qty_sale, 0) <> 0 or coalesce(a.qty_sale_return, 0) <> 0)
order by i.code
`

const grossProfitCustomerSQL = `
with filtered_docs as (
  select h.doc_no, h.doc_date, h.trans_flag, h.cust_code
  from ic_trans h
  where h.doc_date between $1::date and $2::date
    and h.trans_flag in (44, 46, 48)
),
detail_by_doc as (
  select
    d.doc_no, d.doc_date, d.trans_flag,
    coalesce(sum(case when (d.trans_flag in (44) or (d.trans_flag in (46) and d.inquiry_type in (0, 2))) then d.qty * (d.stand_value / nullif(d.divide_value, 0)) else 0 end), 0) as qty_sale,
    coalesce(sum(case when d.trans_flag in (44, 46) then d.sum_amount_exclude_vat else 0 end), 0) as amount_sale,
    coalesce(sum(case when d.trans_flag in (44, 46) then d.sum_of_cost else 0 end), 0) as cost_sale,
    coalesce(sum(case when d.trans_flag in (48) then d.qty * (d.stand_value / nullif(d.divide_value, 0)) else 0 end), 0) as qty_sale_return,
    coalesce(sum(case when d.trans_flag in (48) then d.sum_amount_exclude_vat else 0 end), 0) as amount_sale_return,
    coalesce(sum(case when d.trans_flag in (48) then d.sum_of_cost else 0 end), 0) as cost_sale_return
  from ic_trans_detail d
  where d.item_type <> 5 and d.item_type <> 3 and d.last_status = 0
    and d.doc_date between $1::date and $2::date
    and d.trans_flag in (44, 46, 48)
    and exists (
      select 1 from filtered_docs h
      where h.doc_no = d.doc_no and h.doc_date = d.doc_date and h.trans_flag = d.trans_flag
    )
  group by d.doc_no, d.doc_date, d.trans_flag
)
select
  coalesce(nullif(t.cust_code, ''), 'ไม่ระบุลูกหนี้') as ar_code,
  coalesce(c.name_1, '') as ar_detail,
  coalesce(sum(d.qty_sale), 0) as qty_sale,
  coalesce(sum(d.amount_sale), 0) as amount_sale,
  coalesce(sum(d.cost_sale), 0) as cost_sale,
  coalesce(sum(d.qty_sale_return), 0) as qty_sale_return,
  coalesce(sum(d.amount_sale_return), 0) as amount_sale_return,
  coalesce(sum(d.cost_sale_return), 0) as cost_sale_return
from detail_by_doc d
inner join filtered_docs t on t.doc_no = d.doc_no and t.doc_date = d.doc_date and t.trans_flag = d.trans_flag
left join ar_customer c on c.code = t.cust_code
group by coalesce(nullif(t.cust_code, ''), 'ไม่ระบุลูกหนี้'), coalesce(c.name_1, '')
having coalesce(sum(d.qty_sale), 0) <> 0 or coalesce(sum(d.qty_sale_return), 0) <> 0
order by ar_code
`

const stockBalanceSQL = `
with inventory_scope as (
  select
    i.code, i.name_1, i.unit_standard as ic_unit_code,
    coalesce(i.unit_standard_stand_value / nullif(i.unit_standard_divide_value, 0), 1) as unit_ratio,
    coalesce(u.stand_value / nullif(u.divide_value, 0), 1) as unit_standard_ratio
  from ic_inventory i
  left join ic_unit_use u on u.ic_code = i.code and u.code = i.unit_standard
  where coalesce(i.item_type, 0) not in (1, 3)
),
base_detail as (
  select
    d.item_code, inv.name_1 as ic_name, inv.ic_unit_code, inv.unit_ratio, inv.unit_standard_ratio,
    d.doc_date_calc, d.doc_time, d.line_number, d.trans_flag, d.inquiry_type,
    d.qty, d.sum_of_cost, d.calc_flag, d.average_cost,
    coalesce(d.profit_lost_cost_amount, 0) as profit_lost_cost_amount,
    round((d.qty * d.stand_value) / nullif(d.divide_value, 0), 4) as standard_qty
  from ic_trans_detail d
  inner join inventory_scope inv on inv.code = d.item_code
  where d.last_status = 0
    and d.item_type <> 5
    and d.is_doc_copy = 0
    and d.doc_date_calc <= $2::date
    and not (coalesce(d.doc_ref, '') <> '' and d.is_pos = 1)
),
classified_detail as (
  select *,
    (trans_flag in (70, 54, 60, 58, 310, 12) or (trans_flag = 66 and qty > 0) or (trans_flag = 14 and inquiry_type = 0) or (trans_flag = 48 and inquiry_type < 2)) as is_qty_in,
    (trans_flag in (56, 68, 72, 44) or (trans_flag = 66 and qty < 0) or (trans_flag = 46 and inquiry_type in (0, 2)) or (trans_flag = 16 and inquiry_type in (0, 2)) or (trans_flag = 311 and inquiry_type = 0)) as is_qty_out,
    (trans_flag in (70, 54, 60, 58, 310, 12) or (trans_flag = 66 and (qty > 0 or sum_of_cost > 0)) or trans_flag = 14 or (trans_flag = 48 and inquiry_type < 2)) as is_amount_in,
    (trans_flag in (56, 68, 72, 44) or (trans_flag = 66 and (qty < 0 or sum_of_cost < 0)) or trans_flag = 46 or trans_flag = 16 or trans_flag = 311) as is_amount_out
  from base_detail
),
item_agg as (
  select
    item_code as ic_code,
    max(ic_name) as ic_name,
    max(ic_unit_code) as ic_unit_code,
    max(unit_ratio) as unit_ratio,
    max(unit_standard_ratio) as unit_standard_ratio,
    coalesce(sum(case when is_qty_in or is_qty_out then calc_flag * standard_qty else 0 end), 0) as balance_qty,
    coalesce(sum(case when is_amount_in or is_amount_out then calc_flag * (case when trans_flag = 66 and qty < 0 then (-1 * sum_of_cost) + profit_lost_cost_amount else sum_of_cost + profit_lost_cost_amount end) else 0 end), 0) as balance_amount,
    coalesce(sum(case when doc_date_calc >= $1::date and is_qty_in then calc_flag * standard_qty else 0 end), 0) as qty_in,
    coalesce(sum(case when doc_date_calc >= $1::date and is_amount_in then (calc_flag * sum_of_cost) + profit_lost_cost_amount else 0 end), 0) as amount_in,
    coalesce(sum(case when doc_date_calc >= $1::date and is_qty_out then -1 * calc_flag * standard_qty else 0 end), 0) as qty_out,
    coalesce(sum(case when doc_date_calc >= $1::date and is_amount_out then -1 * ((case when trans_flag = 66 and qty < 0 then -1 else calc_flag end) * (sum_of_cost + profit_lost_cost_amount)) else 0 end), 0) as amount_out
  from classified_detail
  group by item_code
),
latest_cost as (
  select distinct on (item_code) item_code, average_cost
  from classified_detail
  where is_amount_in or is_amount_out
  order by item_code, doc_date_calc desc, doc_time desc, line_number desc
)
select
  agg.ic_code, agg.ic_name, agg.ic_unit_code,
  coalesce(agg.balance_qty / nullif(agg.unit_standard_ratio, 0), 0) as balance_qty,
  coalesce(case when agg.balance_qty = 0 then 0 else agg.balance_amount / agg.balance_qty end * agg.unit_standard_ratio, 0) as average_cost,
  coalesce(latest.average_cost * agg.unit_ratio, 0) as average_cost_end,
  agg.balance_amount,
  coalesce(agg.qty_in / nullif(agg.unit_standard_ratio, 0), 0) as qty_in,
  agg.amount_in,
  coalesce(case when agg.qty_in = 0 then 0 else agg.amount_in / agg.qty_in end * agg.unit_standard_ratio, 0) as average_cost_in,
  coalesce(agg.qty_out / nullif(agg.unit_standard_ratio, 0), 0) as qty_out,
  agg.amount_out,
  coalesce(case when agg.qty_out = 0 then 0 else agg.amount_out / agg.qty_out end * agg.unit_standard_ratio, 0) as average_cost_out
from item_agg agg
left join latest_cost latest on latest.item_code = agg.ic_code
where agg.qty_in <> 0 or agg.amount_in <> 0 or agg.qty_out <> 0 or agg.amount_out <> 0 or agg.balance_qty <> 0 or agg.balance_amount <> 0
order by abs(agg.balance_amount) desc, agg.ic_code
`

const stockReorderSQL = `
with reorder_config as (
  select d.ic_code, max(coalesce(d.purchase_point, 0)) as purchase_point
  from ic_inventory_detail d
  group by d.ic_code
),
reorder_items as (
  select
    i.code as ic_code,
    i.name_1 as ic_name,
    coalesce(i.unit_standard, '') || '~' || coalesce(i.unit_standard_name, '') as ic_unit_code,
    coalesce(i.balance_qty, 0) as balance_qty,
    r.purchase_point,
    coalesce(i.accrued_in_qty, 0) as purchase_balance_qty
  from ic_inventory i
  inner join reorder_config r on r.ic_code = i.code
  where coalesce(i.item_type, 0) <> 5
    and r.purchase_point > 0
    and coalesce(i.balance_qty, 0) < r.purchase_point
)
select ic_code, ic_name, ic_unit_code, balance_qty, purchase_point, purchase_balance_qty
from reorder_items
order by ic_code
`

const arCustomerMovementSQL = `
with ar_docs as (
  select t.roworder, 1 as doc_sort, t.cust_code, coalesce(c.name_1, '') as cust_name,
    t.trans_flag as doc_type, t.doc_date, t.doc_no, t.tax_doc_no, t.doc_ref, t.credit_day, t.total_amount as amount
  from ic_trans t
  left join ar_customer c on c.code = t.cust_code
  where t.last_status = 0 and t.doc_date <= $1::date
    and ((t.trans_flag in (44, 250) and t.inquiry_type in (0, 2)) or t.trans_flag in (46) or t.trans_flag in (93, 99, 95, 101, 254, 418))
  union all
  select t.roworder, 2 as doc_sort, t.cust_code, coalesce(c.name_1, '') as cust_name,
    t.trans_flag as doc_type, t.doc_date, t.doc_no, t.tax_doc_no, t.doc_ref, t.credit_day, t.total_amount as amount
  from ic_trans t
  left join ar_customer c on c.code = t.cust_code
  where t.last_status = 0 and t.doc_date <= $1::date
    and ((t.trans_flag = 48 and t.inquiry_type in (0, 2, 4)) or t.trans_flag in (97, 103) or (t.trans_flag = 262 and t.inquiry_type not in (1, 3)))
  union all
  select t.roworder, 3 as doc_sort, t.cust_code, coalesce(c.name_1, '') as cust_name,
    t.trans_flag as doc_type, t.doc_date, t.doc_no, t.tax_doc_no, t.doc_ref, 0 as credit_day, t.total_net_value as amount
  from ap_ar_trans t
  left join ar_customer c on c.code = t.cust_code
  where t.last_status = 0 and t.doc_date <= $1::date and t.trans_flag = 239
  union all
  select t.roworder, 3 as doc_sort, t.cust_code, coalesce(c.name_1, '') as cust_name,
    t.trans_flag as doc_type, t.doc_date, t.doc_no, t.tax_doc_no, t.doc_ref, 0 as credit_day, t.total_amount as amount
  from as_trans t
  left join ar_customer c on c.code = t.cust_code
  where t.last_status = 0 and t.doc_date <= $1::date and t.trans_flag = 1802
)
select roworder, doc_sort, cust_code, cust_name, doc_type, doc_date, doc_no, tax_doc_no, doc_ref, credit_day, amount
from ar_docs
order by cust_code, doc_date, doc_sort, doc_no
`

const arDebtReceiptSQL = `
with billing_dates as (
  select d.doc_no, d.trans_flag, min(d.billing_date) as billing_date
  from ap_ar_trans_detail d
  where d.trans_flag = 239
  group by d.doc_no, d.trans_flag
),
payment_splits as (
  select p.doc_no, p.trans_flag,
    sum(coalesce(p.cash_amount, 0)) as cash_amount,
    sum(coalesce(p.tranfer_amount, 0)) as transfer_amount
  from cb_trans p
  where p.trans_flag = 239
  group by p.doc_no, p.trans_flag
)
select
  a.doc_date, a.doc_no, b.billing_date, a.cust_code,
  coalesce(c.name_1, '') as cust_name,
  coalesce(p.cash_amount, 0) as cash_amount,
  coalesce(p.transfer_amount, 0) as transfer_amount,
  coalesce(a.total_net_value, 0) as total_net_value,
  p.doc_no is null as payment_split_missing
from ap_ar_trans a
left join payment_splits p on p.doc_no = a.doc_no and p.trans_flag = a.trans_flag
left join ar_customer c on c.code = a.cust_code
left join billing_dates b on b.doc_no = a.doc_no and b.trans_flag = a.trans_flag
where a.trans_flag = 239
  and a.last_status = 0
  and a.doc_date between $1::date and $2::date
order by a.doc_date, a.doc_no
`

const cashBankReceiptsSQL = `
with filtered_cb as (
  select cb.*
  from cb_trans cb
  where cb.doc_date between $1::date and $2::date
    and cb.pay_type = 1
    and cb.status = 0
    and cb.trans_flag not in (144)
)
select
  cb.doc_date, cb.doc_no, cb.doc_time,
  cb.trans_flag as trans_flag_code,
  trans_flag(cb.trans_flag) as trans_flag_label,
  cb.ap_ar_code,
  coalesce((select c.name_1 from ar_customer c where c.code = cb.ap_ar_code), '') || ' (' || coalesce(cb.ap_ar_code, '') || ')' || ' ' || coalesce(cb.remark, '') as ap_ar_name,
  coalesce(cb.cash_amount, 0) as cash_amount,
  coalesce(cb.card_amount, 0) as card_amount,
  coalesce(cb.chq_amount, 0) as chq_amount,
  coalesce(cb.tranfer_amount, 0) as transfer_amount,
  coalesce(cb.total_income_amount, 0) as total_income_amount,
  coalesce(cb.coupon_amount, 0) as coupon_amount,
  0::numeric as petty_cash_amount,
  coalesce(cb.total_amount, 0) as total_amount
from filtered_cb cb
where (case when cb.trans_flag in (19, 239) then (select a.last_status from ap_ar_trans a where a.doc_no = cb.doc_no limit 1) else (select i.last_status from ic_trans i where i.doc_no = cb.doc_no limit 1) end) = 0
order by cb.doc_date, cb.trans_flag, cb.doc_no
`

const cashBankPaymentsSQL = `
select
  cb.doc_date, cb.doc_no, cb.doc_time,
  cb.trans_flag as trans_flag_code,
  trans_flag(cb.trans_flag) as trans_flag_label,
  cb.ap_ar_code,
  coalesce((select s.name_1 from ap_supplier s where s.code = cb.ap_ar_code), '') || ' (' || coalesce(cb.ap_ar_code, '') || ')' || ' ' || coalesce(cb.remark, '') as ap_ar_name,
  coalesce(cb.cash_amount, 0) as cash_amount,
  coalesce(cb.card_amount, 0) + coalesce(cb.total_credit_charge, 0) as card_amount,
  coalesce(cb.chq_amount, 0) as chq_amount,
  coalesce(cb.tranfer_amount, 0) as transfer_amount,
  coalesce(cb.total_income_amount, 0) as total_income_amount,
  0::numeric as coupon_amount,
  coalesce(cb.petty_cash_amount, 0) as petty_cash_amount,
  coalesce(cb.total_amount, 0) as total_amount
from cb_trans cb
where cb.doc_date between $1::date and $2::date
  and cb.pay_type = 2
  and cb.status = 0
order by cb.doc_date, cb.trans_flag, cb.doc_no
`
