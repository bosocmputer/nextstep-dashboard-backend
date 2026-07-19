package tablequery

import (
	"context"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/google/uuid"
)

type scheduleQueryStore struct {
	Store
	items []schedule.Schedule
}

func (store scheduleQueryStore) QuerySchedules(context.Context, uuid.UUID, SchedulesInput, time.Time) ([]schedule.Schedule, int, error) {
	return store.items, len(store.items), nil
}

func TestQuerySchedulesReturnsNonNilCollections(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	input := schedule.Input{
		Name:         "Morning summary",
		DaysOfWeek:   []int{1},
		LocalTime:    "08:00",
		Timezone:     "Asia/Bangkok",
		PeriodPreset: report.Yesterday,
		ReportKeys:   []report.Key{report.SalesGoodsServices},
		RecipientIDs: []uuid.UUID{uuid.New()},
	}
	tests := []struct {
		name   string
		status schedule.Status
	}{
		{name: "ready schedule", status: schedule.StatusDraft},
		{name: "archived schedule", status: schedule.StatusArchived},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := NewService(scheduleQueryStore{items: []schedule.Schedule{{
				ID: uuid.New(), TenantID: tenantID, Input: input, Status: test.status,
			}}}, nil, true, func() time.Time { return now })

			result, err := service.QuerySchedules(context.Background(), tenantID, SchedulesInput{
				CommonInput: CommonInput{Page: 0, PageSize: 25},
			})
			if err != nil {
				t.Fatalf("QuerySchedules() error = %v", err)
			}
			if len(result.Data) != 1 {
				t.Fatalf("QuerySchedules().Data length = %d, want 1", len(result.Data))
			}
			if result.Data[0].ReadinessBlockers == nil {
				t.Fatal("QuerySchedules().Data[0].ReadinessBlockers is nil, want []")
			}
			if result.Data[0].NextOccurrences == nil {
				t.Fatal("QuerySchedules().Data[0].NextOccurrences is nil, want [] or calculated occurrences")
			}
		})
	}
}
