package tenant

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound            = errors.New("tenant not found")
	ErrConflict            = errors.New("tenant conflict")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different input")
)

type ListFilter struct {
	Cursor   string
	PageSize int
	Status   *Status
	Search   string
}

type Store interface {
	Create(context.Context, []byte, string, string, CreateInput, time.Time) (Tenant, error)
	List(context.Context, ListFilter, time.Time) (Page, error)
	Get(context.Context, uuid.UUID, time.Time) (Tenant, error)
	Update(context.Context, []byte, string, uuid.UUID, PatchInput, time.Time) (Tenant, error)
	Archive(context.Context, []byte, string, uuid.UUID, int, time.Time) error
}

type Service struct {
	store Store
	now   func() time.Time
}

func NewService(store Store, now func() time.Time) *Service {
	return &Service{store: store, now: now}
}

func (service *Service) Create(ctx context.Context, actorHash []byte, requestID, idempotencyKey string, input CreateInput) (Tenant, error) {
	if len(idempotencyKey) < 8 || len(idempotencyKey) > 200 {
		return Tenant{}, validation("Idempotency-Key", "INVALID_IDEMPOTENCY_KEY", "Idempotency key must contain 8 to 200 characters.")
	}
	normalized, err := input.NormalizeAndValidate(service.now().UTC())
	if err != nil {
		return Tenant{}, err
	}
	return service.store.Create(ctx, actorHash, requestID, idempotencyKey, normalized, service.now().UTC())
}

func (service *Service) List(ctx context.Context, filter ListFilter) (Page, error) {
	if filter.PageSize == 0 {
		filter.PageSize = 25
	}
	if filter.PageSize < 1 || filter.PageSize > 100 {
		return Page{}, validation("pageSize", "INVALID_PAGE_SIZE", "Page size must be between 1 and 100.")
	}
	filter.Search = strings.TrimSpace(filter.Search)
	if len(filter.Search) > 160 {
		return Page{}, validation("search", "INVALID_LENGTH", "Search text must not exceed 160 characters.")
	}
	if filter.Status != nil && *filter.Status != StatusActive && *filter.Status != StatusDisabled && *filter.Status != StatusExpired {
		return Page{}, validation("status", "INVALID_STATUS", "Tenant status is invalid.")
	}
	return service.store.List(ctx, filter, service.now().UTC())
}

func (service *Service) Get(ctx context.Context, id uuid.UUID) (Tenant, error) {
	return service.store.Get(ctx, id, service.now().UTC())
}

func (service *Service) Update(ctx context.Context, actorHash []byte, requestID string, id uuid.UUID, input PatchInput) (Tenant, error) {
	normalized, err := input.NormalizeAndValidate(service.now().UTC())
	if err != nil {
		return Tenant{}, err
	}
	return service.store.Update(ctx, actorHash, requestID, id, normalized, service.now().UTC())
}

func (service *Service) Archive(ctx context.Context, actorHash []byte, requestID string, id uuid.UUID, version int) error {
	if version < 1 {
		return validation("version", "INVALID_VERSION", "Version must be a positive integer.")
	}
	return service.store.Archive(ctx, actorHash, requestID, id, version, service.now().UTC())
}
