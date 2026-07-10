package report

import (
	"errors"
	"time"
)

type Preset string

const (
	Yesterday   Preset = "YESTERDAY"
	TodayToNow  Preset = "TODAY_TO_NOW"
	MonthToDate Preset = "MONTH_TO_DATE"
	AsOfRun     Preset = "AS_OF_RUN"
	Custom      Preset = "CUSTOM"
)

type Period struct {
	Preset   Preset `json:"preset"`
	DateFrom string `json:"dateFrom"`
	DateTo   string `json:"dateTo"`
}

func ResolvePeriod(preset Preset, location *time.Location, runAt time.Time, customFrom, customTo *string) (Period, error) {
	if location == nil {
		return Period{}, errors.New("report timezone is required")
	}
	localRun := runAt.In(location)
	localDay := time.Date(localRun.Year(), localRun.Month(), localRun.Day(), 0, 0, 0, 0, location)
	var from, to time.Time
	switch preset {
	case Yesterday:
		from = localDay.AddDate(0, 0, -1)
		to = from
	case TodayToNow, AsOfRun:
		from, to = localDay, localDay
	case MonthToDate:
		from = time.Date(localRun.Year(), localRun.Month(), 1, 0, 0, 0, 0, location)
		to = localDay
	case Custom:
		if customFrom == nil || customTo == nil {
			return Period{}, errors.New("custom report period requires dateFrom and dateTo")
		}
		var err error
		from, err = time.ParseInLocation(time.DateOnly, *customFrom, location)
		if err != nil {
			return Period{}, errors.New("custom report dateFrom is invalid")
		}
		to, err = time.ParseInLocation(time.DateOnly, *customTo, location)
		if err != nil {
			return Period{}, errors.New("custom report dateTo is invalid")
		}
		if to.Before(from) {
			return Period{}, errors.New("custom report dateTo must not precede dateFrom")
		}
		if int(to.Sub(from).Hours()/24)+1 > 366 {
			return Period{}, errors.New("custom report range exceeds 366 days")
		}
	default:
		return Period{}, errors.New("report period preset is invalid")
	}
	return Period{Preset: preset, DateFrom: from.Format(time.DateOnly), DateTo: to.Format(time.DateOnly)}, nil
}
