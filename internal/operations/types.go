package operations

import (
	"context"
	"errors"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/quota"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

type LineQuotaStatus = quota.Status

var ErrInvalidCursor = errors.New("operations cursor is invalid")

type ReportRunFilter struct {
	TenantID    *uuid.UUID
	Status      *report.RunStatus
	ReportKey   *report.Key
	Source      *report.Source
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	Cursor      string
	PageSize    int
	Now         time.Time
}

type ReportRunPage struct {
	Data       []ReportRun
	NextCursor string
	HasMore    bool
}

type ReportRun struct {
	Run              report.Run
	TenantName       string
	RuntimeStatus    string
	RetryAvailableAt *time.Time
	WaitReason       *string
	FailureSummary   *failure.Evidence
}

type ReportRunDetail struct {
	ReportRun
	Impact                        failure.Impact
	TriggerKind                   string
	ConnectionChangedSinceFailure bool
}

type DeliveryStatus string

type Delivery struct {
	ID                uuid.UUID                 `json:"id"`
	TenantID          uuid.UUID                 `json:"tenantId"`
	TenantName        string                    `json:"tenantName"`
	RecipientName     string                    `json:"recipientDisplayName"`
	ReportKeys        []report.Key              `json:"reportKeys"`
	ReportCount       int                       `json:"reportCount"`
	Status            DeliveryStatus            `json:"status"`
	Attempt           int                       `json:"attempt"`
	SafeErrorCode     *string                   `json:"safeErrorCode"`
	ProviderRequestID *string                   `json:"providerRequestId,omitempty"`
	AcceptedAt        *time.Time                `json:"acceptedAt"`
	CreatedAt         time.Time                 `json:"createdAt"`
	ExpiresAt         time.Time                 `json:"expiresAt"`
	StoredRecipient   recipient.StoredRecipient `json:"-"`
}

type DeliveryFilter struct {
	TenantID    *uuid.UUID
	Status      *DeliveryStatus
	RecipientID *uuid.UUID
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	Cursor      string
	PageSize    int
}

type DeliveryPage struct {
	Data       []Delivery
	NextCursor string
	HasMore    bool
}

type AuditEvent struct {
	ID            uuid.UUID  `json:"id"`
	TenantID      *uuid.UUID `json:"tenantId"`
	TenantName    *string    `json:"tenantName"`
	ActorType     string     `json:"actorType"`
	Action        string     `json:"action"`
	ResourceType  string     `json:"resourceType"`
	ResourceID    *string    `json:"resourceId"`
	Result        string     `json:"result"`
	SafeErrorCode *string    `json:"safeErrorCode"`
	CreatedAt     time.Time  `json:"createdAt"`
}

type AuditFilter struct {
	TenantID    *uuid.UUID
	ActorType   *string
	Action      *string
	Result      *string
	CreatedFrom *time.Time
	CreatedTo   *time.Time
	Cursor      string
	PageSize    int
}

type AuditPage struct {
	Data       []AuditEvent
	NextCursor string
	HasMore    bool
}

type Store interface {
	GetLineQuota(context.Context, time.Time) (LineQuotaStatus, error)
	ListReportRuns(context.Context, ReportRunFilter) (ReportRunPage, error)
	GetReportRunDetail(context.Context, uuid.UUID, time.Time) (ReportRunDetail, error)
	ListDeliveries(context.Context, DeliveryFilter) (DeliveryPage, error)
	ListAudit(context.Context, AuditFilter) (AuditPage, error)
}

type RecipientNameResolver interface {
	DisplayName(recipient.StoredRecipient) (string, error)
}
