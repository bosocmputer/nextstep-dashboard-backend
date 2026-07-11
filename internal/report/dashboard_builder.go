package report

import (
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"time"
)

const dashboardTopLimit = 10

type dashboardMetricInput struct {
	key, label, current, previous string
	unit                          MetricUnit
}

type rankedValue struct {
	code  string
	label string
	value *big.Rat
	row   map[string]string
}

func BuildDashboard(key Key, period, comparisonPeriod Period, currentSteps, previousSteps map[string][]map[string]string) (Dashboard, error) {
	definition, ok := DefinitionFor(key)
	if !ok {
		return Dashboard{}, fmt.Errorf("dashboard report key %s is not approved", key)
	}
	currentSummary, err := Summarize(key, currentSteps)
	if err != nil {
		return Dashboard{}, fmt.Errorf("summarize current dashboard period: %w", err)
	}
	previousSummary, err := Summarize(key, previousSteps)
	if err != nil {
		return Dashboard{}, fmt.Errorf("summarize comparison dashboard period: %w", err)
	}

	dashboard := Dashboard{
		ReportKey: key, Version: definition.Version, Period: period, ComparisonPeriod: comparisonPeriod,
		Timezone: "Asia/Bangkok", Quality: DashboardQuality{Status: "OK", Warnings: []string{}},
	}
	metricInputs, err := buildDashboardMetrics(key, currentSummary, previousSummary, currentSteps, previousSteps)
	if err != nil {
		return Dashboard{}, err
	}
	for _, input := range metricInputs {
		metric, metricErr := newDashboardMetric(input)
		if metricErr != nil {
			return Dashboard{}, fmt.Errorf("build dashboard KPI %s: %w", input.key, metricErr)
		}
		dashboard.KPIs = append(dashboard.KPIs, metric)
	}
	visualizations, err := buildDashboardVisualizations(key, period, comparisonPeriod, currentSteps, previousSteps)
	if err != nil {
		return Dashboard{}, err
	}
	if visualizations == nil {
		visualizations = []DashboardVisualization{}
	}
	dashboard.Visualizations = visualizations
	if !ComparisonSupported(key, period) {
		SetComparisonUnavailable(&dashboard, ComparisonUnavailableReason(key, period))
	}
	return dashboard, nil
}

func ComparisonUnavailableReason(key Key, period Period) string {
	if key == StockReorder {
		return "COMPARISON_UNAVAILABLE_FOR_REPORT"
	}
	if period.Preset == TodayToNow {
		return "COMPARISON_TIME_WINDOW_UNAVAILABLE"
	}
	return "COMPARISON_UNAVAILABLE_FOR_PERIOD"
}

// ComparisonSupported prevents unlike time windows and current-only reports
// from being presented as historical comparisons.
func ComparisonSupported(key Key, period Period) bool {
	if _, ok := DefinitionFor(key); !ok || key == StockReorder || period.Preset == TodayToNow {
		return false
	}
	if period.Preset == AsOfRun {
		return key == StockBalance || key == ARCustomerMovement
	}
	return period.Preset == Yesterday || period.Preset == MonthToDate || period.Preset == Custom
}

// SetComparisonUnavailable removes every previous-period value from a
// dashboard while preserving the current-period KPIs and visualizations.
func SetComparisonUnavailable(dashboard *Dashboard, warning string) {
	if dashboard == nil {
		return
	}
	for index := range dashboard.KPIs {
		dashboard.KPIs[index].Comparison = MetricComparison{Availability: ComparisonUnavailable}
	}
	for visualizationIndex := range dashboard.Visualizations {
		series := dashboard.Visualizations[visualizationIndex].Series[:0]
		for _, item := range dashboard.Visualizations[visualizationIndex].Series {
			if item.Key != "previous" {
				series = append(series, item)
			}
		}
		dashboard.Visualizations[visualizationIndex].Series = series
	}
	if warning == "" {
		return
	}
	dashboard.Quality.Status = "WARNING"
	for _, existing := range dashboard.Quality.Warnings {
		if existing == warning {
			return
		}
	}
	dashboard.Quality.Warnings = append(dashboard.Quality.Warnings, warning)
}

func buildDashboardMetrics(key Key, current, previous SummaryResult, currentSteps, previousSteps map[string][]map[string]string) ([]dashboardMetricInput, error) {
	metric := func(key, label string, unit MetricUnit) dashboardMetricInput {
		return dashboardMetricInput{key: key, label: label, unit: unit, current: current.Metrics[key], previous: previous.Metrics[key]}
	}
	average := func(totalKey, countKey string, unit MetricUnit, key, label string) (dashboardMetricInput, error) {
		currentAverage, err := divideDecimal(current.Metrics[totalKey], current.Metrics[countKey], unit)
		if err != nil {
			return dashboardMetricInput{}, err
		}
		previousAverage, err := divideDecimal(previous.Metrics[totalKey], previous.Metrics[countKey], unit)
		if err != nil {
			return dashboardMetricInput{}, err
		}
		return dashboardMetricInput{key: key, label: label, unit: unit, current: currentAverage, previous: previousAverage}, nil
	}

	switch key {
	case SalesGoodsServices, PurchaseGoodsPayables:
		label := "ยอดขาย"
		averageLabel := "ยอดเฉลี่ยต่อเอกสาร"
		if key == PurchaseGoodsPayables {
			label = "ยอดซื้อ"
		}
		averageMetric, err := average("total_amount", "document_count", UnitTHB, "average_per_document", averageLabel)
		if err != nil {
			return nil, err
		}
		return []dashboardMetricInput{metric("total_amount", label, UnitTHB), metric("document_count", "จำนวนเอกสาร", UnitCount), averageMetric}, nil
	case GrossProfitByProduct, GrossProfitByARCustomer:
		return []dashboardMetricInput{
			metric("gross_profit_amount", "กำไรขั้นต้น", UnitTHB),
			metric("gross_margin_percent", "อัตรากำไรขั้นต้น", UnitPercent),
			{key: "net_amount", label: "ยอดขายสุทธิ", unit: UnitTHB, current: stringValue(current.Reconciliation["netAmount"]), previous: stringValue(previous.Reconciliation["netAmount"])},
			{key: "net_cost", label: "ต้นทุนสุทธิ", unit: UnitTHB, current: stringValue(current.Reconciliation["netCost"]), previous: stringValue(previous.Reconciliation["netCost"])},
		}, nil
	case StockBalance:
		currentIn, err := sumField(currentSteps["rows"], "amount_in")
		if err != nil {
			return nil, err
		}
		previousIn, err := sumField(previousSteps["rows"], "amount_in")
		if err != nil {
			return nil, err
		}
		currentOut, err := sumField(currentSteps["rows"], "amount_out")
		if err != nil {
			return nil, err
		}
		previousOut, err := sumField(previousSteps["rows"], "amount_out")
		if err != nil {
			return nil, err
		}
		return []dashboardMetricInput{
			metric("balance_amount", "มูลค่าสต็อกคงเหลือ", UnitTHB), metric("item_count", "จำนวนสินค้า", UnitCount),
			{key: "amount_in", label: "มูลค่ารับเข้า", unit: UnitTHB, current: money(currentIn), previous: money(previousIn)},
			{key: "amount_out", label: "มูลค่าจ่ายออก", unit: UnitTHB, current: money(currentOut), previous: money(previousOut)},
		}, nil
	case StockReorder:
		return []dashboardMetricInput{metric("reorder_item_count", "สินค้าที่ต้องสั่ง", UnitCount), metric("shortage_qty", "จำนวนขาดรวม", UnitQuantity)}, nil
	case ARCustomerMovement:
		currentDebit, currentCredit, err := movementTotals(currentSteps["rows"])
		if err != nil {
			return nil, err
		}
		previousDebit, previousCredit, err := movementTotals(previousSteps["rows"])
		if err != nil {
			return nil, err
		}
		return []dashboardMetricInput{
			metric("net_movement_amount", "ยอดเคลื่อนไหวสุทธิ", UnitTHB), metric("customer_count", "จำนวนลูกหนี้", UnitCount),
			{key: "debit_amount", label: "ยอดเพิ่ม", unit: UnitTHB, current: money(currentDebit), previous: money(previousDebit)},
			{key: "credit_amount", label: "ยอดลด", unit: UnitTHB, current: money(currentCredit), previous: money(previousCredit)},
		}, nil
	case ARDebtReceipt:
		averageMetric, err := average("total_received_amount", "receipt_count", UnitTHB, "average_per_receipt", "ยอดเฉลี่ยต่อเอกสาร")
		if err != nil {
			return nil, err
		}
		return []dashboardMetricInput{
			metric("total_received_amount", "ยอดรับชำระ", UnitTHB), metric("receipt_count", "จำนวนเอกสาร", UnitCount), averageMetric,
			{key: "payment_split_missing_count", label: "เอกสารแยกวิธีชำระไม่ครบ", unit: UnitCount, current: strconv.Itoa(countTrue(currentSteps["rows"], "payment_split_missing")), previous: strconv.Itoa(countTrue(previousSteps["rows"], "payment_split_missing"))},
		}, nil
	case CashBankReceipts, CashBankPayments:
		label := "ยอดรับเงิน"
		if key == CashBankPayments {
			label = "ยอดจ่ายเงิน"
		}
		averageMetric, err := average("total_amount", "document_count", UnitTHB, "average_per_document", "ยอดเฉลี่ยต่อเอกสาร")
		if err != nil {
			return nil, err
		}
		return []dashboardMetricInput{metric("total_amount", label, UnitTHB), metric("document_count", "จำนวนเอกสาร", UnitCount), averageMetric}, nil
	default:
		return nil, fmt.Errorf("dashboard metrics are not defined for %s", key)
	}
}

func newDashboardMetric(input dashboardMetricInput) (DashboardMetric, error) {
	current, err := decimal(input.current)
	if err != nil {
		return DashboardMetric{}, err
	}
	previous, err := decimal(input.previous)
	if err != nil {
		return DashboardMetric{}, err
	}
	delta := new(big.Rat).Sub(current, previous)
	direction := DirectionSame
	if delta.Sign() > 0 {
		direction = DirectionUp
	} else if delta.Sign() < 0 {
		direction = DirectionDown
	}
	comparison := MetricComparison{
		Availability: ComparisonAvailable, PreviousValue: formatUnit(previous, input.unit), Delta: formatUnit(delta, input.unit), Direction: direction,
	}
	if previous.Sign() != 0 {
		percent := new(big.Rat).Mul(delta, big.NewRat(100, 1))
		percent.Quo(percent, previous)
		comparison.Percent = percent.FloatString(2)
	}
	return DashboardMetric{Key: input.key, Label: input.label, Value: formatUnit(current, input.unit), Unit: input.unit, Comparison: comparison}, nil
}

func buildDashboardVisualizations(key Key, period, comparisonPeriod Period, currentSteps, previousSteps map[string][]map[string]string) ([]DashboardVisualization, error) {
	switch key {
	case SalesGoodsServices:
		trend, err := buildTrend("sales_trend", "แนวโน้มยอดขาย", UnitTHB, period, comparisonPeriod, currentSteps["headers"], previousSteps["headers"], "total_amount")
		if err != nil {
			return nil, err
		}
		ranking, err := buildRanking("top_products", "สินค้าทำยอดขายสูงสุด", UnitTHB, currentSteps["details"], "item_code", "item_name", func(row map[string]string) (*big.Rat, error) { return decimal(row["sum_amount"]) }, false)
		return compactVisualizations(trend, ranking), err
	case PurchaseGoodsPayables:
		trend, err := buildTrend("purchase_trend", "แนวโน้มยอดซื้อ", UnitTHB, period, comparisonPeriod, currentSteps["headers"], previousSteps["headers"], "total_amount")
		if err != nil {
			return nil, err
		}
		ranking, err := buildRanking("top_suppliers", "ผู้จำหน่ายที่มียอดซื้อสูงสุด", UnitTHB, currentSteps["headers"], "cust_code", "cust_name", func(row map[string]string) (*big.Rat, error) { return decimal(row["total_amount"]) }, false)
		return compactVisualizations(trend, ranking), err
	case GrossProfitByProduct:
		positive, err := buildProfitRanking("top_profit_products", "สินค้ากำไรสูงสุด", currentSteps["rows"], "code", "name_1", false)
		if err != nil {
			return nil, err
		}
		negative, err := buildProfitRanking("loss_products", "สินค้าที่ขาดทุน", currentSteps["rows"], "code", "name_1", true)
		return compactVisualizations(positive, negative), err
	case GrossProfitByARCustomer:
		positive, err := buildProfitRanking("top_profit_customers", "ลูกค้าที่สร้างกำไรสูงสุด", currentSteps["rows"], "ar_code", "ar_detail", false)
		if err != nil {
			return nil, err
		}
		negative, err := buildProfitRanking("loss_customers", "ลูกค้าที่ขาดทุน", currentSteps["rows"], "ar_code", "ar_detail", true)
		return compactVisualizations(positive, negative), err
	case StockBalance:
		balance, err := buildRanking("top_stock_value", "สินค้าที่มีมูลค่าคงเหลือสูงสุด", UnitTHB, currentSteps["rows"], "ic_code", "ic_name", func(row map[string]string) (*big.Rat, error) { return decimal(row["balance_amount"]) }, false)
		if err != nil {
			return nil, err
		}
		movement, err := buildStockMovement(currentSteps["rows"])
		return compactVisualizations(balance, movement), err
	case StockReorder:
		reorder, err := buildReorderExceptions(currentSteps["rows"])
		return compactVisualizations(reorder), err
	case ARCustomerMovement:
		return buildMovementVisualizations(currentSteps["rows"])
	case ARDebtReceipt:
		trend, err := buildTrend("debt_receipt_trend", "แนวโน้มรับชำระหนี้", UnitTHB, period, comparisonPeriod, currentSteps["rows"], previousSteps["rows"], "total_net_value")
		if err != nil {
			return nil, err
		}
		composition, err := buildComposition("debt_receipt_methods", "วิธีรับชำระ", currentSteps["rows"], []fieldLabel{{"cash_amount", "เงินสด"}, {"transfer_amount", "เงินโอน"}})
		return compactVisualizations(trend, composition), err
	case CashBankReceipts, CashBankPayments:
		title := "แนวโน้มรับเงิน"
		keyPrefix := "cash_receipt"
		fields := []fieldLabel{{"cash_amount", "เงินสด"}, {"card_amount", "บัตร"}, {"chq_amount", "เช็ค"}, {"transfer_amount", "เงินโอน"}, {"coupon_amount", "คูปอง"}, {"total_income_amount", "รายได้อื่น"}}
		if key == CashBankPayments {
			title, keyPrefix = "แนวโน้มจ่ายเงิน", "cash_payment"
			fields = []fieldLabel{{"cash_amount", "เงินสด"}, {"card_amount", "บัตร"}, {"chq_amount", "เช็ค"}, {"transfer_amount", "เงินโอน"}, {"petty_cash_amount", "เงินสดย่อย"}, {"total_income_amount", "รายการอื่น"}}
		}
		trend, err := buildTrend(keyPrefix+"_trend", title, UnitTHB, period, comparisonPeriod, currentSteps["rows"], previousSteps["rows"], "total_amount")
		if err != nil {
			return nil, err
		}
		composition, err := buildComposition(keyPrefix+"_methods", "ช่องทางการชำระ", currentSteps["rows"], fields)
		return compactVisualizations(trend, composition), err
	default:
		return nil, fmt.Errorf("dashboard visualizations are not defined for %s", key)
	}
}

func buildTrend(key, title string, unit MetricUnit, period, comparisonPeriod Period, currentRows, previousRows []map[string]string, amountField string) (DashboardVisualization, error) {
	currentBuckets, currentLabels, err := aggregatePeriodRows(period, currentRows, amountField)
	if err != nil {
		return DashboardVisualization{}, err
	}
	previousBuckets, previousLabels, err := aggregatePeriodRows(comparisonPeriod, previousRows, amountField)
	if err != nil {
		return DashboardVisualization{}, err
	}
	count := len(currentBuckets)
	if len(previousBuckets) > count {
		count = len(previousBuckets)
	}
	if count == 0 {
		return DashboardVisualization{}, nil
	}
	categories := make([]string, count)
	currentValues, previousValues := make([]string, count), make([]string, count)
	currentPointLabels, previousPointLabels := make([]string, count), make([]string, count)
	for index := 0; index < count; index++ {
		categories[index] = strconv.Itoa(index + 1)
		currentValues[index], previousValues[index] = formatBucket(currentBuckets, index), formatBucket(previousBuckets, index)
		currentPointLabels[index], previousPointLabels[index] = labelAt(currentLabels, index), labelAt(previousLabels, index)
	}
	return DashboardVisualization{
		Key: key, Title: title, Intent: IntentTrend, Unit: unit, Categories: categories,
		Series: []VisualizationSeries{
			{Key: "current", Label: "งวดปัจจุบัน", Values: currentValues, PointLabels: currentPointLabels},
			{Key: "previous", Label: "งวดก่อน", Values: previousValues, PointLabels: previousPointLabels},
		},
	}, nil
}

func aggregatePeriodRows(period Period, rows []map[string]string, amountField string) ([]*big.Rat, []string, error) {
	from, err := time.Parse(time.DateOnly, period.DateFrom)
	if err != nil {
		return nil, nil, err
	}
	to, err := time.Parse(time.DateOnly, period.DateTo)
	if err != nil || to.Before(from) {
		return nil, nil, fmt.Errorf("invalid trend period")
	}
	days := int(to.Sub(from).Hours()/24) + 1
	bucketMode := "day"
	if days > 186 {
		bucketMode = "month"
	} else if days > 62 {
		bucketMode = "week"
	}
	labels, indexes := periodBuckets(from, to, bucketMode)
	values := make([]*big.Rat, len(labels))
	for index := range values {
		values[index] = new(big.Rat)
	}
	for _, row := range rows {
		date, parseErr := time.Parse(time.DateOnly, row["doc_date"])
		if parseErr != nil || date.Before(from) || date.After(to) {
			continue
		}
		value, parseErr := decimal(row[amountField])
		if parseErr != nil {
			return nil, nil, fieldDecimalError(amountField, parseErr)
		}
		bucketKey := dashboardBucketKey(from, date, bucketMode)
		if index, exists := indexes[bucketKey]; exists {
			values[index].Add(values[index], value)
		}
	}
	return values, labels, nil
}

func periodBuckets(from, to time.Time, mode string) ([]string, map[string]int) {
	labels := make([]string, 0)
	indexes := make(map[string]int)
	for date := from; !date.After(to); date = date.AddDate(0, 0, 1) {
		key := dashboardBucketKey(from, date, mode)
		if _, exists := indexes[key]; exists {
			continue
		}
		indexes[key] = len(labels)
		labels = append(labels, key)
	}
	return labels, indexes
}

func dashboardBucketKey(from, date time.Time, mode string) string {
	switch mode {
	case "week":
		week := int(date.Sub(from).Hours()/24) / 7
		start := from.AddDate(0, 0, week*7)
		return start.Format(time.DateOnly)
	case "month":
		return date.Format("2006-01")
	default:
		return date.Format(time.DateOnly)
	}
}

func buildRanking(key, title string, unit MetricUnit, rows []map[string]string, codeField, nameField string, value func(map[string]string) (*big.Rat, error), ascending bool) (DashboardVisualization, error) {
	grouped := make(map[string]*rankedValue)
	for _, row := range rows {
		amount, err := value(row)
		if err != nil {
			return DashboardVisualization{}, err
		}
		code := row[codeField]
		label := row[nameField]
		if label == "" {
			label = code
		}
		if existing := grouped[code]; existing != nil {
			existing.value.Add(existing.value, amount)
			continue
		}
		grouped[code] = &rankedValue{code: code, label: label, value: new(big.Rat).Set(amount), row: row}
	}
	items := make([]rankedValue, 0, len(grouped))
	for _, item := range grouped {
		if ascending && item.value.Sign() >= 0 {
			continue
		}
		if !ascending && item.value.Sign() <= 0 {
			continue
		}
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool {
		comparison := items[i].value.Cmp(items[j].value)
		if comparison == 0 {
			return items[i].code < items[j].code
		}
		if ascending {
			return comparison < 0
		}
		return comparison > 0
	})
	if len(items) > dashboardTopLimit {
		items = items[:dashboardTopLimit]
	}
	if len(items) == 0 {
		return DashboardVisualization{}, nil
	}
	categories, values := make([]string, len(items)), make([]string, len(items))
	for index, item := range items {
		categories[index], values[index] = item.label, formatUnit(item.value, unit)
	}
	return DashboardVisualization{
		Key: key, Title: title, Intent: IntentRanking, Unit: unit, Categories: categories,
		Series: []VisualizationSeries{{Key: "value", Label: title, Values: values}},
	}, nil
}

func buildProfitRanking(key, title string, rows []map[string]string, codeField, nameField string, negative bool) (DashboardVisualization, error) {
	return buildRanking(key, title, UnitTHB, rows, codeField, nameField, grossProfitForRow, negative)
}

func grossProfitForRow(row map[string]string) (*big.Rat, error) {
	amount, err := decimal(row["amount_sale"])
	if err != nil {
		return nil, err
	}
	cost, err := decimal(row["cost_sale"])
	if err != nil {
		return nil, err
	}
	amountReturn, err := decimal(row["amount_sale_return"])
	if err != nil {
		return nil, err
	}
	costReturn, err := decimal(row["cost_sale_return"])
	if err != nil {
		return nil, err
	}
	return new(big.Rat).Sub(new(big.Rat).Sub(amount, amountReturn), new(big.Rat).Sub(cost, costReturn)), nil
}

func buildStockMovement(rows []map[string]string) (DashboardVisualization, error) {
	ranking, err := rankedRows(rows, "ic_code", "ic_name", func(row map[string]string) (*big.Rat, error) {
		amountIn, err := decimal(row["amount_in"])
		if err != nil {
			return nil, err
		}
		amountOut, err := decimal(row["amount_out"])
		if err != nil {
			return nil, err
		}
		return new(big.Rat).Add(amountIn, amountOut), nil
	})
	if err != nil || len(ranking) == 0 {
		return DashboardVisualization{}, err
	}
	categories, inValues, outValues := make([]string, len(ranking)), make([]string, len(ranking)), make([]string, len(ranking))
	for index, item := range ranking {
		amountIn, parseErr := decimal(item.row["amount_in"])
		if parseErr != nil {
			return DashboardVisualization{}, parseErr
		}
		amountOut, parseErr := decimal(item.row["amount_out"])
		if parseErr != nil {
			return DashboardVisualization{}, parseErr
		}
		categories[index], inValues[index], outValues[index] = item.label, money(amountIn), money(amountOut)
	}
	return DashboardVisualization{
		Key: "stock_movement", Title: "มูลค่ารับเข้าและจ่ายออกตามสินค้า", Intent: IntentComposition, Unit: UnitTHB, Categories: categories,
		Series: []VisualizationSeries{{Key: "amount_in", Label: "รับเข้า", Values: inValues}, {Key: "amount_out", Label: "จ่ายออก", Values: outValues}},
	}, nil
}

func buildReorderExceptions(rows []map[string]string) (DashboardVisualization, error) {
	return buildRanking("reorder_shortage_ratio", "สินค้าที่ต่ำกว่าจุดสั่งซื้อมากที่สุด", UnitPercent, rows, "ic_code", "ic_name", func(row map[string]string) (*big.Rat, error) {
		balance, err := decimal(row["balance_qty"])
		if err != nil {
			return nil, err
		}
		point, err := decimal(row["purchase_point"])
		if err != nil {
			return nil, err
		}
		if point.Sign() <= 0 {
			return new(big.Rat), nil
		}
		shortage := new(big.Rat).Sub(point, balance)
		if shortage.Sign() < 0 {
			shortage.SetInt64(0)
		}
		return shortage.Mul(shortage, big.NewRat(100, 1)).Quo(shortage, point), nil
	}, false)
}

func buildMovementVisualizations(rows []map[string]string) ([]DashboardVisualization, error) {
	type totals struct{ debit, credit *big.Rat }
	grouped := make(map[string]*totals)
	labels := make(map[string]string)
	for _, row := range rows {
		value, err := decimal(row["amount"])
		if err != nil {
			return nil, err
		}
		code := row["cust_code"]
		if grouped[code] == nil {
			grouped[code] = &totals{debit: new(big.Rat), credit: new(big.Rat)}
			labels[code] = firstNonEmpty(row["cust_name"], code)
		}
		if row["doc_sort"] == "2" || row["doc_sort"] == "3" {
			grouped[code].credit.Add(grouped[code].credit, value)
		} else {
			grouped[code].debit.Add(grouped[code].debit, value)
		}
	}
	items := make([]rankedValue, 0, len(grouped))
	for code, total := range grouped {
		items = append(items, rankedValue{code: code, label: labels[code], value: new(big.Rat).Sub(total.debit, total.credit)})
	}
	sort.Slice(items, func(i, j int) bool {
		left, right := new(big.Rat).Abs(items[i].value), new(big.Rat).Abs(items[j].value)
		if compared := left.Cmp(right); compared != 0 {
			return compared > 0
		}
		return items[i].code < items[j].code
	})
	if len(items) > dashboardTopLimit {
		items = items[:dashboardTopLimit]
	}
	if len(items) == 0 {
		return nil, nil
	}
	categories, netValues, debitValues, creditValues := make([]string, len(items)), make([]string, len(items)), make([]string, len(items)), make([]string, len(items))
	for index, item := range items {
		total := grouped[item.code]
		categories[index], netValues[index] = item.label, money(item.value)
		debitValues[index], creditValues[index] = money(total.debit), money(total.credit)
	}
	return []DashboardVisualization{
		{Key: "customer_net_movement", Title: "ลูกหนี้ที่มียอดเคลื่อนไหวสุทธิสูงสุด", Intent: IntentRanking, Unit: UnitTHB, Categories: categories, Series: []VisualizationSeries{{Key: "net", Label: "ยอดสุทธิ", Values: netValues}}},
		{Key: "customer_debit_credit", Title: "ยอดเพิ่มและยอดลดตามลูกหนี้", Intent: IntentComposition, Unit: UnitTHB, Categories: categories, Series: []VisualizationSeries{{Key: "debit", Label: "ยอดเพิ่ม", Values: debitValues}, {Key: "credit", Label: "ยอดลด", Values: creditValues}}},
	}, nil
}

type fieldLabel struct{ field, label string }

func buildComposition(key, title string, rows []map[string]string, fields []fieldLabel) (DashboardVisualization, error) {
	categories, values := make([]string, 0, len(fields)), make([]string, 0, len(fields))
	for _, item := range fields {
		total, err := sumField(rows, item.field)
		if err != nil {
			return DashboardVisualization{}, err
		}
		if total.Sign() == 0 {
			continue
		}
		categories, values = append(categories, item.label), append(values, money(total))
	}
	if len(categories) == 0 {
		return DashboardVisualization{}, nil
	}
	return DashboardVisualization{Key: key, Title: title, Intent: IntentComposition, Unit: UnitTHB, Categories: categories, Series: []VisualizationSeries{{Key: "amount", Label: "จำนวนเงิน", Values: values}}}, nil
}

func rankedRows(rows []map[string]string, codeField, nameField string, value func(map[string]string) (*big.Rat, error)) ([]rankedValue, error) {
	items := make([]rankedValue, 0, len(rows))
	for _, row := range rows {
		amount, err := value(row)
		if err != nil {
			return nil, err
		}
		items = append(items, rankedValue{code: row[codeField], label: firstNonEmpty(row[nameField], row[codeField]), value: amount, row: row})
	}
	sort.Slice(items, func(i, j int) bool {
		if compared := items[i].value.Cmp(items[j].value); compared != 0 {
			return compared > 0
		}
		return items[i].code < items[j].code
	})
	if len(items) > dashboardTopLimit {
		items = items[:dashboardTopLimit]
	}
	return items, nil
}

func movementTotals(rows []map[string]string) (*big.Rat, *big.Rat, error) {
	debit, credit := new(big.Rat), new(big.Rat)
	for _, row := range rows {
		amount, err := decimal(row["amount"])
		if err != nil {
			return nil, nil, err
		}
		if row["doc_sort"] == "2" || row["doc_sort"] == "3" {
			credit.Add(credit, amount)
		} else {
			debit.Add(debit, amount)
		}
	}
	return debit, credit, nil
}

func countTrue(rows []map[string]string, field string) int {
	count := 0
	for _, row := range rows {
		if row[field] == "true" || row[field] == "1" {
			count++
		}
	}
	return count
}

func divideDecimal(totalValue, countValue string, unit MetricUnit) (string, error) {
	total, err := decimal(totalValue)
	if err != nil {
		return "", err
	}
	count, err := decimal(countValue)
	if err != nil {
		return "", err
	}
	if count.Sign() == 0 {
		return formatUnit(new(big.Rat), unit), nil
	}
	return formatUnit(new(big.Rat).Quo(total, count), unit), nil
}

func formatUnit(value *big.Rat, unit MetricUnit) string {
	switch unit {
	case UnitCount:
		return value.FloatString(0)
	case UnitQuantity:
		return value.FloatString(4)
	default:
		return value.FloatString(2)
	}
}

func formatBucket(values []*big.Rat, index int) string {
	if index < 0 || index >= len(values) || values[index] == nil {
		return "0.00"
	}
	return values[index].FloatString(2)
}

func labelAt(labels []string, index int) string {
	if index < 0 || index >= len(labels) {
		return ""
	}
	return labels[index]
}

func stringValue(value any) string {
	if value == nil {
		return "0"
	}
	return fmt.Sprint(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "ไม่ระบุ"
}

func compactVisualizations(items ...DashboardVisualization) []DashboardVisualization {
	result := make([]DashboardVisualization, 0, len(items))
	for _, item := range items {
		if item.Key != "" && len(item.Categories) > 0 && len(item.Series) > 0 {
			result = append(result, item)
		}
	}
	return result
}
