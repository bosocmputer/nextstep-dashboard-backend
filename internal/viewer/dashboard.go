package viewer

import (
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type DashboardSnapshot struct {
	RunID     uuid.UUID        `json:"runId"`
	Dashboard report.Dashboard `json:"dashboard"`
}

type ExecutiveOverview struct {
	TenantID uuid.UUID           `json:"tenantId"`
	Timezone string              `json:"timezone"`
	Items    []DashboardSnapshot `json:"items"`
}

type DashboardRefreshStatus string

const (
	DashboardRefreshQueued    DashboardRefreshStatus = "QUEUED"
	DashboardRefreshRunning   DashboardRefreshStatus = "RUNNING"
	DashboardRefreshPartial   DashboardRefreshStatus = "PARTIAL"
	DashboardRefreshSucceeded DashboardRefreshStatus = "SUCCEEDED"
	DashboardRefreshFailed    DashboardRefreshStatus = "FAILED"
)

type DashboardRefreshRun struct {
	ReportKey report.Key       `json:"reportKey"`
	RunID     uuid.UUID        `json:"runId"`
	Status    report.RunStatus `json:"status"`
}

type DashboardRefresh struct {
	ID         uuid.UUID              `json:"id"`
	TenantID   uuid.UUID              `json:"tenantId"`
	Status     DashboardRefreshStatus `json:"status"`
	Total      int                    `json:"total"`
	Completed  int                    `json:"completed"`
	Failed     int                    `json:"failed"`
	Runs       []DashboardRefreshRun  `json:"runs"`
	CreatedAt  time.Time              `json:"createdAt"`
	FinishedAt *time.Time             `json:"finishedAt,omitempty"`
}
