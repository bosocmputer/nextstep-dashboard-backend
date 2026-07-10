package report

import (
	"errors"
	"time"
)

// ResolveComparisonPeriod returns the business-equivalent period used by the
// executive dashboard. The returned preset is CUSTOM because the comparison
// range is already fully resolved and must not be interpreted again.
func ResolveComparisonPeriod(period Period) (Period, error) {
	from, err := time.Parse(time.DateOnly, period.DateFrom)
	if err != nil {
		return Period{}, errors.New("report comparison dateFrom is invalid")
	}
	to, err := time.Parse(time.DateOnly, period.DateTo)
	if err != nil {
		return Period{}, errors.New("report comparison dateTo is invalid")
	}
	if to.Before(from) {
		return Period{}, errors.New("report comparison dateTo precedes dateFrom")
	}

	var comparisonFrom, comparisonTo time.Time
	switch period.Preset {
	case MonthToDate:
		comparisonFrom = from.AddDate(0, -1, 0)
		priorMonthEnd := from.AddDate(0, 0, -1)
		targetDay := to.Day()
		if targetDay > priorMonthEnd.Day() {
			targetDay = priorMonthEnd.Day()
		}
		comparisonTo = time.Date(priorMonthEnd.Year(), priorMonthEnd.Month(), targetDay, 0, 0, 0, 0, time.UTC)
	case Yesterday, TodayToNow, AsOfRun:
		if !from.Equal(to) {
			return Period{}, errors.New("single-day report preset resolved to a date range")
		}
		comparisonFrom = from.AddDate(0, 0, -1)
		comparisonTo = comparisonFrom
	case Custom:
		days := int(to.Sub(from).Hours()/24) + 1
		comparisonTo = from.AddDate(0, 0, -1)
		comparisonFrom = comparisonTo.AddDate(0, 0, -(days - 1))
	default:
		return Period{}, errors.New("report comparison preset is invalid")
	}

	return Period{
		Preset:   Custom,
		DateFrom: comparisonFrom.Format(time.DateOnly),
		DateTo:   comparisonTo.Format(time.DateOnly),
	}, nil
}
