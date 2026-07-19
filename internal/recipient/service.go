package recipient

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/google/uuid"
)

type Status string

const (
	StatusPending Status = "PENDING"
	StatusActive  Status = "ACTIVE"
	StatusRevoked Status = "REVOKED"
)

var (
	ErrInvitationInvalid    = errors.New("recipient invitation is invalid or expired")
	ErrRecipientNotFound    = errors.New("recipient not found")
	ErrPermissionInvalid    = errors.New("recipient report permission is invalid")
	ErrInvalidInput         = errors.New("recipient input is invalid")
	ErrIdempotencyConflict  = errors.New("recipient idempotency conflict")
	ErrVersionConflict      = errors.New("recipient permission version conflict")
	ErrInvitationNotPending = errors.New("recipient invitation can only be reissued while pending")
)

type PermissionInUseError struct{ ScheduleNames []string }

func (err *PermissionInUseError) Error() string {
	return "recipient permission is used by an active schedule"
}

type RecipientInUseError struct{ ScheduleNames []string }

func (err *RecipientInUseError) Error() string {
	return "recipient is used by an active schedule"
}

type StoredRecipient struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	LineUserIDHash     []byte
	LineUserID         secret.Sealed
	DisplayName        secret.Sealed
	Status             Status
	ReportKeys         []report.Key
	PermissionsVersion int
	VerifiedAt         *time.Time
	CreatedAt          time.Time
}

type Recipient struct {
	ID                 uuid.UUID    `json:"id"`
	Status             Status       `json:"status"`
	DisplayName        string       `json:"displayName"`
	ReportKeys         []report.Key `json:"reportKeys"`
	PermissionsVersion int          `json:"permissionsVersion"`
	VerifiedAt         *time.Time   `json:"verifiedAt"`
	CreatedAt          time.Time    `json:"createdAt"`
	InvitationURL      string       `json:"invitationUrl,omitempty"`
}

type ScheduleDependency struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type PermissionDependency struct {
	ReportKey           report.Key           `json:"reportKey"`
	ActiveScheduleCount int                  `json:"activeScheduleCount"`
	Schedules           []ScheduleDependency `json:"schedules"`
	AdditionalCount     int                  `json:"additionalCount"`
}

type PermissionDependencies struct {
	RecipientID        uuid.UUID              `json:"recipientId"`
	PermissionsVersion int                    `json:"permissionsVersion"`
	Items              []PermissionDependency `json:"items"`
}

type ScheduleRecipientOption struct {
	Recipient
	Eligible          bool         `json:"eligible"`
	MissingReportKeys []report.Key `json:"missingReportKeys"`
}

type ScheduleRecipientOptionsInput struct {
	ReportKeys           []report.Key `json:"reportKeys"`
	SelectedRecipientIDs []uuid.UUID  `json:"selectedRecipientIds"`
	Search               string       `json:"search,omitempty"`
	GlobalSearch         string       `json:"globalSearch,omitempty"`
	Statuses             []Status     `json:"statuses,omitempty"`
	EligibilityStates    []string     `json:"eligibilityStates,omitempty"`
	Page                 int          `json:"page"`
	PageSize             int          `json:"pageSize"`
}

type ScheduleRecipientOptions struct {
	Data       []ScheduleRecipientOption `json:"data"`
	Selected   []ScheduleRecipientOption `json:"selected"`
	Page       int                       `json:"page"`
	PageSize   int                       `json:"pageSize"`
	Total      int                       `json:"total"`
	TotalPages int                       `json:"totalPages"`
	HasMore    bool                      `json:"hasMore"`
}

type QueryInput struct {
	Search           string   `json:"search,omitempty"`
	GlobalSearch     string   `json:"globalSearch,omitempty"`
	Status           Status   `json:"status,omitempty"`
	Statuses         []Status `json:"statuses,omitempty"`
	PermissionState  string   `json:"permissionState,omitempty"`
	PermissionStates []string `json:"permissionStates,omitempty"`
	Page             int      `json:"page"`
	PageSize         int      `json:"pageSize"`
}

type QueryResult struct {
	Data       []Recipient `json:"data"`
	Page       int         `json:"page"`
	PageSize   int         `json:"pageSize"`
	Total      int         `json:"total"`
	TotalPages int         `json:"totalPages"`
	HasMore    bool        `json:"hasMore"`
}

type Page struct {
	Stored     []StoredRecipient
	NextCursor string
	HasMore    bool
}

type RecipientPage struct {
	Data       []Recipient
	NextCursor string
	HasMore    bool
}

type Store interface {
	CreateInvitation(context.Context, []byte, string, string, []byte, StoredRecipient, []byte, time.Time, time.Time) (StoredRecipient, error)
	ReissueInvitation(context.Context, []byte, string, uuid.UUID, uuid.UUID, []byte, time.Time, time.Time) (StoredRecipient, error)
	List(context.Context, uuid.UUID, int, string) (Page, error)
	PermissionDependencies(context.Context, uuid.UUID, uuid.UUID) (PermissionDependencies, error)
	ListScheduleCandidates(context.Context, uuid.UUID, int) ([]StoredRecipient, error)
	ReplacePermissions(context.Context, []byte, string, uuid.UUID, uuid.UUID, []report.Key, int, time.Time) (StoredRecipient, error)
	Revoke(context.Context, []byte, string, uuid.UUID, uuid.UUID, time.Time) error
	RedeemInvitation(context.Context, []byte, []byte, StoredRecipient, time.Time) (StoredRecipient, error)
	FindByLineHash(context.Context, []byte) (StoredRecipient, error)
	GetByID(context.Context, uuid.UUID) (StoredRecipient, error)
	GetForTenant(context.Context, uuid.UUID, uuid.UUID) (StoredRecipient, error)
}

func (service *Service) PermissionDependencies(ctx context.Context, tenantID, recipientID uuid.UUID) (PermissionDependencies, error) {
	if tenantID == uuid.Nil || recipientID == uuid.Nil {
		return PermissionDependencies{}, ErrInvalidInput
	}
	return service.store.PermissionDependencies(ctx, tenantID, recipientID)
}

func (service *Service) ScheduleRecipientOptions(ctx context.Context, tenantID uuid.UUID, input ScheduleRecipientOptionsInput) (ScheduleRecipientOptions, error) {
	search, err := normalizedTableSearch(input.Search, input.GlobalSearch)
	if err != nil || tenantID == uuid.Nil || input.Page < 0 || !validTablePageSize(input.PageSize) || len(input.ReportKeys) < 1 || len(input.ReportKeys) > 10 || len(input.SelectedRecipientIDs) > 500 || !validRecipientStatuses(input.Statuses) || !validEligibilityStates(input.EligibilityStates) {
		return ScheduleRecipientOptions{}, ErrInvalidInput
	}
	required := make(map[report.Key]struct{}, len(input.ReportKeys))
	for _, key := range input.ReportKeys {
		if _, ok := report.DefinitionFor(key); !ok {
			return ScheduleRecipientOptions{}, ErrPermissionInvalid
		}
		if _, duplicate := required[key]; duplicate {
			return ScheduleRecipientOptions{}, ErrPermissionInvalid
		}
		required[key] = struct{}{}
	}
	selectedIDs := make(map[uuid.UUID]struct{}, len(input.SelectedRecipientIDs))
	for _, id := range input.SelectedRecipientIDs {
		if id == uuid.Nil {
			return ScheduleRecipientOptions{}, ErrInvalidInput
		}
		selectedIDs[id] = struct{}{}
	}
	stored, err := service.store.ListScheduleCandidates(ctx, tenantID, 501)
	if err != nil {
		return ScheduleRecipientOptions{}, err
	}
	// The schedule contract allows at most 500 recipients. Refuse an
	// incomplete candidate set rather than returning a misleading eligibility
	// result when a tenant exceeds that supported bound.
	if len(stored) > 500 {
		return ScheduleRecipientOptions{}, ErrInvalidInput
	}
	needle := strings.ToLower(search)
	filtered := make([]ScheduleRecipientOption, 0, len(stored))
	selected := make([]ScheduleRecipientOption, 0, len(selectedIDs))
	for _, item := range stored {
		public, err := service.publicRecipient(item)
		if err != nil {
			return ScheduleRecipientOptions{}, err
		}
		granted := make(map[report.Key]struct{}, len(public.ReportKeys))
		for _, key := range public.ReportKeys {
			granted[key] = struct{}{}
		}
		missing := make([]report.Key, 0)
		for _, key := range input.ReportKeys {
			if _, ok := granted[key]; !ok {
				missing = append(missing, key)
			}
		}
		option := ScheduleRecipientOption{Recipient: public, Eligible: public.Status == StatusActive && len(missing) == 0, MissingReportKeys: missing}
		if _, ok := selectedIDs[public.ID]; ok {
			selected = append(selected, option)
		}
		eligibilityState := "ELIGIBLE"
		if public.Status != StatusActive {
			eligibilityState = "NOT_ACTIVE"
		} else if len(missing) > 0 {
			eligibilityState = "MISSING_PERMISSIONS"
		}
		if containsRecipientStatus(input.Statuses, public.Status) && containsStringFilter(input.EligibilityStates, eligibilityState) && (needle == "" || strings.Contains(strings.ToLower(public.DisplayName), needle)) {
			filtered = append(filtered, option)
		}
	}
	start := input.Page * input.PageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := min(start+input.PageSize, len(filtered))
	return ScheduleRecipientOptions{
		Data: filtered[start:end], Selected: selected, Page: input.Page, PageSize: input.PageSize,
		Total: len(filtered), TotalPages: pageCount(len(filtered), input.PageSize), HasMore: end < len(filtered),
	}, nil
}

func (service *Service) Query(ctx context.Context, tenantID uuid.UUID, input QueryInput) (QueryResult, error) {
	search, err := normalizedTableSearch(input.Search, input.GlobalSearch)
	if err != nil || tenantID == uuid.Nil || input.Page < 0 || !validTablePageSize(input.PageSize) {
		return QueryResult{}, ErrInvalidInput
	}
	statuses := append([]Status(nil), input.Statuses...)
	if input.Status != "" {
		statuses = append(statuses, input.Status)
	}
	permissionStates := append([]string(nil), input.PermissionStates...)
	if input.PermissionState != "" {
		permissionStates = append(permissionStates, input.PermissionState)
	}
	if !validRecipientStatuses(statuses) {
		return QueryResult{}, ErrInvalidInput
	}
	if !validPermissionStates(permissionStates) {
		return QueryResult{}, ErrInvalidInput
	}
	stored, err := service.store.ListScheduleCandidates(ctx, tenantID, 501)
	if err != nil {
		return QueryResult{}, err
	}
	if len(stored) > 500 {
		return QueryResult{}, ErrInvalidInput
	}
	needle := strings.ToLower(search)
	filtered := make([]Recipient, 0, len(stored))
	for _, item := range stored {
		public, err := service.publicRecipient(item)
		if err != nil {
			return QueryResult{}, err
		}
		if !containsRecipientStatus(statuses, public.Status) {
			continue
		}
		permissionState := "WITHOUT_REPORTS"
		if len(public.ReportKeys) > 0 {
			permissionState = "WITH_REPORTS"
		}
		if !containsStringFilter(permissionStates, permissionState) {
			continue
		}
		if needle == "" || strings.Contains(strings.ToLower(public.DisplayName), needle) {
			filtered = append(filtered, public)
		}
	}
	start := min(input.Page*input.PageSize, len(filtered))
	end := min(start+input.PageSize, len(filtered))
	return QueryResult{Data: filtered[start:end], Page: input.Page, PageSize: input.PageSize, Total: len(filtered), TotalPages: pageCount(len(filtered), input.PageSize), HasMore: end < len(filtered)}, nil
}

func normalizedTableSearch(legacy, global string) (string, error) {
	legacy = strings.TrimSpace(legacy)
	global = strings.TrimSpace(global)
	if len([]rune(legacy)) > 160 || len([]rune(global)) > 160 || (global != "" && len([]rune(global)) < 2) {
		return "", ErrInvalidInput
	}
	if legacy != "" && global != "" && legacy != global {
		return "", ErrInvalidInput
	}
	if global != "" {
		return global, nil
	}
	return legacy, nil
}

func validTablePageSize(pageSize int) bool {
	return pageSize == 25 || pageSize == 50 || pageSize == 100
}

func validRecipientStatuses(statuses []Status) bool {
	if len(statuses) > 2 {
		return false
	}
	seen := make(map[Status]struct{}, len(statuses))
	for _, status := range statuses {
		if status != StatusPending && status != StatusActive {
			return false
		}
		if _, duplicate := seen[status]; duplicate {
			return false
		}
		seen[status] = struct{}{}
	}
	return true
}

func validPermissionStates(values []string) bool {
	return validStringEnum(values, map[string]struct{}{"WITH_REPORTS": {}, "WITHOUT_REPORTS": {}})
}

func validEligibilityStates(values []string) bool {
	return validStringEnum(values, map[string]struct{}{"ELIGIBLE": {}, "NOT_ACTIVE": {}, "MISSING_PERMISSIONS": {}})
}

func validStringEnum(values []string, allowed map[string]struct{}) bool {
	if len(values) > len(allowed) {
		return false
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, ok := allowed[value]; !ok {
			return false
		}
		if _, duplicate := seen[value]; duplicate {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func containsRecipientStatus(values []Status, value Status) bool {
	if len(values) == 0 {
		return true
	}
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func containsStringFilter(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func pageCount(total, pageSize int) int {
	if total == 0 {
		return 0
	}
	return (total + pageSize - 1) / pageSize
}

func (service *Service) Get(ctx context.Context, recipientID uuid.UUID) (Recipient, error) {
	stored, err := service.store.GetByID(ctx, recipientID)
	if err != nil {
		return Recipient{}, err
	}
	return service.publicRecipient(stored)
}

func (service *Service) GetForTenant(ctx context.Context, tenantID, recipientID uuid.UUID) (Recipient, error) {
	stored, err := service.store.GetForTenant(ctx, tenantID, recipientID)
	if err != nil {
		return Recipient{}, err
	}
	return service.publicRecipient(stored)
}

func (service *Service) DisplayName(stored StoredRecipient) (string, error) {
	public, err := service.publicRecipient(stored)
	if err != nil {
		return "", err
	}
	return public.DisplayName, nil
}

func (service *Service) OutboundLineUserID(ctx context.Context, recipientID uuid.UUID) (string, error) {
	stored, err := service.store.GetByID(ctx, recipientID)
	if err != nil || stored.Status != StatusActive || len(stored.LineUserIDHash) == 0 {
		return "", ErrRecipientNotFound
	}
	lineUserID, err := service.box.Decrypt(stored.LineUserID, activeLineIDAAD(stored.LineUserIDHash))
	if err != nil {
		return "", fmt.Errorf("decrypt outbound LINE recipient: %w", err)
	}
	if len(lineUserID) < 2 || len(lineUserID) > 128 {
		return "", ErrRecipientNotFound
	}
	return string(lineUserID), nil
}

type Service struct {
	store         Store
	box           *secret.Box
	tokens        *auth.SessionManager
	entropy       io.Reader
	publicBaseURL string
	now           func() time.Time
}

func NewService(store Store, box *secret.Box, tokens *auth.SessionManager, entropy io.Reader, publicBaseURL string, now func() time.Time) *Service {
	return &Service{store: store, box: box, tokens: tokens, entropy: entropy, publicBaseURL: strings.TrimRight(publicBaseURL, "/"), now: now}
}

func (service *Service) CreateInvitation(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, tenantID uuid.UUID, label string) (Recipient, error) {
	label = strings.TrimSpace(label)
	if len(label) < 1 || len(label) > 160 || len(idempotencyKey) < 8 || len(idempotencyKey) > 200 || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return Recipient{}, ErrInvalidInput
	}
	referenceBytes := service.tokens.HashToken("recipient-invitation-reference:" + hex.EncodeToString(actorHash) + ":" + tenantID.String() + ":" + idempotencyKey)
	reference := base64.RawURLEncoding.EncodeToString(referenceBytes)
	invitationHash := service.tokens.HashToken("recipient-invitation:" + reference)
	requestHash := service.tokens.HashToken("recipient-invitation-input:" + tenantID.String() + ":" + label)
	recipientID := uuid.New()
	displayName, err := service.box.Encrypt([]byte(label), pendingDisplayAAD(recipientID))
	if err != nil {
		return Recipient{}, err
	}
	now := service.now().UTC()
	pending := StoredRecipient{
		ID: recipientID, TenantID: tenantID, DisplayName: displayName, Status: StatusPending, ReportKeys: []report.Key{}, PermissionsVersion: 1, CreatedAt: now,
	}
	stored, err := service.store.CreateInvitation(ctx, actorHash, requestID, idempotencyKey, requestHash, pending, invitationHash, now.Add(7*24*time.Hour), now)
	if err != nil {
		return Recipient{}, err
	}
	public, err := service.publicRecipient(stored)
	if err != nil {
		return Recipient{}, err
	}
	public.InvitationURL = service.publicBaseURL + "/app/invite?ref=" + url.QueryEscape(reference)
	return public, nil
}

func (service *Service) ReissueInvitation(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, tenantID, recipientID uuid.UUID) (Recipient, error) {
	if tenantID == uuid.Nil || recipientID == uuid.Nil || len(idempotencyKey) < 8 || len(idempotencyKey) > 200 || strings.TrimSpace(idempotencyKey) != idempotencyKey {
		return Recipient{}, ErrInvalidInput
	}
	referenceBytes := service.tokens.HashToken("recipient-invitation-reissue-reference:" + hex.EncodeToString(actorHash) + ":" + tenantID.String() + ":" + recipientID.String() + ":" + idempotencyKey)
	reference := base64.RawURLEncoding.EncodeToString(referenceBytes)
	invitationHash := service.tokens.HashToken("recipient-invitation:" + reference)
	now := service.now().UTC()
	stored, err := service.store.ReissueInvitation(ctx, actorHash, requestID, tenantID, recipientID, invitationHash, now.Add(7*24*time.Hour), now)
	if err != nil {
		return Recipient{}, err
	}
	public, err := service.publicRecipient(stored)
	if err != nil {
		return Recipient{}, err
	}
	public.InvitationURL = service.publicBaseURL + "/app/invite?ref=" + url.QueryEscape(reference)
	return public, nil
}

func (service *Service) List(ctx context.Context, tenantID uuid.UUID, pageSize int, cursor string) (RecipientPage, error) {
	if pageSize == 0 {
		pageSize = 25
	}
	if pageSize < 1 || pageSize > 100 {
		return RecipientPage{}, ErrInvalidInput
	}
	page, err := service.store.List(ctx, tenantID, pageSize, cursor)
	if err != nil {
		return RecipientPage{}, err
	}
	result := RecipientPage{Data: make([]Recipient, 0, len(page.Stored)), NextCursor: page.NextCursor, HasMore: page.HasMore}
	for _, stored := range page.Stored {
		public, err := service.publicRecipient(stored)
		if err != nil {
			return RecipientPage{}, err
		}
		result.Data = append(result.Data, public)
	}
	return result, nil
}

func (service *Service) ReplacePermissions(ctx context.Context, actorHash []byte, requestID string, tenantID, recipientID uuid.UUID, keys []report.Key, version int) (Recipient, error) {
	if version < 1 || len(keys) > len(report.Keys()) {
		return Recipient{}, ErrPermissionInvalid
	}
	seen := make(map[report.Key]struct{}, len(keys))
	for _, key := range keys {
		if _, ok := report.DefinitionFor(key); !ok {
			return Recipient{}, ErrPermissionInvalid
		}
		if _, duplicate := seen[key]; duplicate {
			return Recipient{}, ErrPermissionInvalid
		}
		seen[key] = struct{}{}
	}
	stored, err := service.store.ReplacePermissions(ctx, actorHash, requestID, tenantID, recipientID, keys, version, service.now().UTC())
	if err != nil {
		return Recipient{}, err
	}
	return service.publicRecipient(stored)
}

func (service *Service) Revoke(ctx context.Context, actorHash []byte, requestID string, tenantID, recipientID uuid.UUID) error {
	if tenantID == uuid.Nil || recipientID == uuid.Nil {
		return ErrInvalidInput
	}
	return service.store.Revoke(ctx, actorHash, requestID, tenantID, recipientID, service.now().UTC())
}

func (service *Service) ResolveIdentity(ctx context.Context, identity line.Identity, invitationReference string) (Recipient, error) {
	if len(identity.Subject) < 2 || len(identity.Subject) > 128 {
		return Recipient{}, ErrRecipientNotFound
	}
	lineHash := service.tokens.HashToken("line-user:" + identity.Subject)
	if invitationReference == "" {
		stored, err := service.store.FindByLineHash(ctx, lineHash)
		if err != nil {
			return Recipient{}, err
		}
		return service.publicRecipient(stored)
	}
	if len(invitationReference) < 32 || len(invitationReference) > 128 {
		return Recipient{}, ErrInvitationInvalid
	}
	invitationHash := service.tokens.HashToken("recipient-invitation:" + invitationReference)
	lineUserID, err := service.box.Encrypt([]byte(identity.Subject), activeLineIDAAD(lineHash))
	if err != nil {
		return Recipient{}, err
	}
	displayName := strings.TrimSpace(identity.DisplayName)
	if displayName == "" {
		displayName = "LINE User"
	}
	sealedDisplayName, err := service.box.Encrypt([]byte(displayName), activeDisplayAAD(lineHash))
	if err != nil {
		return Recipient{}, err
	}
	now := service.now().UTC()
	stored, err := service.store.RedeemInvitation(ctx, invitationHash, lineHash, StoredRecipient{
		ID: uuid.New(), LineUserIDHash: lineHash, LineUserID: lineUserID, DisplayName: sealedDisplayName,
		Status: StatusActive, VerifiedAt: &now, CreatedAt: now,
	}, now)
	if err != nil {
		return Recipient{}, err
	}
	return service.publicRecipient(stored)
}

func (service *Service) publicRecipient(stored StoredRecipient) (Recipient, error) {
	aad := pendingDisplayAAD(stored.ID)
	if len(stored.LineUserIDHash) > 0 {
		aad = activeDisplayAAD(stored.LineUserIDHash)
	}
	displayName, err := service.box.Decrypt(stored.DisplayName, aad)
	if err != nil {
		return Recipient{}, fmt.Errorf("decrypt recipient display name: %w", err)
	}
	reportKeys := make([]report.Key, len(stored.ReportKeys))
	copy(reportKeys, stored.ReportKeys)
	return Recipient{
		ID: stored.ID, Status: stored.Status, DisplayName: string(displayName),
		ReportKeys: reportKeys, PermissionsVersion: stored.PermissionsVersion, VerifiedAt: stored.VerifiedAt, CreatedAt: stored.CreatedAt,
	}, nil
}

func pendingDisplayAAD(recipientID uuid.UUID) []byte {
	return []byte("recipient-pending:" + recipientID.String() + ":display")
}

func activeLineIDAAD(lineHash []byte) []byte {
	return []byte("recipient-active:" + hex.EncodeToString(lineHash) + ":line-id")
}

func activeDisplayAAD(lineHash []byte) []byte {
	return []byte("recipient-active:" + hex.EncodeToString(lineHash) + ":display")
}
