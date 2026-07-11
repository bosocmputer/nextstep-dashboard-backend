package schedule

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type memoryScheduleStore struct {
	item           Schedule
	blockers       []string
	activated      bool
	paused         bool
	archived       bool
	restored       bool
	listedArchived bool
	activation     time.Time
}

func (store *memoryScheduleStore) Create(_ context.Context, _ []byte, _, _ string, tenantID uuid.UUID, input Input, now time.Time) (Schedule, error) {
	store.item = Schedule{ID: uuid.New(), TenantID: tenantID, Input: input, Status: StatusDraft, Version: 1, CreatedAt: now, UpdatedAt: now}
	return store.item, nil
}

func (store *memoryScheduleStore) List(_ context.Context, _ uuid.UUID, _ int, _ string, includeArchived bool) (Page, error) {
	store.listedArchived = includeArchived
	return Page{Data: []Schedule{store.item}}, nil
}

func (store *memoryScheduleStore) Get(context.Context, uuid.UUID, uuid.UUID) (Schedule, error) {
	return store.item, nil
}

func (store *memoryScheduleStore) Update(_ context.Context, _ []byte, _ string, _, _ uuid.UUID, input Input, _ int, now time.Time) (Schedule, error) {
	store.item.Input = input
	store.item.UpdatedAt = now
	store.item.Version++
	return store.item, nil
}

func (store *memoryScheduleStore) Readiness(context.Context, uuid.UUID, []uuid.UUID, time.Time) (map[uuid.UUID][]string, error) {
	return map[uuid.UUID][]string{store.item.ID: append([]string(nil), store.blockers...)}, nil
}

func (store *memoryScheduleStore) Activate(_ context.Context, _ []byte, _ string, _, _ uuid.UUID, next time.Time, now time.Time) (Schedule, error) {
	if len(store.blockers) > 0 {
		return Schedule{}, &ReadinessError{Blockers: store.blockers}
	}
	store.activated = true
	store.activation = next
	store.item.Status = StatusActive
	store.item.UpdatedAt = now
	store.item.Version++
	return store.item, nil
}

func (store *memoryScheduleStore) Pause(_ context.Context, _ []byte, _ string, _, _ uuid.UUID, now time.Time) (Schedule, error) {
	store.paused = true
	store.item.Status = StatusPaused
	store.item.UpdatedAt = now
	store.item.Version++
	return store.item, nil
}

func (store *memoryScheduleStore) Archive(_ context.Context, _ []byte, _ string, _, _ uuid.UUID, _ int, now time.Time) (Schedule, error) {
	store.archived = true
	store.item.Status = StatusArchived
	store.item.ArchivedAt = &now
	store.item.UpdatedAt = now
	store.item.Version++
	return store.item, nil
}

func (store *memoryScheduleStore) Restore(_ context.Context, _ []byte, _ string, _, _ uuid.UUID, _ int, now time.Time) (Schedule, error) {
	store.restored = true
	store.item.Status = StatusDraft
	store.item.ArchivedAt = nil
	store.item.UpdatedAt = now
	store.item.Version++
	return store.item, nil
}

func validInput(recipientID uuid.UUID) Input {
	return Input{
		Name: "Morning summary", DaysOfWeek: []int{1, 3, 5}, LocalTime: "09:30", Timezone: "Asia/Bangkok",
		PeriodPreset: report.Yesterday, ReportKeys: []report.Key{report.SalesGoodsServices, report.StockBalance}, RecipientIDs: []uuid.UUID{recipientID},
	}
}

func TestValidateNormalizesOrderAndRejectsUserError(t *testing.T) {
	recipientID := uuid.New()
	input := validInput(recipientID)
	input.DaysOfWeek = []int{5, 1, 3}
	normalized, err := Validate(input)
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !reflect.DeepEqual(normalized.DaysOfWeek, []int{1, 3, 5}) {
		t.Fatalf("DaysOfWeek = %v", normalized.DaysOfWeek)
	}

	for _, mutate := range []func(*Input){
		func(value *Input) { value.DaysOfWeek = []int{1, 1} },
		func(value *Input) { value.LocalTime = "24:00" },
		func(value *Input) { value.Timezone = "GMT+7-ish" },
		func(value *Input) { value.Timezone = "Asia/Tokyo" },
		func(value *Input) { value.ReportKeys = append(value.ReportKeys, value.ReportKeys...) },
		func(value *Input) { value.RecipientIDs = []uuid.UUID{uuid.Nil} },
		func(value *Input) { value.PeriodPreset = report.Custom },
	} {
		invalid := validInput(recipientID)
		mutate(&invalid)
		if _, err := Validate(invalid); err == nil {
			t.Fatalf("invalid input accepted: %+v", invalid)
		}
	}
}

func TestValidateAcceptsTenReportsButRejectsEleven(t *testing.T) {
	input := validInput(uuid.New())
	input.ReportKeys = append([]report.Key(nil), report.Keys()...)
	if _, err := Validate(input); err != nil {
		t.Fatalf("ten reports rejected: %v", err)
	}
	input.ReportKeys = append(input.ReportKeys, report.Keys()[0])
	if _, err := Validate(input); err == nil {
		t.Fatal("eleven reports accepted")
	}
}

func TestNextOccurrencesUseLocalWeekdayAndTime(t *testing.T) {
	input := validInput(uuid.New())
	input.DaysOfWeek = []int{6, 0}
	input.LocalTime = "09:30"
	now := time.Date(2026, 7, 10, 18, 0, 0, 0, time.UTC) // Saturday 01:00 Bangkok
	occurrences, err := NextOccurrences(input, now, 3)
	if err != nil {
		t.Fatalf("NextOccurrences() error = %v", err)
	}
	want := []time.Time{
		time.Date(2026, 7, 11, 2, 30, 0, 0, time.UTC),
		time.Date(2026, 7, 12, 2, 30, 0, 0, time.UTC),
		time.Date(2026, 7, 18, 2, 30, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(occurrences, want) {
		t.Fatalf("NextOccurrences() = %v, want %v", occurrences, want)
	}
}

func TestHydrationReturnsEmptyReadinessBlockersAsArray(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &memoryScheduleStore{}
	service := NewService(store, true, func() time.Time { return now })
	created, err := service.Create(context.Background(), []byte("admin"), "request-1", "schedule-create-1", uuid.New(), validInput(uuid.New()))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ReadinessBlockers == nil || len(created.ReadinessBlockers) != 0 {
		t.Fatalf("Create().ReadinessBlockers = %#v, want non-nil empty slice", created.ReadinessBlockers)
	}

	page, err := service.List(context.Background(), created.TenantID, 25, "", false)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ReadinessBlockers == nil || len(page.Data[0].ReadinessBlockers) != 0 {
		t.Fatalf("List().Data = %#v, want one schedule with non-nil empty readiness blockers", page.Data)
	}
}

func TestArchiveAndRestoreUseVersionAndArchivedHydration(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := &memoryScheduleStore{}
	service := NewService(store, true, func() time.Time { return now })
	created, err := service.Create(context.Background(), []byte("admin"), "request-1", "schedule-create-1", uuid.New(), validInput(uuid.New()))
	if err != nil {
		t.Fatal(err)
	}
	archived, err := service.Archive(context.Background(), []byte("admin"), "request-2", created.TenantID, created.ID, created.Version)
	if err != nil || !store.archived || archived.Status != StatusArchived || archived.ArchivedAt == nil {
		t.Fatalf("Archive() = %+v err=%v", archived, err)
	}
	if len(archived.ReadinessBlockers) != 0 || len(archived.NextOccurrences) != 0 {
		t.Fatalf("archived hydration exposes readiness/future runs: %+v", archived)
	}
	restored, err := service.Restore(context.Background(), []byte("admin"), "request-3", created.TenantID, created.ID, archived.Version)
	if err != nil || !store.restored || restored.Status != StatusDraft || restored.ArchivedAt != nil {
		t.Fatalf("Restore() = %+v err=%v", restored, err)
	}
	if _, err := service.List(context.Background(), created.TenantID, 25, "", true); err != nil || !store.listedArchived {
		t.Fatalf("List(includeArchived) err=%v listed=%v", err, store.listedArchived)
	}
}

func TestActivationReportsAllReadinessBlockersAndComputesNextRun(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	store := &memoryScheduleStore{}
	service := NewService(store, false, func() time.Time { return now })
	created, err := service.Create(context.Background(), []byte("admin"), "request-1", "schedule-create-1", uuid.New(), validInput(uuid.New()))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	_, err = service.Activate(context.Background(), []byte("admin"), "request-2", created.TenantID, created.ID)
	var readiness *ReadinessError
	if !errors.As(err, &readiness) || !reflect.DeepEqual(readiness.Blockers, []string{BlockerLineNotConfigured}) {
		t.Fatalf("Activate() error = %v", err)
	}

	service = NewService(store, true, func() time.Time { return now })
	activated, err := service.Activate(context.Background(), []byte("admin"), "request-3", created.TenantID, created.ID)
	if err != nil || activated.Status != StatusActive || !store.activated || store.activation.IsZero() {
		t.Fatalf("Activate() = %+v, %v activation=%s", activated, err, store.activation)
	}
}
