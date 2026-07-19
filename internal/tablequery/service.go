package tablequery

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/google/uuid"
)

var safeCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

type Service struct {
	store          Store
	recipientNames RecipientNameResolver
	lineReady      bool
	now            func() time.Time
}

func NewService(store Store, recipientNames RecipientNameResolver, lineReady bool, now func() time.Time) *Service {
	return &Service{store: store, recipientNames: recipientNames, lineReady: lineReady, now: now}
}

func (service *Service) QueryTenants(ctx context.Context, input TenantsInput) (TenantsResult, error) {
	if !validCommon(&input.CommonInput) || countFilters(len(input.Filters.Statuses), len(input.Filters.SMLReadiness)) > 8 || !validTenantFilters(input.Filters) {
		return TenantsResult{}, ErrInvalidInput
	}
	items, total, err := service.store.QueryTenants(ctx, input, service.now().UTC())
	return TenantsResult{Data: items, PageMeta: NewPageMeta(input.Page, input.PageSize, total)}, err
}

func (service *Service) QuerySchedules(ctx context.Context, tenantID uuid.UUID, input SchedulesInput) (SchedulesResult, error) {
	if tenantID == uuid.Nil || !validCommon(&input.CommonInput) || len(input.Filters.Statuses) > 5 || !validScheduleStatuses(input.Filters.Statuses) {
		return SchedulesResult{}, ErrInvalidInput
	}
	items, total, err := service.store.QuerySchedules(ctx, tenantID, input, service.now().UTC())
	if err != nil {
		return SchedulesResult{}, err
	}
	for index := range items {
		if items[index].Status == schedule.StatusArchived {
			continue
		}
		if !service.lineReady {
			items[index].ReadinessBlockers = appendUnique(items[index].ReadinessBlockers, schedule.BlockerLineNotConfigured)
		}
		items[index].NextOccurrences, err = schedule.NextOccurrences(items[index].Input, service.now().UTC(), 3)
		if err != nil {
			return SchedulesResult{}, err
		}
	}
	return SchedulesResult{Data: items, PageMeta: NewPageMeta(input.Page, input.PageSize, total)}, nil
}

func (service *Service) QueryReportRuns(ctx context.Context, input ReportRunsInput) (ReportRunsResult, error) {
	if !validCommon(&input.CommonInput) || !validDateRange(input.Filters.DateFrom, input.Filters.DateTo) || countFilters(boolInt(input.Filters.TenantID != nil), len(input.Filters.Statuses), len(input.Filters.ReportKeys), len(input.Filters.Sources), boolInt(input.Filters.DateFrom != "" || input.Filters.DateTo != "")) > 8 || !validReportRunFilters(input.Filters) {
		return ReportRunsResult{}, ErrInvalidInput
	}
	items, total, err := service.store.QueryReportRuns(ctx, input, service.now().UTC())
	if err != nil {
		return ReportRunsResult{}, err
	}
	for index := range items {
		items[index].FailureSummary = operations.SummarizeFailure(items[index].Run)
	}
	return ReportRunsResult{Data: items, PageMeta: NewPageMeta(input.Page, input.PageSize, total)}, nil
}

func (service *Service) QueryDeliveries(ctx context.Context, input DeliveriesInput) (DeliveriesResult, error) {
	if !validCommon(&input.CommonInput) || !validDateRange(input.Filters.DateFrom, input.Filters.DateTo) || countFilters(boolInt(input.Filters.TenantID != nil), boolInt(input.Filters.RecipientID != nil), len(input.Filters.Statuses), len(input.Filters.ReportKeys), boolInt(input.Filters.DateFrom != "" || input.Filters.DateTo != "")) > 8 || !validDeliveryFilters(input.Filters) {
		return DeliveriesResult{}, ErrInvalidInput
	}
	items, total, err := service.store.QueryDeliveries(ctx, input)
	if err != nil {
		return DeliveriesResult{}, err
	}
	for index := range items {
		name, resolveErr := service.recipientNames.DisplayName(items[index].StoredRecipient)
		if resolveErr != nil {
			return DeliveriesResult{}, fmt.Errorf("resolve delivery recipient name: %w", resolveErr)
		}
		items[index].RecipientName = name
	}
	return DeliveriesResult{Data: items, PageMeta: NewPageMeta(input.Page, input.PageSize, total)}, nil
}

func (service *Service) QueryAudit(ctx context.Context, input AuditInput) (AuditResult, error) {
	if !validCommon(&input.CommonInput) || !validDateRange(input.Filters.DateFrom, input.Filters.DateTo) || countFilters(boolInt(input.Filters.TenantID != nil), len(input.Filters.ActorTypes), len(input.Filters.Actions), len(input.Filters.Results), boolInt(input.Filters.DateFrom != "" || input.Filters.DateTo != "")) > 8 || !validAuditFilters(input.Filters) {
		return AuditResult{}, ErrInvalidInput
	}
	items, total, err := service.store.QueryAudit(ctx, input)
	return AuditResult{Data: items, PageMeta: NewPageMeta(input.Page, input.PageSize, total)}, err
}

func (service *Service) QueryIncidents(ctx context.Context, input IncidentsInput) (IncidentsResult, error) {
	if !validCommon(&input.CommonInput) || countFilters(len(input.Filters.Statuses), len(input.Filters.Severities), len(input.Filters.RootCauses), boolInt(input.Filters.ActiveOnly)) > 8 || !validIncidentFilters(input.Filters) {
		return IncidentsResult{}, ErrInvalidInput
	}
	items, total, err := service.store.QueryIncidents(ctx, input)
	if err != nil {
		return IncidentsResult{}, err
	}
	for index := range items {
		items[index] = sentinel.PresentIncident(items[index])
	}
	return IncidentsResult{Data: items, PageMeta: NewPageMeta(input.Page, input.PageSize, total)}, nil
}

func (service *Service) QueryOccurrences(ctx context.Context, incidentID uuid.UUID, input OccurrencesInput) (OccurrencesResult, error) {
	if incidentID == uuid.Nil || !validCommon(&input.CommonInput) || !validDateRange(input.Filters.DateFrom, input.Filters.DateTo) || countFilters(boolInt(input.Filters.TenantID != nil), len(input.Filters.ReportKeys), len(input.Filters.SourceKinds), len(input.Filters.SafeErrorCodes), boolInt(input.Filters.DateFrom != "" || input.Filters.DateTo != "")) > 8 || !validOccurrenceFilters(input.Filters) {
		return OccurrencesResult{}, ErrInvalidInput
	}
	items, total, err := service.store.QueryOccurrences(ctx, incidentID, input)
	if err != nil {
		return OccurrencesResult{}, err
	}
	for index := range items {
		items[index].ConnectionReference = sentinel.SanitizeOccurrenceConnectionReference(items[index].ConnectionReference)
	}
	return OccurrencesResult{Data: items, PageMeta: NewPageMeta(input.Page, input.PageSize, total)}, nil
}

func validTenantFilters(filters TenantFilters) bool {
	for _, status := range filters.Statuses {
		switch status {
		case "ACTIVE", "DISABLED", "EXPIRED":
		default:
			return false
		}
	}
	for _, value := range filters.SMLReadiness {
		switch value {
		case "UNCONFIGURED", "READY", "FAILED":
		default:
			return false
		}
	}
	return len(filters.Statuses) <= 3 && len(filters.SMLReadiness) <= 3
}
func validScheduleStatuses(values []schedule.Status) bool {
	for _, value := range values {
		switch value {
		case schedule.StatusDraft, schedule.StatusActive, schedule.StatusPaused, schedule.StatusExpired, schedule.StatusArchived:
		default:
			return false
		}
	}
	return true
}
func validReportRunFilters(filters ReportRunFilters) bool {
	for _, value := range filters.Statuses {
		switch value {
		case report.StatusQueued, report.StatusClaimed, report.StatusRunning, report.StatusSucceeded, report.StatusFailed, report.StatusCancelled, report.StatusExpired:
		default:
			return false
		}
	}
	for _, key := range filters.ReportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return false
		}
	}
	for _, value := range filters.Sources {
		switch value {
		case report.SourceDashboard, report.SourceSchedule, report.SourceBackground:
		default:
			return false
		}
	}
	return len(filters.Statuses) <= 7 && len(filters.ReportKeys) <= 10 && len(filters.Sources) <= 3
}
func validDeliveryFilters(filters DeliveryFilters) bool {
	for _, value := range filters.Statuses {
		switch value {
		case "PENDING", "SENDING", "ACCEPTED", "RETRY_WAIT", "UNCERTAIN", "FAILED_PERMANENT":
		default:
			return false
		}
	}
	for _, key := range filters.ReportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return false
		}
	}
	return len(filters.Statuses) <= 6 && len(filters.ReportKeys) <= 10
}
func validAuditFilters(filters AuditFilters) bool {
	for _, value := range filters.ActorTypes {
		if value != "ADMIN" && value != "VIEWER" && value != "WORKER" && value != "SYSTEM" {
			return false
		}
	}
	for _, value := range filters.Results {
		if value != "SUCCESS" && value != "DENIED" && value != "FAILED" {
			return false
		}
	}
	for _, value := range filters.Actions {
		if !safeCodePattern.MatchString(value) {
			return false
		}
	}
	return len(filters.ActorTypes) <= 4 && len(filters.Actions) <= 8 && len(filters.Results) <= 3
}
func validIncidentFilters(filters IncidentFilters) bool {
	for _, value := range filters.Statuses {
		switch value {
		case sentinel.StatusOpen, sentinel.StatusAcknowledged, sentinel.StatusResolved, sentinel.StatusClosedAccepted:
		default:
			return false
		}
	}
	for _, value := range filters.Severities {
		if value != sentinel.SeverityP1 && value != sentinel.SeverityP2 {
			return false
		}
	}
	return len(filters.Statuses) <= 4 && len(filters.Severities) <= 2 && len(filters.RootCauses) <= 8
}
func validOccurrenceFilters(filters OccurrenceFilters) bool {
	for _, key := range filters.ReportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return false
		}
	}
	for _, code := range filters.SafeErrorCodes {
		if !safeCodePattern.MatchString(code) {
			return false
		}
	}
	return len(filters.ReportKeys) <= 10 && len(filters.SourceKinds) <= 8 && len(filters.SafeErrorCodes) <= 8
}
func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
