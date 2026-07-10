package operations

import (
	"errors"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/quota"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type LineQuotaStatus = quota.Status

var ErrInvalidCursor = errors.New("operations cursor is invalid")

type ReportRunFilter struct {
	TenantID *uuid.UUID
	Status   *report.RunStatus
	Cursor   string
	PageSize int
	Now      time.Time
}

type ReportRunPage struct {
	Data       []report.Run
	NextCursor string
	HasMore    bool
}

type DeliveryStatus string

type Delivery struct {
	ID                uuid.UUID      `json:"id"`
	TenantID          uuid.UUID      `json:"tenantId"`
	Status            DeliveryStatus `json:"status"`
	Attempt           int            `json:"attempt"`
	SafeErrorCode     *string        `json:"safeErrorCode"`
	ProviderRequestID *string        `json:"providerRequestId,omitempty"`
	AcceptedAt        *time.Time     `json:"acceptedAt"`
	CreatedAt         time.Time      `json:"createdAt"`
	ExpiresAt         time.Time      `json:"expiresAt"`
}

type DeliveryFilter struct {
	TenantID *uuid.UUID
	Cursor   string
	PageSize int
}

type DeliveryPage struct {
	Data       []Delivery
	NextCursor string
	HasMore    bool
}

type AuditEvent struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      *uuid.UUID `json:"tenantId"`
	ActorType     string     `json:"actorType"`
	Action        string     `json:"action"`
	ResourceType  string     `json:"resourceType"`
	ResourceID    *string    `json:"resourceId"`
	Result        string     `json:"result"`
	SafeErrorCode *string    `json:"safeErrorCode"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type AuditFilter struct {
	TenantID *uuid.UUID
	Cursor   string
	PageSize int
}

type AuditPage struct {
	Data       []AuditEvent
	NextCursor string
	HasMore    bool
}
