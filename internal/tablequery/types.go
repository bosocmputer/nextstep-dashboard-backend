package tablequery

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/google/uuid"
)

var ErrInvalidInput = errors.New("table query input is invalid")

type CommonInput struct {
	Page         int    `json:"page"`
	PageSize     int    `json:"pageSize"`
	GlobalSearch string `json:"globalSearch,omitempty"`
}

type TenantFilters struct {
	Statuses     []tenant.Status `json:"statuses,omitempty"`
	SMLReadiness []string        `json:"smlReadiness,omitempty"`
}
type TenantsInput struct {
	CommonInput
	Filters TenantFilters `json:"filters"`
}

type ScheduleFilters struct {
	Statuses        []schedule.Status `json:"statuses,omitempty"`
	IncludeArchived bool              `json:"includeArchived,omitempty"`
}
type SchedulesInput struct {
	CommonInput
	Filters ScheduleFilters `json:"filters"`
}

type ReportRunFilters struct {
	TenantID   *uuid.UUID         `json:"tenantId,omitempty"`
	Statuses   []report.RunStatus `json:"statuses,omitempty"`
	ReportKeys []report.Key       `json:"reportKeys,omitempty"`
	Sources    []report.Source    `json:"sources,omitempty"`
	DateFrom   string             `json:"dateFrom,omitempty"`
	DateTo     string             `json:"dateTo,omitempty"`
}
type ReportRunsInput struct {
	CommonInput
	Filters ReportRunFilters `json:"filters"`
}

type DeliveryFilters struct {
	TenantID    *uuid.UUID                  `json:"tenantId,omitempty"`
	RecipientID *uuid.UUID                  `json:"recipientId,omitempty"`
	Statuses    []operations.DeliveryStatus `json:"statuses,omitempty"`
	ReportKeys  []report.Key                `json:"reportKeys,omitempty"`
	DateFrom    string                      `json:"dateFrom,omitempty"`
	DateTo      string                      `json:"dateTo,omitempty"`
}
type DeliveriesInput struct {
	CommonInput
	Filters DeliveryFilters `json:"filters"`
}

type AuditFilters struct {
	TenantID   *uuid.UUID `json:"tenantId,omitempty"`
	ActorTypes []string   `json:"actorTypes,omitempty"`
	Actions    []string   `json:"actions,omitempty"`
	Results    []string   `json:"results,omitempty"`
	DateFrom   string     `json:"dateFrom,omitempty"`
	DateTo     string     `json:"dateTo,omitempty"`
}
type AuditInput struct {
	CommonInput
	Filters AuditFilters `json:"filters"`
}

type IncidentFilters struct {
	Statuses   []sentinel.Status    `json:"statuses,omitempty"`
	Severities []sentinel.Severity  `json:"severities,omitempty"`
	RootCauses []sentinel.RootCause `json:"rootCauses,omitempty"`
	ActiveOnly bool                 `json:"activeOnly,omitempty"`
}
type IncidentsInput struct {
	CommonInput
	Filters IncidentFilters `json:"filters"`
}

type OccurrenceFilters struct {
	TenantID       *uuid.UUID            `json:"tenantId,omitempty"`
	ReportKeys     []report.Key          `json:"reportKeys,omitempty"`
	SourceKinds    []sentinel.SourceKind `json:"sourceKinds,omitempty"`
	SafeErrorCodes []string              `json:"safeErrorCodes,omitempty"`
	DateFrom       string                `json:"dateFrom,omitempty"`
	DateTo         string                `json:"dateTo,omitempty"`
}
type OccurrencesInput struct {
	CommonInput
	Filters OccurrenceFilters `json:"filters"`
}

type PageMeta struct {
	Page       int `json:"page"`
	PageSize   int `json:"pageSize"`
	Total      int `json:"total"`
	TotalPages int `json:"totalPages"`
}

type TenantsResult struct {
	Data []tenant.Tenant `json:"data"`
	PageMeta
}
type SchedulesResult struct {
	Data []schedule.Schedule `json:"data"`
	PageMeta
}
type ReportRunsResult struct {
	Data []operations.ReportRun `json:"-"`
	PageMeta
}
type DeliveriesResult struct {
	Data []operations.Delivery `json:"data"`
	PageMeta
}
type AuditResult struct {
	Data []operations.AuditEvent `json:"data"`
	PageMeta
}
type IncidentsResult struct {
	Data []sentinel.Incident `json:"data"`
	PageMeta
}
type OccurrencesResult struct {
	Data []sentinel.IncidentOccurrence `json:"data"`
	PageMeta
}

func NewPageMeta(page, pageSize, total int) PageMeta {
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	return PageMeta{Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages}
}

type Store interface {
	QueryTenants(context.Context, TenantsInput, time.Time) ([]tenant.Tenant, int, error)
	QuerySchedules(context.Context, uuid.UUID, SchedulesInput, time.Time) ([]schedule.Schedule, int, error)
	QueryReportRuns(context.Context, ReportRunsInput, time.Time) ([]operations.ReportRun, int, error)
	QueryDeliveries(context.Context, DeliveriesInput) ([]operations.Delivery, int, error)
	QueryAudit(context.Context, AuditInput) ([]operations.AuditEvent, int, error)
	QueryIncidents(context.Context, IncidentsInput) ([]sentinel.Incident, int, error)
	QueryOccurrences(context.Context, uuid.UUID, OccurrencesInput) ([]sentinel.IncidentOccurrence, int, error)
}

type RecipientNameResolver interface {
	DisplayName(recipient.StoredRecipient) (string, error)
}

func validCommon(input *CommonInput) bool {
	input.GlobalSearch = strings.TrimSpace(input.GlobalSearch)
	searchLength := utf8.RuneCountInString(input.GlobalSearch)
	return input.Page >= 0 && input.Page <= 200_000 && (input.PageSize == 25 || input.PageSize == 50 || input.PageSize == 100) &&
		(input.GlobalSearch == "" || searchLength >= 2 && searchLength <= 160)
}

// ValidateEnvelope performs the resource-independent validation before an API
// service is invoked. Resource-specific allowlists are still enforced by the
// service, so handlers never need reflection or client-provided column names.
func ValidateEnvelope(input any) bool {
	switch value := input.(type) {
	case *TenantsInput:
		return validCommon(&value.CommonInput)
	case *SchedulesInput:
		return validCommon(&value.CommonInput)
	case *ReportRunsInput:
		return validCommon(&value.CommonInput)
	case *DeliveriesInput:
		return validCommon(&value.CommonInput)
	case *AuditInput:
		return validCommon(&value.CommonInput)
	case *IncidentsInput:
		return validCommon(&value.CommonInput)
	case *OccurrencesInput:
		return validCommon(&value.CommonInput)
	default:
		return false
	}
}

func validDateRange(from, to string) bool {
	if from == "" && to == "" {
		return true
	}
	parse := func(value string) (time.Time, bool) {
		if value == "" {
			return time.Time{}, true
		}
		parsed, err := time.Parse("2006-01-02", value)
		return parsed, err == nil
	}
	fromDate, fromOK := parse(from)
	toDate, toOK := parse(to)
	return fromOK && toOK && (from == "" || to == "" || !fromDate.After(toDate))
}

func countFilters(values ...int) int {
	total := 0
	for _, value := range values {
		if value > 0 {
			total++
		}
	}
	return total
}
