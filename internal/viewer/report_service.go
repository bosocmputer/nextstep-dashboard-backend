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

type ReportAccessControl interface {
	ListTenants(context.Context, uuid.UUID) ([]TenantAccess, error)
	CanAccessReport(context.Context, uuid.UUID, uuid.UUID, report.Key) (bool, error)
}

type ViewerRunStore interface {
	Enqueue(context.Context, report.EnqueueInput, time.Time) (report.Run, error)
	Get(context.Context, uuid.UUID, time.Time) (report.Run, error)
	GetDashboard(context.Context, uuid.UUID, uuid.UUID) (report.Dashboard, error)
	ListRows(context.Context, uuid.UUID, int, int, time.Time) (report.RowsPage, error)
	Cancel(context.Context, uuid.UUID, time.Time) (report.Run, error)
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
	access ReportAccessControl
	store  ViewerRunStore
	now    func() time.Time
}

func NewReportService(access ReportAccessControl, store ViewerRunStore, now func() time.Time) *ReportService {
	return &ReportService{access: access, store: store, now: now}
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
	if definition.ParameterKind == report.AsOfDate && input.PeriodPreset == report.Custom && period.DateFrom != period.DateTo {
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
	if !runOwnedBy(run, recipientID, tenantID, reportKey) {
		return report.Run{}, ErrReportForbidden
	}
	return run, nil
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

func (service *ReportService) Cancel(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key, runID uuid.UUID) (report.Run, error) {
	if _, err := service.Get(ctx, recipientID, tenantID, reportKey, runID); err != nil {
		return report.Run{}, err
	}
	return service.store.Cancel(ctx, runID, service.now().UTC())
}

func (service *ReportService) authorizedTimezone(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key) (string, error) {
	tenants, err := service.access.ListTenants(ctx, recipientID)
	if err != nil {
		return "", err
	}
	for _, item := range tenants {
		if item.ID == tenantID {
			if err := service.authorizeReport(ctx, recipientID, tenantID, reportKey); err != nil {
				return "", err
			}
			return item.Timezone, nil
		}
	}
	return "", ErrReportForbidden
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

func runOwnedBy(run report.Run, recipientID, tenantID uuid.UUID, reportKey report.Key) bool {
	return run.Source == report.SourceDashboard && run.TenantID == tenantID && run.ReportKey == reportKey &&
		run.RequestedByRecipient != nil && *run.RequestedByRecipient == recipientID
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
