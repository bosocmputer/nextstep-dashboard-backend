package schedule

import (
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

// PeriodPolicy gates producer-side rollout while consumers remain compatible
// with both legacy uniform periods and smart per-report periods.
type PeriodPolicy struct {
	enabled   bool
	tenantIDs map[uuid.UUID]struct{}
	observer  PeriodResolutionObserver
}

type PeriodResolutionObserver func(preset report.Preset, mode report.ParameterKind, result string)

func NewPeriodPolicy(enabled bool, tenantIDs []uuid.UUID, observers ...PeriodResolutionObserver) PeriodPolicy {
	allowed := make(map[uuid.UUID]struct{}, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		if tenantID != uuid.Nil {
			allowed[tenantID] = struct{}{}
		}
	}
	var observer PeriodResolutionObserver
	if len(observers) > 0 {
		observer = observers[0]
	}
	return PeriodPolicy{enabled: enabled, tenantIDs: allowed, observer: observer}
}

func (policy PeriodPolicy) EnabledFor(tenantID uuid.UUID) bool {
	if !policy.enabled {
		return false
	}
	if len(policy.tenantIDs) == 0 {
		return true
	}
	_, ok := policy.tenantIDs[tenantID]
	return ok
}

func ResolveEffectivePeriod(preset report.Preset, mode report.ParameterKind, location *time.Location, runAt time.Time) (report.Period, error) {
	effectivePreset := preset
	switch mode {
	case report.DateRange:
		if preset == report.AsOfRun {
			effectivePreset = report.TodayToNow
		}
	case report.AsOfDate:
		if preset != report.Yesterday {
			effectivePreset = report.AsOfRun
		}
	case report.CurrentOnly:
		effectivePreset = report.AsOfRun
	default:
		return report.Period{}, &ValidationError{Field: "periodMode", Code: "INVALID_PERIOD_MODE"}
	}
	return report.ResolvePeriod(effectivePreset, location, runAt, nil, nil)
}

func (policy PeriodPolicy) Resolve(tenantID uuid.UUID, preset report.Preset, mode report.ParameterKind, location *time.Location, runAt time.Time) (period report.Period, err error) {
	result := "LEGACY"
	defer func() {
		if err != nil {
			result = "ERROR"
		}
		if policy.observer != nil {
			policy.observer(preset, mode, result)
		}
	}()
	if !policy.EnabledFor(tenantID) {
		return report.ResolvePeriod(preset, location, runAt, nil, nil)
	}
	result = "SMART"
	return ResolveEffectivePeriod(preset, mode, location, runAt)
}
