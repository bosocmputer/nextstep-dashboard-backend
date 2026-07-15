package viewer

import (
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

var ErrReportInputInvalid = errors.New("viewer report input is invalid")
var ErrViewerContextChanged = errors.New("viewer report context changed")

type ReportAccessControl interface {
	ListTenants(context.Context, uuid.UUID) ([]TenantAccess, error)
	CanAccessReport(context.Context, uuid.UUID, uuid.UUID, report.Key) (bool, error)
}

type ViewerRunStore interface {
	Enqueue(context.Context, report.EnqueueInput, time.Time) (report.Run, error)
	Get(context.Context, uuid.UUID, time.Time) (report.Run, error)
	CanAccessScheduledRun(context.Context, uuid.UUID, uuid.UUID) (bool, error)
	GetDashboard(context.Context, uuid.UUID, uuid.UUID) (report.Dashboard, error)
	ListLatestDashboards(context.Context, uuid.UUID, []report.Key) ([]DashboardSnapshot, error)
	CreateDashboardRefresh(context.Context, uuid.UUID, uuid.UUID, string, []report.EnqueueInput, time.Time) (DashboardRefresh, error)
	GetDashboardRefresh(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) (DashboardRefresh, error)
	GetDashboardRefreshResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (DashboardRefreshResult, error)
	ListRows(context.Context, uuid.UUID, int, int, time.Time) (report.RowsPage, error)
	Cancel(context.Context, uuid.UUID, time.Time) (report.Run, error)
}

type ViewerSnapshotStore interface {
	RevalidateSnapshot(context.Context, uuid.UUID, report.Key, report.Period, time.Time) (ReportRevalidation, error)
	GetExactSnapshotForPeriod(context.Context, uuid.UUID, report.Key, report.Period, time.Time) (DashboardSnapshot, error)
	GetExactSnapshotsForPeriods(context.Context, uuid.UUID, []SnapshotPeriodRequest, time.Time) (map[report.Key]DashboardSnapshot, error)
}

type ViewerGenerationStore interface {
	GetLatestPublishedOverview(context.Context, uuid.UUID, []report.Key, time.Time) (ExecutiveOverview, error)
	GetPublishedOverviewForPeriods(context.Context, uuid.UUID, []SnapshotPeriodRequest, time.Time) (ExecutiveOverview, error)
}

func (service *ReportService) ExactSnapshot(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, input CreateReportRunInput) (DashboardSnapshot, error) {
	definition, ok := report.DefinitionFor(reportKey)
	if !ok {
		return DashboardSnapshot{}, ErrReportInputInvalid
	}
	now := service.now().UTC()
	timezone, err := service.authorizedTimezone(ctx, recipientID, tenantID, reportKey)
	if err != nil {
		return DashboardSnapshot{}, err
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return DashboardSnapshot{}, ErrReportInputInvalid
	}
	period, err := report.ResolvePeriod(input.PeriodPreset, location, now, input.DateFrom, input.DateTo)
	if err != nil || !validViewerPeriod(definition.ParameterKind, input.PeriodPreset, period) || periodAfterToday(period, location, now) {
		return DashboardSnapshot{}, ErrReportInputInvalid
	}
	store, ok := service.store.(ViewerSnapshotStore)
	if !ok {
		return DashboardSnapshot{}, errors.New("viewer snapshot store is unavailable")
	}
	return store.GetExactSnapshotForPeriod(ctx, tenantID, reportKey, period, now)
}

type CreateReportRunInput struct {
	PeriodPreset report.Preset
	DateFrom     *string
	DateTo       *string
}

type ReportRows struct {
	RunID      uuid.UUID
	Columns    []string
	Rows       []map[string]string
	NextCursor string
	HasMore    bool
}

type ReportService struct {
	access                   ReportAccessControl
	store                    ViewerRunStore
	now                      func() time.Time
	snapshotFirstEnabled     bool
	staleRevalidationEnabled bool
	snapshotFirstTenants     map[uuid.UUID]struct{}
}

func NewReportService(access ReportAccessControl, store ViewerRunStore, now func() time.Time) *ReportService {
	return &ReportService{access: access, store: store, now: now, snapshotFirstEnabled: true, staleRevalidationEnabled: true}
}

func (service *ReportService) ConfigureStaleRevalidation(enabled bool) *ReportService {
	service.staleRevalidationEnabled = enabled
	return service
}

func (service *ReportService) ConfigureSnapshotFirst(enabled bool, tenantIDs []uuid.UUID) *ReportService {
	service.snapshotFirstEnabled = enabled
	service.snapshotFirstTenants = make(map[uuid.UUID]struct{}, len(tenantIDs))
	for _, tenantID := range tenantIDs {
		service.snapshotFirstTenants[tenantID] = struct{}{}
	}
	return service
}

func (service *ReportService) snapshotFirstAllowed(tenantID uuid.UUID) bool {
	if !service.snapshotFirstEnabled {
		return false
	}
	if len(service.snapshotFirstTenants) == 0 {
		return true
	}
	_, ok := service.snapshotFirstTenants[tenantID]
	return ok
}

func (service *ReportService) Create(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, idempotencyKey string, input CreateReportRunInput) (report.Run, error) {
	definition, ok := report.DefinitionFor(reportKey)
	if !ok || len(idempotencyKey) < 8 || len(idempotencyKey) > 200 {
		return report.Run{}, ErrReportInputInvalid
	}
	if input.PeriodPreset != report.Custom && (input.DateFrom != nil || input.DateTo != nil) {
		return report.Run{}, ErrReportInputInvalid
	}
	now := service.now().UTC()
	timezone, err := service.authorizedTimezone(ctx, recipientID, tenantID, reportKey)
	if err != nil {
		return report.Run{}, err
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return report.Run{}, ErrReportInputInvalid
	}
	period, err := report.ResolvePeriod(input.PeriodPreset, location, now, input.DateFrom, input.DateTo)
	if err != nil {
		return report.Run{}, ErrReportInputInvalid
	}
	if !validViewerPeriod(definition.ParameterKind, input.PeriodPreset, period) || periodAfterToday(period, location, now) {
		return report.Run{}, ErrReportInputInvalid
	}
	return service.store.Enqueue(ctx, report.EnqueueInput{
		TenantID: tenantID, ReportKey: reportKey, Source: report.SourceDashboard,
		IdempotencyKey: idempotencyKey, Period: period, RequestedByRecipient: &recipientID,
	}, now)
}

func (service *ReportService) Get(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, runID uuid.UUID) (report.Run, error) {
	if err := service.authorizeReport(ctx, recipientID, tenantID, reportKey); err != nil {
		return report.Run{}, err
	}
	run, err := service.store.Get(ctx, runID, service.now().UTC())
	if err != nil {
		return report.Run{}, err
	}
	if !runMatchesRoute(run, tenantID, reportKey) {
		return report.Run{}, ErrReportForbidden
	}
	if run.Source == report.SourceSchedule {
		if run.ResultKind != report.ResultSummary {
			allowed, accessErr := service.store.CanAccessScheduledRun(ctx, recipientID, runID)
			if accessErr != nil {
				return report.Run{}, accessErr
			}
			if !allowed {
				return report.Run{}, ErrReportForbidden
			}
		}
	} else if run.Source == report.SourceBackground && run.ResultKind == report.ResultSummary {
		// Background summaries are tenant scoped; the current report permission above is the authorization boundary.
	} else if run.Source != report.SourceDashboard || run.RequestedByRecipient == nil || *run.RequestedByRecipient != recipientID {
		return report.Run{}, ErrReportForbidden
	}
	return run, nil
}

func (service *ReportService) Revalidate(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, input CreateReportRunInput) (ReportRevalidation, error) {
	if !service.snapshotFirstAllowed(tenantID) {
		if err := service.authorizeReport(ctx, recipientID, tenantID, reportKey); err != nil {
			return ReportRevalidation{}, err
		}
		return ReportRevalidation{Disposition: RevalidationDisabled, LegacyFallback: true}, nil
	}
	definition, ok := report.DefinitionFor(reportKey)
	if !ok {
		return ReportRevalidation{}, ErrReportInputInvalid
	}
	now := service.now().UTC()
	timezone, err := service.authorizedTimezone(ctx, recipientID, tenantID, reportKey)
	if err != nil {
		return ReportRevalidation{}, err
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return ReportRevalidation{}, ErrReportInputInvalid
	}
	period, err := report.ResolvePeriod(input.PeriodPreset, location, now, input.DateFrom, input.DateTo)
	if err != nil || !validViewerPeriod(definition.ParameterKind, input.PeriodPreset, period) || periodAfterToday(period, location, now) {
		return ReportRevalidation{}, ErrReportInputInvalid
	}
	store, ok := service.store.(ViewerSnapshotStore)
	if !ok {
		return ReportRevalidation{}, errors.New("viewer snapshot store is unavailable")
	}
	if !service.staleRevalidationEnabled {
		snapshot, snapshotErr := store.GetExactSnapshotForPeriod(ctx, tenantID, reportKey, period, now)
		if snapshotErr != nil && !errors.Is(snapshotErr, report.ErrRunNotFound) {
			return ReportRevalidation{}, snapshotErr
		}
		result := ReportRevalidation{Disposition: RevalidationDisabled}
		if snapshotErr == nil {
			result.Snapshot = &snapshot
		}
		return result, nil
	}
	return store.RevalidateSnapshot(ctx, tenantID, reportKey, period, now)
}

// ExactOverview resolves only snapshots that already exist for the requested
// periods. It never enqueues or joins SML work, so navigation and cache lookup
// remain read-only operations.
func (service *ReportService) ExactOverview(ctx context.Context, recipientID, tenantID uuid.UUID, input DashboardRefreshInput) (ExecutiveOverview, error) {
	tenant, err := service.authorizedTenant(ctx, recipientID, tenantID)
	if err != nil {
		return ExecutiveOverview{}, err
	}
	if !sameReportKeys(input.ReportKeys, tenant.ReportKeys) {
		return ExecutiveOverview{}, ErrViewerContextChanged
	}
	location, err := time.LoadLocation(tenant.Timezone)
	if err != nil {
		return ExecutiveOverview{}, ErrReportInputInvalid
	}
	now := service.now().UTC()
	selectedPeriod, err := report.ResolvePeriod(input.PeriodPreset, location, now, input.DateFrom, input.DateTo)
	if err != nil || periodAfterToday(selectedPeriod, location, now) {
		return ExecutiveOverview{}, ErrReportInputInvalid
	}
	store, ok := service.store.(ViewerSnapshotStore)
	if !ok {
		return ExecutiveOverview{}, errors.New("viewer snapshot store is unavailable")
	}
	periods := make([]SnapshotPeriodRequest, 0, len(tenant.ReportKeys))
	for _, key := range tenant.ReportKeys {
		definition, found := report.DefinitionFor(key)
		if !found {
			return ExecutiveOverview{}, ErrReportForbidden
		}
		period, resolveErr := dashboardRefreshPeriod(definition.ParameterKind, selectedPeriod, false, location, now)
		if resolveErr != nil {
			return ExecutiveOverview{}, ErrReportInputInvalid
		}
		periods = append(periods, SnapshotPeriodRequest{ReportKey: key, Period: period})
	}
	if generationStore, ok := service.store.(ViewerGenerationStore); ok {
		generation, generationErr := generationStore.GetPublishedOverviewForPeriods(ctx, tenantID, periods, now)
		if generationErr == nil {
			generation.Timezone = tenant.Timezone
			return generation, nil
		}
		if !errors.Is(generationErr, report.ErrRunNotFound) {
			return ExecutiveOverview{}, generationErr
		}
	}
	exactSnapshots, err := store.GetExactSnapshotsForPeriods(ctx, tenantID, periods, now)
	if err != nil {
		return ExecutiveOverview{}, err
	}
	overview := ExecutiveOverview{TenantID: tenantID, Timezone: tenant.Timezone}
	for _, requested := range periods {
		if snapshot, found := exactSnapshots[requested.ReportKey]; found && snapshot.FreshnessStatus != FreshnessExpired {
			overview.Items = append(overview.Items, snapshot)
		}
	}
	return overview, nil
}

func (service *ReportService) RevalidateOverview(ctx context.Context, recipientID, tenantID uuid.UUID, input DashboardRefreshInput) (OverviewRevalidation, error) {
	if !service.snapshotFirstAllowed(tenantID) {
		overview, err := service.ExecutiveOverview(ctx, recipientID, tenantID)
		return OverviewRevalidation{Disposition: RevalidationDisabled, Overview: overview, LegacyFallback: true}, err
	}
	if !service.staleRevalidationEnabled {
		overview, err := service.ExactOverview(ctx, recipientID, tenantID, input)
		return OverviewRevalidation{Disposition: RevalidationDisabled, Overview: overview}, err
	}
	tenant, err := service.authorizedTenant(ctx, recipientID, tenantID)
	if err != nil {
		return OverviewRevalidation{}, err
	}
	if !sameReportKeys(input.ReportKeys, tenant.ReportKeys) {
		return OverviewRevalidation{}, ErrViewerContextChanged
	}
	location, err := time.LoadLocation(tenant.Timezone)
	if err != nil {
		return OverviewRevalidation{}, ErrReportInputInvalid
	}
	now := service.now().UTC()
	selectedPeriod, err := report.ResolvePeriod(input.PeriodPreset, location, now, input.DateFrom, input.DateTo)
	if err != nil || periodAfterToday(selectedPeriod, location, now) {
		return OverviewRevalidation{}, ErrReportInputInvalid
	}
	store, ok := service.store.(ViewerSnapshotStore)
	if !ok {
		return OverviewRevalidation{}, errors.New("viewer snapshot store is unavailable")
	}
	periods := make([]SnapshotPeriodRequest, 0, len(tenant.ReportKeys))
	for _, key := range tenant.ReportKeys {
		definition, found := report.DefinitionFor(key)
		if !found {
			return OverviewRevalidation{}, ErrReportForbidden
		}
		period, resolveErr := dashboardRefreshPeriod(definition.ParameterKind, selectedPeriod, false, location, now)
		if resolveErr != nil {
			return OverviewRevalidation{}, ErrReportInputInvalid
		}
		periods = append(periods, SnapshotPeriodRequest{ReportKey: key, Period: period})
	}
	exactSnapshots, err := store.GetExactSnapshotsForPeriods(ctx, tenantID, periods, now)
	if err != nil {
		return OverviewRevalidation{}, err
	}
	result := OverviewRevalidation{Disposition: RevalidationFreshCache, Overview: ExecutiveOverview{TenantID: tenantID, Timezone: tenant.Timezone}}
	for _, requested := range periods {
		if snapshot, found := exactSnapshots[requested.ReportKey]; found && snapshot.FreshnessStatus == FreshnessFresh {
			result.Overview.Items = append(result.Overview.Items, snapshot)
			continue
		}
		item, revalidateErr := store.RevalidateSnapshot(ctx, tenantID, requested.ReportKey, requested.Period, now)
		if revalidateErr != nil {
			return OverviewRevalidation{}, revalidateErr
		}
		if item.Snapshot != nil && item.Snapshot.FreshnessStatus != FreshnessExpired {
			result.Overview.Items = append(result.Overview.Items, *item.Snapshot)
		}
		if item.Run != nil {
			result.Runs = append(result.Runs, *item.Run)
		}
		if item.RetryAfter > result.RetryAfter {
			result.RetryAfter = item.RetryAfter
		}
		switch item.Disposition {
		case RevalidationCircuitOpen:
			result.Disposition = RevalidationCircuitOpen
		case RevalidationMissingRefreshing:
			if result.Disposition != RevalidationCircuitOpen {
				result.Disposition = RevalidationMissingRefreshing
			}
		case RevalidationStaleRefreshing, RevalidationJoined:
			if result.Disposition == RevalidationFreshCache {
				result.Disposition = item.Disposition
			}
		case RevalidationDisabled:
			if result.Disposition == RevalidationFreshCache {
				result.Disposition = RevalidationDisabled
			}
		}
	}
	return result, nil
}

func (service *ReportService) ListRows(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, runID uuid.UUID, cursor string, pageSize int) (ReportRows, error) {
	if pageSize == 0 {
		pageSize = 25
	}
	if pageSize < 1 || pageSize > 100 {
		return ReportRows{}, ErrReportInputInvalid
	}
	if _, err := service.Get(ctx, recipientID, tenantID, reportKey, runID); err != nil {
		return ReportRows{}, err
	}
	afterOrdinal, err := decodeReportCursor(cursor)
	if err != nil {
		return ReportRows{}, ErrReportInputInvalid
	}
	page, err := service.store.ListRows(ctx, runID, afterOrdinal, pageSize, service.now().UTC())
	if err != nil {
		return ReportRows{}, err
	}
	columns := make(map[string]struct{})
	for _, row := range page.Rows {
		for column := range row {
			columns[column] = struct{}{}
		}
	}
	orderedColumns := make([]string, 0, len(columns))
	for column := range columns {
		orderedColumns = append(orderedColumns, column)
	}
	sort.Strings(orderedColumns)
	nextCursor := ""
	if page.HasMore {
		nextCursor = encodeReportCursor(page.NextOrdinal)
	}
	return ReportRows{RunID: runID, Columns: orderedColumns, Rows: page.Rows, NextCursor: nextCursor, HasMore: page.HasMore}, nil
}

func (service *ReportService) GetDashboard(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, runID uuid.UUID) (report.Dashboard, error) {
	if _, err := service.Get(ctx, recipientID, tenantID, reportKey, runID); err != nil {
		return report.Dashboard{}, err
	}
	dashboard, err := service.store.GetDashboard(ctx, tenantID, runID)
	if err != nil {
		return report.Dashboard{}, err
	}
	if dashboard.ReportKey != reportKey {
		return report.Dashboard{}, ErrReportForbidden
	}
	return dashboard, nil
}

func (service *ReportService) ExecutiveOverview(ctx context.Context, recipientID, tenantID uuid.UUID) (ExecutiveOverview, error) {
	tenant, err := service.authorizedTenant(ctx, recipientID, tenantID)
	if err != nil {
		return ExecutiveOverview{}, err
	}
	if generationStore, ok := service.store.(ViewerGenerationStore); ok {
		generation, generationErr := generationStore.GetLatestPublishedOverview(ctx, tenantID, tenant.ReportKeys, service.now().UTC())
		if generationErr == nil {
			generation.Timezone = tenant.Timezone
			return generation, nil
		}
		if !errors.Is(generationErr, report.ErrRunNotFound) {
			return ExecutiveOverview{}, generationErr
		}
	}
	items, err := service.store.ListLatestDashboards(ctx, tenantID, tenant.ReportKeys)
	if err != nil {
		return ExecutiveOverview{}, err
	}
	allowed := make(map[report.Key]struct{}, len(tenant.ReportKeys))
	for _, key := range tenant.ReportKeys {
		allowed[key] = struct{}{}
	}
	for _, item := range items {
		if _, ok := allowed[item.Dashboard.ReportKey]; !ok {
			return ExecutiveOverview{}, ErrReportForbidden
		}
	}
	return ExecutiveOverview{TenantID: tenantID, Timezone: tenant.Timezone, Items: items}, nil
}

func (service *ReportService) CreateDashboardRefresh(ctx context.Context, recipientID, tenantID uuid.UUID, idempotencyKey string, input *DashboardRefreshInput) (DashboardRefresh, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 {
		return DashboardRefresh{}, ErrReportInputInvalid
	}
	tenant, err := service.authorizedTenant(ctx, recipientID, tenantID)
	if err != nil {
		return DashboardRefresh{}, err
	}
	location, err := time.LoadLocation(tenant.Timezone)
	if err != nil {
		return DashboardRefresh{}, ErrReportInputInvalid
	}
	now := service.now().UTC()
	legacy := input == nil
	if input == nil {
		input = &DashboardRefreshInput{PeriodPreset: report.MonthToDate, ReportKeys: append([]report.Key(nil), tenant.ReportKeys...)}
	}
	if len(input.ReportKeys) == 0 || input.PeriodPreset != report.Custom && (input.DateFrom != nil || input.DateTo != nil) {
		return DashboardRefresh{}, ErrReportInputInvalid
	}
	selectedPeriod, err := report.ResolvePeriod(input.PeriodPreset, location, now, input.DateFrom, input.DateTo)
	if err != nil || periodAfterToday(selectedPeriod, location, now) {
		return DashboardRefresh{}, ErrReportInputInvalid
	}
	if !sameReportKeys(input.ReportKeys, tenant.ReportKeys) {
		return DashboardRefresh{}, ErrViewerContextChanged
	}
	inputs := make([]report.EnqueueInput, 0, len(tenant.ReportKeys))
	for _, key := range tenant.ReportKeys {
		definition, ok := report.DefinitionFor(key)
		if !ok {
			return DashboardRefresh{}, ErrReportForbidden
		}
		period, resolveErr := dashboardRefreshPeriod(definition.ParameterKind, selectedPeriod, legacy, location, now)
		if resolveErr != nil {
			return DashboardRefresh{}, ErrReportInputInvalid
		}
		requestedBy := recipientID
		inputs = append(inputs, report.EnqueueInput{
			TenantID: tenantID, ReportKey: key, Source: report.SourceDashboard,
			Period: period, RequestedByRecipient: &requestedBy,
		})
	}
	return service.store.CreateDashboardRefresh(ctx, recipientID, tenantID, idempotencyKey, inputs, now)
}

func validViewerPeriod(kind report.ParameterKind, preset report.Preset, period report.Period) bool {
	switch kind {
	case report.DateRange:
		return preset == report.Yesterday || preset == report.TodayToNow || preset == report.MonthToDate || preset == report.Custom
	case report.AsOfDate:
		return (preset == report.AsOfRun || preset == report.Custom) && period.DateFrom == period.DateTo
	case report.CurrentOnly:
		return preset == report.AsOfRun && period.DateFrom == period.DateTo
	default:
		return false
	}
}

func periodAfterToday(period report.Period, location *time.Location, now time.Time) bool {
	today := now.In(location).Format(time.DateOnly)
	return period.DateFrom > today || period.DateTo > today
}

func sameReportKeys(left, right []report.Key) bool {
	if len(left) == 0 || len(left) != len(right) {
		return false
	}
	counts := make(map[report.Key]int, len(left))
	for _, key := range left {
		if _, ok := report.DefinitionFor(key); !ok {
			return false
		}
		counts[key]++
	}
	for _, key := range right {
		counts[key]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func dashboardRefreshPeriod(kind report.ParameterKind, selected report.Period, legacy bool, location *time.Location, now time.Time) (report.Period, error) {
	switch kind {
	case report.DateRange:
		return selected, nil
	case report.AsOfDate:
		if legacy || selected.DateTo == now.In(location).Format(time.DateOnly) {
			return report.ResolvePeriod(report.AsOfRun, location, now, nil, nil)
		}
		date := selected.DateTo
		return report.ResolvePeriod(report.Custom, location, now, &date, &date)
	case report.CurrentOnly:
		return report.ResolvePeriod(report.AsOfRun, location, now, nil, nil)
	default:
		return report.Period{}, ErrReportInputInvalid
	}
}

func (service *ReportService) GetDashboardRefresh(ctx context.Context, recipientID, tenantID, refreshID uuid.UUID) (DashboardRefresh, error) {
	if _, err := service.authorizedTenant(ctx, recipientID, tenantID); err != nil {
		return DashboardRefresh{}, err
	}
	return service.store.GetDashboardRefresh(ctx, recipientID, tenantID, refreshID, service.now().UTC())
}

func (service *ReportService) GetDashboardRefreshResult(ctx context.Context, recipientID, tenantID, refreshID uuid.UUID) (DashboardRefreshResult, error) {
	tenant, err := service.authorizedTenant(ctx, recipientID, tenantID)
	if err != nil {
		return DashboardRefreshResult{}, err
	}
	result, err := service.store.GetDashboardRefreshResult(ctx, recipientID, tenantID, refreshID)
	if err != nil {
		return DashboardRefreshResult{}, err
	}
	if result.TenantID != tenantID {
		return DashboardRefreshResult{}, ErrReportForbidden
	}
	allowed := make(map[report.Key]struct{}, len(tenant.ReportKeys))
	for _, key := range tenant.ReportKeys {
		allowed[key] = struct{}{}
	}
	for _, item := range result.Items {
		if _, ok := allowed[item.Dashboard.ReportKey]; !ok {
			return DashboardRefreshResult{}, ErrReportForbidden
		}
	}
	for _, failure := range result.Failures {
		if _, ok := allowed[failure.ReportKey]; !ok {
			return DashboardRefreshResult{}, ErrReportForbidden
		}
	}
	return result, nil
}

func (service *ReportService) Cancel(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, runID uuid.UUID) (report.Run, error) {
	run, err := service.Get(ctx, recipientID, tenantID, reportKey, runID)
	if err != nil {
		return report.Run{}, err
	}
	if run.Source != report.SourceDashboard || run.RequestedByRecipient == nil || *run.RequestedByRecipient != recipientID {
		return report.Run{}, ErrReportForbidden
	}
	return service.store.Cancel(ctx, runID, service.now().UTC())
}

func (service *ReportService) authorizedTimezone(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key) (string, error) {
	tenant, err := service.authorizedTenant(ctx, recipientID, tenantID)
	if err != nil {
		return "", err
	}
	if err := service.authorizeReport(ctx, recipientID, tenantID, reportKey); err != nil {
		return "", err
	}
	return tenant.Timezone, nil
}

func (service *ReportService) authorizedTenant(ctx context.Context, recipientID, tenantID uuid.UUID) (TenantAccess, error) {
	tenants, err := service.access.ListTenants(ctx, recipientID)
	if err != nil {
		return TenantAccess{}, err
	}
	for _, item := range tenants {
		if item.ID == tenantID {
			return item, nil
		}
	}
	return TenantAccess{}, ErrReportForbidden
}
func (service *ReportService) authorizeReport(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key) error {
	if _, ok := report.DefinitionFor(reportKey); !ok {
		return ErrReportForbidden
	}
	allowed, err := service.access.CanAccessReport(ctx, recipientID, tenantID, reportKey)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrReportForbidden
	}
	return nil
}

func runMatchesRoute(run report.Run, tenantID uuid.UUID, reportKey report.Key) bool {
	return run.TenantID == tenantID && run.ReportKey == reportKey
}

func encodeReportCursor(ordinal int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(ordinal)))
}

func decodeReportCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	if len(cursor) > 64 {
		return 0, ErrReportInputInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrReportInputInvalid
	}
	ordinal, err := strconv.Atoi(string(raw))
	if err != nil || ordinal < 0 {
		return 0, ErrReportInputInvalid
	}
	return ordinal, nil
}
