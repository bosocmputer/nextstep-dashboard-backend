package schedule

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type Status string

const (
	StatusDraft    Status = "DRAFT"
	StatusActive   Status = "ACTIVE"
	StatusPaused   Status = "PAUSED"
	StatusExpired  Status = "EXPIRED"
	StatusArchived Status = "ARCHIVED"
)

const (
	BlockerTenantInactive              = "TENANT_INACTIVE"
	BlockerSMLNotReady                 = "SML_NOT_READY"
	BlockerRecipientNotActive          = "RECIPIENT_NOT_ACTIVE"
	BlockerRecipientPermissionMismatch = "RECIPIENT_PERMISSION_MISMATCH"
	BlockerLineNotConfigured           = "LINE_NOT_CONFIGURED"
)

var (
	ErrNotFound        = errors.New("schedule not found")
	ErrConflict        = errors.New("schedule conflict")
	ErrVersionConflict = errors.New("schedule version conflict")
	ErrStateConflict   = errors.New("schedule state conflict")
)

type ValidationError struct {
	Field string
	Code  string
}

func (err *ValidationError) Error() string { return err.Field + ": " + err.Code }

type ReadinessError struct {
	Blockers []string
}

func (err *ReadinessError) Error() string { return "schedule is not ready" }

type Input struct {
	Name         string        `json:"name"`
	DaysOfWeek   []int         `json:"daysOfWeek"`
	LocalTime    string        `json:"localTime"`
	Timezone     string        `json:"timezone"`
	PeriodPreset report.Preset `json:"periodPreset"`
	ReportKeys   []report.Key  `json:"reportKeys"`
	RecipientIDs []uuid.UUID   `json:"recipientIds"`
}

type Schedule struct {
	ID       uuid.UUID `json:"id"`
	TenantID uuid.UUID `json:"tenantId"`
	Input
	Status            Status      `json:"status"`
	Version           int         `json:"version"`
	ReadinessBlockers []string    `json:"readinessBlockers"`
	NextOccurrences   []time.Time `json:"nextOccurrences"`
	CreatedAt         time.Time   `json:"createdAt"`
	UpdatedAt         time.Time   `json:"updatedAt"`
	ArchivedAt        *time.Time  `json:"archivedAt,omitempty"`
}

type Page struct {
	Data       []Schedule
	NextCursor string
	HasMore    bool
}

type Store interface {
	Create(context.Context, []byte, string, string, uuid.UUID, Input, time.Time) (Schedule, error)
	List(context.Context, uuid.UUID, int, string, bool) (Page, error)
	Get(context.Context, uuid.UUID, uuid.UUID) (Schedule, error)
	Update(context.Context, []byte, string, uuid.UUID, uuid.UUID, Input, int, time.Time) (Schedule, error)
	Readiness(context.Context, uuid.UUID, []uuid.UUID, time.Time) (map[uuid.UUID][]string, error)
	Activate(context.Context, []byte, string, uuid.UUID, uuid.UUID, time.Time, time.Time) (Schedule, error)
	Pause(context.Context, []byte, string, uuid.UUID, uuid.UUID, time.Time) (Schedule, error)
	Archive(context.Context, []byte, string, uuid.UUID, uuid.UUID, int, time.Time) (Schedule, error)
	Restore(context.Context, []byte, string, uuid.UUID, uuid.UUID, int, time.Time) (Schedule, error)
}

type Service struct {
	store     Store
	lineReady bool
	now       func() time.Time
}

func NewService(store Store, lineReady bool, now func() time.Time) *Service {
	return &Service{store: store, lineReady: lineReady, now: now}
}

func Validate(input Input) (Input, error) {
	input.Name = strings.TrimSpace(input.Name)
	if len(input.Name) < 1 || len(input.Name) > 160 {
		return Input{}, &ValidationError{Field: "name", Code: "INVALID_NAME"}
	}
	if len(input.DaysOfWeek) < 1 || len(input.DaysOfWeek) > 7 {
		return Input{}, &ValidationError{Field: "daysOfWeek", Code: "INVALID_DAYS"}
	}
	days := append([]int(nil), input.DaysOfWeek...)
	sort.Ints(days)
	for index, day := range days {
		if day < 0 || day > 6 || index > 0 && days[index-1] == day {
			return Input{}, &ValidationError{Field: "daysOfWeek", Code: "INVALID_DAYS"}
		}
	}
	input.DaysOfWeek = days
	if _, err := time.Parse("15:04", input.LocalTime); err != nil || len(input.LocalTime) != 5 {
		return Input{}, &ValidationError{Field: "localTime", Code: "INVALID_LOCAL_TIME"}
	}
	if input.Timezone != "Asia/Bangkok" {
		return Input{}, &ValidationError{Field: "timezone", Code: "INVALID_TIMEZONE"}
	}
	switch input.PeriodPreset {
	case report.Yesterday, report.TodayToNow, report.MonthToDate, report.AsOfRun:
	default:
		return Input{}, &ValidationError{Field: "periodPreset", Code: "INVALID_PERIOD_PRESET"}
	}
	if len(input.ReportKeys) < 1 || len(input.ReportKeys) > 10 {
		return Input{}, &ValidationError{Field: "reportKeys", Code: "INVALID_REPORTS"}
	}
	seenReports := make(map[report.Key]struct{}, len(input.ReportKeys))
	for _, key := range input.ReportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return Input{}, &ValidationError{Field: "reportKeys", Code: "INVALID_REPORTS"}
		}
		if _, duplicate := seenReports[key]; duplicate {
			return Input{}, &ValidationError{Field: "reportKeys", Code: "DUPLICATE_REPORT"}
		}
		seenReports[key] = struct{}{}
	}
	input.ReportKeys = append([]report.Key(nil), input.ReportKeys...)
	if len(input.RecipientIDs) < 1 || len(input.RecipientIDs) > 500 {
		return Input{}, &ValidationError{Field: "recipientIds", Code: "INVALID_RECIPIENTS"}
	}
	seenRecipients := make(map[uuid.UUID]struct{}, len(input.RecipientIDs))
	for _, recipientID := range input.RecipientIDs {
		if recipientID == uuid.Nil {
			return Input{}, &ValidationError{Field: "recipientIds", Code: "INVALID_RECIPIENTS"}
		}
		if _, duplicate := seenRecipients[recipientID]; duplicate {
			return Input{}, &ValidationError{Field: "recipientIds", Code: "DUPLICATE_RECIPIENT"}
		}
		seenRecipients[recipientID] = struct{}{}
	}
	input.RecipientIDs = append([]uuid.UUID(nil), input.RecipientIDs...)
	return input, nil
}

func NextOccurrences(input Input, after time.Time, count int) ([]time.Time, error) {
	if count < 1 || count > 100 {
		return nil, &ValidationError{Field: "count", Code: "INVALID_COUNT"}
	}
	validated, err := Validate(input)
	if err != nil {
		return nil, err
	}
	return calculateOccurrences(validated.DaysOfWeek, validated.LocalTime, validated.Timezone, after, count)
}

func NextOccurrence(daysOfWeek []int, localTime, timezone string, after time.Time) (time.Time, error) {
	days := append([]int(nil), daysOfWeek...)
	sort.Ints(days)
	if len(days) < 1 || len(days) > 7 {
		return time.Time{}, &ValidationError{Field: "daysOfWeek", Code: "INVALID_DAYS"}
	}
	for index, day := range days {
		if day < 0 || day > 6 || index > 0 && days[index-1] == day {
			return time.Time{}, &ValidationError{Field: "daysOfWeek", Code: "INVALID_DAYS"}
		}
	}
	if _, err := time.Parse("15:04", localTime); err != nil || len(localTime) != 5 {
		return time.Time{}, &ValidationError{Field: "localTime", Code: "INVALID_LOCAL_TIME"}
	}
	if timezone != "Asia/Bangkok" {
		return time.Time{}, &ValidationError{Field: "timezone", Code: "INVALID_TIMEZONE"}
	}
	items, err := calculateOccurrences(days, localTime, timezone, after, 1)
	if err != nil {
		return time.Time{}, err
	}
	return items[0], nil
}

func calculateOccurrences(daysOfWeek []int, localTime, timezone string, after time.Time, count int) ([]time.Time, error) {
	location, _ := time.LoadLocation(timezone)
	clock, _ := time.Parse("15:04", localTime)
	localAfter := after.In(location)
	startDay := time.Date(localAfter.Year(), localAfter.Month(), localAfter.Day(), 0, 0, 0, 0, location)
	allowedDays := make(map[time.Weekday]struct{}, len(daysOfWeek))
	for _, day := range daysOfWeek {
		allowedDays[time.Weekday(day)] = struct{}{}
	}
	result := make([]time.Time, 0, count)
	for offset := 0; offset < 800 && len(result) < count; offset++ {
		day := startDay.AddDate(0, 0, offset)
		if _, ok := allowedDays[day.Weekday()]; !ok {
			continue
		}
		candidate := time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), 0, 0, location)
		candidateLocal := candidate.In(location)
		if candidateLocal.Year() != day.Year() || candidateLocal.YearDay() != day.YearDay() || candidateLocal.Hour() != clock.Hour() || candidateLocal.Minute() != clock.Minute() {
			continue
		}
		if candidate.After(after) {
			result = append(result, candidate.UTC())
		}
	}
	if len(result) != count {
		return nil, errors.New("unable to calculate schedule occurrences")
	}
	return result, nil
}

func (service *Service) Create(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, tenantID uuid.UUID, input Input) (Schedule, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return Schedule{}, &ValidationError{Field: "idempotencyKey", Code: "INVALID_IDEMPOTENCY_KEY"}
	}
	validated, err := Validate(input)
	if err != nil {
		return Schedule{}, err
	}
	created, err := service.store.Create(ctx, actorHash, requestID, idempotencyKey, tenantID, validated, service.now().UTC())
	if err != nil {
		return Schedule{}, err
	}
	return service.hydrate(ctx, created)
}

func (service *Service) List(ctx context.Context, tenantID uuid.UUID, pageSize int, cursor string, includeArchived bool) (Page, error) {
	if pageSize == 0 {
		pageSize = 25
	}
	if pageSize < 1 || pageSize > 100 {
		return Page{}, &ValidationError{Field: "pageSize", Code: "INVALID_PAGE_SIZE"}
	}
	page, err := service.store.List(ctx, tenantID, pageSize, cursor, includeArchived)
	if err != nil {
		return Page{}, err
	}
	items, err := service.hydrateMany(ctx, tenantID, page.Data)
	if err != nil {
		return Page{}, err
	}
	page.Data = items
	return page, nil
}

func (service *Service) Archive(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, version int) (Schedule, error) {
	if version < 1 {
		return Schedule{}, &ValidationError{Field: "version", Code: "INVALID_VERSION"}
	}
	item, err := service.store.Archive(ctx, actorHash, requestID, tenantID, scheduleID, version, service.now().UTC())
	if err != nil {
		return Schedule{}, err
	}
	return service.hydrate(ctx, item)
}

func (service *Service) Restore(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, version int) (Schedule, error) {
	if version < 1 {
		return Schedule{}, &ValidationError{Field: "version", Code: "INVALID_VERSION"}
	}
	item, err := service.store.Restore(ctx, actorHash, requestID, tenantID, scheduleID, version, service.now().UTC())
	if err != nil {
		return Schedule{}, err
	}
	return service.hydrate(ctx, item)
}

func (service *Service) Get(ctx context.Context, tenantID, scheduleID uuid.UUID) (Schedule, error) {
	item, err := service.store.Get(ctx, tenantID, scheduleID)
	if err != nil {
		return Schedule{}, err
	}
	return service.hydrate(ctx, item)
}

func (service *Service) Update(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID, input Input, version int) (Schedule, error) {
	validated, err := Validate(input)
	if err != nil || version < 1 {
		if err != nil {
			return Schedule{}, err
		}
		return Schedule{}, &ValidationError{Field: "version", Code: "INVALID_VERSION"}
	}
	updated, err := service.store.Update(ctx, actorHash, requestID, tenantID, scheduleID, validated, version, service.now().UTC())
	if err != nil {
		return Schedule{}, err
	}
	return service.hydrate(ctx, updated)
}

func (service *Service) Activate(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID) (Schedule, error) {
	item, err := service.store.Get(ctx, tenantID, scheduleID)
	if err != nil {
		return Schedule{}, err
	}
	hydrated, err := service.hydrate(ctx, item)
	if err != nil {
		return Schedule{}, err
	}
	if len(hydrated.ReadinessBlockers) > 0 {
		return Schedule{}, &ReadinessError{Blockers: hydrated.ReadinessBlockers}
	}
	now := service.now().UTC()
	occurrences, err := NextOccurrences(item.Input, now, 1)
	if err != nil {
		return Schedule{}, err
	}
	activated, err := service.store.Activate(ctx, actorHash, requestID, tenantID, scheduleID, occurrences[0], now)
	if err != nil {
		return Schedule{}, err
	}
	return service.hydrate(ctx, activated)
}

func (service *Service) Pause(ctx context.Context, actorHash []byte, requestID string, tenantID, scheduleID uuid.UUID) (Schedule, error) {
	paused, err := service.store.Pause(ctx, actorHash, requestID, tenantID, scheduleID, service.now().UTC())
	if err != nil {
		return Schedule{}, err
	}
	return service.hydrate(ctx, paused)
}

func (service *Service) hydrate(ctx context.Context, item Schedule) (Schedule, error) {
	items, err := service.hydrateMany(ctx, item.TenantID, []Schedule{item})
	if err != nil {
		return Schedule{}, err
	}
	return items[0], nil
}

func (service *Service) hydrateMany(ctx context.Context, tenantID uuid.UUID, items []Schedule) ([]Schedule, error) {
	if len(items) == 0 {
		return []Schedule{}, nil
	}
	ids := make([]uuid.UUID, 0, len(items))
	for index := range items {
		if items[index].Status != StatusArchived {
			ids = append(ids, items[index].ID)
		}
	}
	now := service.now().UTC()
	readiness, err := service.store.Readiness(ctx, tenantID, ids, now)
	if err != nil {
		return nil, err
	}
	for index := range items {
		if items[index].Status == StatusArchived {
			items[index].ReadinessBlockers = []string{}
			items[index].NextOccurrences = []time.Time{}
			continue
		}
		blockers := readiness[items[index].ID]
		items[index].ReadinessBlockers = make([]string, len(blockers))
		copy(items[index].ReadinessBlockers, blockers)
		if !service.lineReady {
			items[index].ReadinessBlockers = appendUnique(items[index].ReadinessBlockers, BlockerLineNotConfigured)
		}
		items[index].NextOccurrences, err = NextOccurrences(items[index].Input, now, 3)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
