package report

import (
	"reflect"
	"testing"
	"time"
)

func TestCatalogKeepsStableTenReportContract(t *testing.T) {
	want := []Key{
		SalesGoodsServices,
		PurchaseGoodsPayables,
		GrossProfitByProduct,
		GrossProfitByARCustomer,
		StockBalance,
		StockReorder,
		ARCustomerMovement,
		ARDebtReceipt,
		CashBankReceipts,
		CashBankPayments,
	}
	if got := Keys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Keys() = %v, want %v", got, want)
	}
	for _, key := range want {
		definition, ok := DefinitionFor(key)
		if !ok {
			t.Errorf("missing definition for %s", key)
			continue
		}
		if definition.Version == "" || definition.LabelTH == "" || definition.CategoryLabelTH == "" || definition.Status != StatusActive || definition.MaxRows != 200_000 || definition.RefreshClass == "" || definition.MinimumRefreshInterval <= 0 {
			t.Errorf("incomplete definition for %s: %+v", key, definition)
		}
		if len(definition.LineMetrics) != 2 {
			t.Errorf("%s LINE metrics = %d, want 2", key, len(definition.LineMetrics))
		}
	}
	if _, ok := DefinitionFor(Key("unknown_report")); ok {
		t.Fatal("unknown report key was accepted")
	}
}

func TestReportDefinitionsExposeSafeRefreshClasses(t *testing.T) {
	want := map[Key]RefreshClass{
		SalesGoodsServices: RefreshFast, ARDebtReceipt: RefreshFast,
		CashBankReceipts: RefreshFast, CashBankPayments: RefreshFast,
		PurchaseGoodsPayables: RefreshStandard, GrossProfitByProduct: RefreshStandard,
		GrossProfitByARCustomer: RefreshStandard, ARCustomerMovement: RefreshStandard,
		StockReorder: RefreshStandard, StockBalance: RefreshHeavy,
	}
	for key, refreshClass := range want {
		definition, ok := DefinitionFor(key)
		if !ok || definition.RefreshClass != refreshClass {
			t.Fatalf("DefinitionFor(%s).RefreshClass = %q, want %q", key, definition.RefreshClass, refreshClass)
		}
	}
	if got := DefaultRefreshInterval(RefreshFast); got != 5*time.Minute {
		t.Fatalf("fast refresh = %s", got)
	}
	if got := DefaultRefreshInterval(RefreshStandard); got != 15*time.Minute {
		t.Fatalf("standard refresh = %s", got)
	}
	if got := DefaultRefreshInterval(RefreshHeavy); got != 30*time.Minute {
		t.Fatalf("heavy refresh = %s", got)
	}
}

func TestCanSelectKeepsDeprecatedDefinitionsOnlyWhenAlreadySelected(t *testing.T) {
	if !CanSelect(Definition{Status: StatusActive}, false) {
		t.Fatal("active report was not selectable")
	}
	deprecated := Definition{Status: StatusDeprecated}
	if CanSelect(deprecated, false) || !CanSelect(deprecated, true) {
		t.Fatal("deprecated report selection policy is invalid")
	}
}

func TestResolvePeriodUsesTenantLocalCalendar(t *testing.T) {
	runAt := time.Date(2026, 7, 10, 18, 30, 0, 0, time.UTC) // 2026-07-11 01:30 Bangkok
	location, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		preset Preset
		from   string
		to     string
	}{
		{preset: Yesterday, from: "2026-07-10", to: "2026-07-10"},
		{preset: TodayToNow, from: "2026-07-11", to: "2026-07-11"},
		{preset: MonthToDate, from: "2026-07-01", to: "2026-07-11"},
		{preset: AsOfRun, from: "2026-07-11", to: "2026-07-11"},
	} {
		period, err := ResolvePeriod(test.preset, location, runAt, nil, nil)
		if err != nil {
			t.Fatalf("ResolvePeriod(%s) error = %v", test.preset, err)
		}
		if period.DateFrom != test.from || period.DateTo != test.to {
			t.Errorf("ResolvePeriod(%s) = %+v", test.preset, period)
		}
	}
}

func TestResolveCustomPeriodRejectsMissingReverseAndOversizedRange(t *testing.T) {
	location := time.UTC
	runAt := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	from := "2026-07-11"
	to := "2026-07-10"
	if _, err := ResolvePeriod(Custom, location, runAt, &from, &to); err == nil {
		t.Fatal("reverse custom range was accepted")
	}
	from = "2024-01-01"
	to = "2026-07-10"
	if _, err := ResolvePeriod(Custom, location, runAt, &from, &to); err == nil {
		t.Fatal("oversized custom range was accepted")
	}
	if _, err := ResolvePeriod(Custom, location, runAt, nil, nil); err == nil {
		t.Fatal("missing custom range was accepted")
	}
}

func TestReportDefinitionsExposePeriodModes(t *testing.T) {
	want := map[Key]ParameterKind{
		SalesGoodsServices:      DateRange,
		PurchaseGoodsPayables:   DateRange,
		GrossProfitByProduct:    DateRange,
		GrossProfitByARCustomer: DateRange,
		StockBalance:            AsOfDate,
		StockReorder:            CurrentOnly,
		ARCustomerMovement:      AsOfDate,
		ARDebtReceipt:           DateRange,
		CashBankReceipts:        DateRange,
		CashBankPayments:        DateRange,
	}
	for key, periodMode := range want {
		definition, ok := DefinitionFor(key)
		if !ok || definition.ParameterKind != periodMode {
			t.Fatalf("DefinitionFor(%s).ParameterKind = %q, want %q", key, definition.ParameterKind, periodMode)
		}
	}
}
