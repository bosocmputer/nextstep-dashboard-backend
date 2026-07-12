package report

import "time"

type Key string

const (
	SalesGoodsServices      Key = "sales_goods_services"
	PurchaseGoodsPayables   Key = "purchase_goods_payables"
	GrossProfitByProduct    Key = "gross_profit_by_product"
	GrossProfitByARCustomer Key = "gross_profit_by_ar_customer"
	StockBalance            Key = "stock_balance"
	StockReorder            Key = "stock_reorder"
	ARCustomerMovement      Key = "ar_customer_movement"
	ARDebtReceipt           Key = "ar_debt_receipt"
	CashBankReceipts        Key = "cash_bank_receipts"
	CashBankPayments        Key = "cash_bank_payments"
)

type ParameterKind string

const (
	DateRange   ParameterKind = "DATE_RANGE"
	AsOfDate    ParameterKind = "AS_OF_DATE"
	CurrentOnly ParameterKind = "CURRENT_ONLY"
)

type Metric struct {
	Key     string
	LabelTH string
}

type Status string

const (
	StatusActive     Status = "ACTIVE"
	StatusDeprecated Status = "DEPRECATED"
)

type Definition struct {
	Key             Key
	Version         string
	LabelTH         string
	Category        string
	CategoryLabelTH string
	Status          Status
	Sensitive       bool
	ParameterKind   ParameterKind
	LineMetrics     []Metric
	SummaryTimeout  time.Duration
	DetailTimeout   time.Duration
	MaxRows         int
	MaxRangeDays    int
}

var orderedDefinitions = []Definition{
	definition(SalesGoodsServices, "รายงานขายสินค้าและบริการ", "SALES", false, DateRange, "document_count", "เอกสาร", "total_amount", "ยอดขาย"),
	definition(PurchaseGoodsPayables, "รายงานซื้อสินค้าและตั้งหนี้", "PURCHASE", true, DateRange, "document_count", "เอกสาร", "total_amount", "ยอดซื้อ"),
	definition(GrossProfitByProduct, "กำไรขั้นต้นตามสินค้า", "GROSS_PROFIT", true, DateRange, "gross_profit_amount", "กำไรขั้นต้น", "gross_margin_percent", "อัตรากำไร"),
	definition(GrossProfitByARCustomer, "กำไรขั้นต้นตามลูกหนี้", "GROSS_PROFIT", true, DateRange, "gross_profit_amount", "กำไรขั้นต้น", "gross_margin_percent", "อัตรากำไร"),
	definition(StockBalance, "รายงานสต็อกคงเหลือ", "INVENTORY", true, AsOfDate, "item_count", "สินค้า", "balance_amount", "มูลค่าคงเหลือ"),
	definition(StockReorder, "รายงานสินค้าถึงจุดสั่งซื้อ", "INVENTORY", false, CurrentOnly, "reorder_item_count", "สินค้าต้องสั่ง", "shortage_qty", "จำนวนขาด"),
	definition(ARCustomerMovement, "รายงานความเคลื่อนไหวลูกหนี้", "AR", true, AsOfDate, "customer_count", "ลูกหนี้", "net_movement_amount", "ยอดเคลื่อนไหวสุทธิ"),
	definition(ARDebtReceipt, "รายงานรับชำระหนี้", "AR", true, DateRange, "receipt_count", "เอกสาร", "total_received_amount", "ยอดรับชำระ"),
	definition(CashBankReceipts, "รายงานรับเงิน", "CASH_BANK", true, DateRange, "document_count", "เอกสาร", "total_amount", "ยอดรับเงิน"),
	definition(CashBankPayments, "รายงานจ่ายเงิน", "CASH_BANK", true, DateRange, "document_count", "เอกสาร", "total_amount", "ยอดจ่ายเงิน"),
}

var definitionsByKey = func() map[Key]Definition {
	definitions := make(map[Key]Definition, len(orderedDefinitions))
	for _, item := range orderedDefinitions {
		definitions[item.Key] = item
	}
	return definitions
}()

func definition(key Key, label, category string, sensitive bool, parameterKind ParameterKind, firstMetricKey, firstMetricLabel, secondMetricKey, secondMetricLabel string) Definition {
	categoryLabels := map[string]string{
		"SALES": "ขาย", "PURCHASE": "ซื้อ", "GROSS_PROFIT": "กำไรขั้นต้น",
		"INVENTORY": "สินค้าคงคลัง", "AR": "ลูกหนี้", "CASH_BANK": "เงินสดและธนาคาร",
	}
	return Definition{
		Key:             key,
		Version:         "1.0.0",
		LabelTH:         label,
		Category:        category,
		CategoryLabelTH: categoryLabels[category],
		Status:          StatusActive,
		Sensitive:       sensitive,
		ParameterKind:   parameterKind,
		LineMetrics:     []Metric{{Key: firstMetricKey, LabelTH: firstMetricLabel}, {Key: secondMetricKey, LabelTH: secondMetricLabel}},
		SummaryTimeout:  30 * time.Second,
		DetailTimeout:   120 * time.Second,
		MaxRows:         200_000,
		MaxRangeDays:    366,
	}
}

func Keys() []Key {
	keys := make([]Key, 0, len(orderedDefinitions))
	for _, item := range orderedDefinitions {
		keys = append(keys, item.Key)
	}
	return keys
}

func Definitions() []Definition {
	return append([]Definition(nil), orderedDefinitions...)
}

func DefinitionFor(key Key) (Definition, bool) {
	definition, ok := definitionsByKey[key]
	return definition, ok
}

// CanSelect keeps deprecated report keys readable in existing configuration
// while preventing admins from adding them to new permission or schedule sets.
func CanSelect(definition Definition, alreadySelected bool) bool {
	return definition.Status == StatusActive || definition.Status == StatusDeprecated && alreadySelected
}
