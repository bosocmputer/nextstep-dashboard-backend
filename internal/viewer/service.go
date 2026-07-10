package viewer

import (
	"context"
	"crypto/hmac"
	"errors"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

var (
	ErrSessionInvalid             = errors.New("viewer session is invalid")
	ErrIdentityForbidden          = errors.New("LINE identity is not an active recipient")
	ErrDeliveryReferenceForbidden = errors.New("delivery reference does not belong to LINE identity")
	ErrReportForbidden            = errors.New("viewer report access is forbidden")
)

type SessionRecord struct {
	TokenHash   []byte
	RecipientID uuid.UUID
	CSRFHash    []byte
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

type TenantAccess struct {
	ID         uuid.UUID    `json:"id"`
	Name       string       `json:"name"`
	Timezone   string       `json:"timezone"`
	ReportKeys []report.Key `json:"reportKeys"`
}

type ReportAccess struct {
	Key         report.Key `json:"reportKey"`
	Version     string     `json:"version"`
	Label       string     `json:"label"`
	Category    string     `json:"category"`
	IsSensitive bool       `json:"isSensitive"`
}

type Store interface {
	CreateSession(context.Context, SessionRecord) error
	FindSession(context.Context, []byte, time.Time) (SessionRecord, error)
	RevokeSession(context.Context, []byte, time.Time) error
	CheckDeliveryReference(context.Context, []byte, uuid.UUID, time.Time) (bool, error)
	ListTenants(context.Context, uuid.UUID, time.Time) ([]TenantAccess, error)
	ListReports(context.Context, uuid.UUID, uuid.UUID, time.Time) ([]ReportAccess, error)
	CanAccessReport(context.Context, uuid.UUID, uuid.UUID, report.Key, time.Time) (bool, error)
}

type IdentityVerifier interface {
	Verify(context.Context, string) (line.Identity, error)
}

type RecipientResolver interface {
	ResolveIdentity(context.Context, line.Identity, string) (recipient.Recipient, error)
	Get(context.Context, uuid.UUID) (recipient.Recipient, error)
}

type Service struct {
	identityVerifier IdentityVerifier
	recipients       RecipientResolver
	store            Store
	tokens           *auth.SessionManager
	now              func() time.Time
}

type ExchangeResult struct {
	RawToken    string
	CSRFToken   string
	RecipientID uuid.UUID
	DisplayName string
	ExpiresAt   time.Time
}

type AuthenticatedViewer struct {
	TokenHash   []byte
	RecipientID uuid.UUID
	DisplayName string
	CSRFHash    []byte
	ExpiresAt   time.Time
}

func NewService(identityVerifier IdentityVerifier, recipients RecipientResolver, store Store, tokens *auth.SessionManager, now func() time.Time) *Service {
	return &Service{identityVerifier: identityVerifier, recipients: recipients, store: store, tokens: tokens, now: now}
}

func (service *Service) Exchange(ctx context.Context, idToken, invitationReference, deliveryReference string) (ExchangeResult, error) {
	if service.identityVerifier == nil {
		return ExchangeResult{}, &line.SafeError{Code: "LINE_LOGIN_NOT_CONFIGURED", Retryable: false}
	}
	identity, err := service.identityVerifier.Verify(ctx, idToken)
	if err != nil {
		return ExchangeResult{}, err
	}
	resolved, err := service.recipients.ResolveIdentity(ctx, identity, invitationReference)
	if errors.Is(err, recipient.ErrRecipientNotFound) || errors.Is(err, recipient.ErrInvitationInvalid) {
		return ExchangeResult{}, ErrIdentityForbidden
	}
	if err != nil {
		return ExchangeResult{}, err
	}
	if resolved.Status != recipient.StatusActive {
		return ExchangeResult{}, ErrIdentityForbidden
	}
	if deliveryReference != "" {
		if len(deliveryReference) < 32 || len(deliveryReference) > 512 {
			return ExchangeResult{}, ErrDeliveryReferenceForbidden
		}
		referenceHash := service.tokens.HashToken("delivery-reference:" + deliveryReference)
		allowed, err := service.store.CheckDeliveryReference(ctx, referenceHash, resolved.ID, service.now().UTC())
		if err != nil {
			return ExchangeResult{}, err
		}
		if !allowed {
			return ExchangeResult{}, ErrDeliveryReferenceForbidden
		}
	}
	issued, err := service.tokens.Issue(24 * time.Hour)
	if err != nil {
		return ExchangeResult{}, err
	}
	if err := service.store.CreateSession(ctx, SessionRecord{
		TokenHash: issued.TokenHash, RecipientID: resolved.ID, CSRFHash: issued.CSRFHash, ExpiresAt: issued.ExpiresAt,
	}); err != nil {
		return ExchangeResult{}, err
	}
	return ExchangeResult{
		RawToken: issued.RawToken, CSRFToken: issued.CSRFToken, RecipientID: resolved.ID,
		DisplayName: resolved.DisplayName, ExpiresAt: issued.ExpiresAt,
	}, nil
}

func (service *Service) Authenticate(ctx context.Context, rawToken string) (AuthenticatedViewer, error) {
	if rawToken == "" {
		return AuthenticatedViewer{}, ErrSessionInvalid
	}
	tokenHash := service.tokens.HashToken(rawToken)
	session, err := service.store.FindSession(ctx, tokenHash, service.now().UTC())
	if errors.Is(err, ErrSessionInvalid) {
		return AuthenticatedViewer{}, ErrSessionInvalid
	}
	if err != nil {
		return AuthenticatedViewer{}, err
	}
	resolved, err := service.recipients.Get(ctx, session.RecipientID)
	if errors.Is(err, recipient.ErrRecipientNotFound) || resolved.Status != recipient.StatusActive {
		return AuthenticatedViewer{}, ErrSessionInvalid
	}
	if err != nil {
		return AuthenticatedViewer{}, err
	}
	return AuthenticatedViewer{
		TokenHash: tokenHash, RecipientID: session.RecipientID, DisplayName: resolved.DisplayName,
		CSRFHash: session.CSRFHash, ExpiresAt: session.ExpiresAt,
	}, nil
}

func (service *Service) ValidateCSRF(viewer AuthenticatedViewer, token string) error {
	if token == "" || !hmac.Equal(service.tokens.HashToken(token), viewer.CSRFHash) {
		return auth.ErrInvalidCSRF
	}
	return nil
}

func (service *Service) Logout(ctx context.Context, viewer AuthenticatedViewer) error {
	return service.store.RevokeSession(ctx, viewer.TokenHash, service.now().UTC())
}

func (service *Service) ListTenants(ctx context.Context, recipientID uuid.UUID) ([]TenantAccess, error) {
	return service.store.ListTenants(ctx, recipientID, service.now().UTC())
}

func (service *Service) ListReports(ctx context.Context, recipientID, tenantID uuid.UUID) ([]ReportAccess, error) {
	return service.store.ListReports(ctx, recipientID, tenantID, service.now().UTC())
}

func (service *Service) CanAccessReport(ctx context.Context, recipientID, tenantID uuid.UUID, reportKey report.Key) (bool, error) {
	if _, ok := report.DefinitionFor(reportKey); !ok {
		return false, ErrReportForbidden
	}
	return service.store.CanAccessReport(ctx, recipientID, tenantID, reportKey, service.now().UTC())
}
