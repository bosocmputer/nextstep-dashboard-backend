package report

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRefreshPolicyInvalid  = errors.New("dashboard refresh policy is invalid")
	ErrRefreshPolicyConflict = errors.New("dashboard refresh policy version conflict")
)

type RefreshPolicy struct {
	TenantID                uuid.UUID `json:"tenantId"`
	FastIntervalMinutes     *int      `json:"fastIntervalMinutes"`
	StandardIntervalMinutes *int      `json:"standardIntervalMinutes"`
	HeavyIntervalMinutes    *int      `json:"heavyIntervalMinutes"`
	Version                 int       `json:"version"`
	RolloutStatus           *string   `json:"rolloutStatus,omitempty"`
}

type RefreshPolicyInput struct {
	FastIntervalMinutes     *int `json:"fastIntervalMinutes"`
	StandardIntervalMinutes *int `json:"standardIntervalMinutes"`
	HeavyIntervalMinutes    *int `json:"heavyIntervalMinutes"`
	Version                 int  `json:"version"`
}

func DefaultRefreshPolicy(tenantID uuid.UUID) RefreshPolicy {
	fast, standard, heavy := 5, 15, 30
	return RefreshPolicy{
		TenantID: tenantID, FastIntervalMinutes: &fast,
		StandardIntervalMinutes: &standard, HeavyIntervalMinutes: &heavy,
	}
}

func (policy RefreshPolicy) IntervalFor(definition Definition) (time.Duration, bool) {
	var minutes *int
	switch definition.RefreshClass {
	case RefreshFast:
		minutes = policy.FastIntervalMinutes
	case RefreshStandard:
		minutes = policy.StandardIntervalMinutes
	case RefreshHeavy:
		minutes = policy.HeavyIntervalMinutes
	}
	if minutes == nil {
		return 0, false
	}
	return time.Duration(*minutes) * time.Minute, true
}

type RefreshPolicyStore interface {
	GetRefreshPolicy(context.Context, uuid.UUID) (RefreshPolicy, error)
	PutRefreshPolicy(context.Context, []byte, string, RefreshPolicy, int, time.Time) (RefreshPolicy, error)
}

type RefreshPolicyService struct {
	store                    RefreshPolicyStore
	now                      func() time.Time
	snapshotFirstEnabled     bool
	snapshotFirstTenantIDs   map[uuid.UUID]struct{}
	staleRevalidationEnabled bool
}

func (service *RefreshPolicyService) ConfigureRollout(snapshotFirstEnabled bool, tenantIDs []uuid.UUID, staleRevalidationEnabled bool) *RefreshPolicyService {
	service.snapshotFirstEnabled = snapshotFirstEnabled
	service.staleRevalidationEnabled = staleRevalidationEnabled
	service.snapshotFirstTenantIDs = make(map[uuid.UUID]struct{}, len(tenantIDs))
	for _, id := range tenantIDs {
		service.snapshotFirstTenantIDs[id] = struct{}{}
	}
	return service
}

func NewRefreshPolicyService(store RefreshPolicyStore, now func() time.Time) *RefreshPolicyService {
	return &RefreshPolicyService{store: store, now: now}
}

func (service *RefreshPolicyService) Get(ctx context.Context, tenantID uuid.UUID) (RefreshPolicy, error) {
	policy, err := service.store.GetRefreshPolicy(ctx, tenantID)
	if err != nil {
		return RefreshPolicy{}, err
	}
	return service.withRolloutStatus(policy), nil
}

func (service *RefreshPolicyService) Put(ctx context.Context, actorHash []byte, requestID string, tenantID uuid.UUID, input RefreshPolicyInput) (RefreshPolicy, error) {
	if input.Version < 0 || !validRefreshMinutes(input.FastIntervalMinutes, []int{5, 10, 15, 30, 60}) ||
		!validRefreshMinutes(input.StandardIntervalMinutes, []int{15, 30, 60}) ||
		!validRefreshMinutes(input.HeavyIntervalMinutes, []int{30, 60, 120}) {
		return RefreshPolicy{}, ErrRefreshPolicyInvalid
	}
	policy := RefreshPolicy{
		TenantID: tenantID, FastIntervalMinutes: input.FastIntervalMinutes,
		StandardIntervalMinutes: input.StandardIntervalMinutes,
		HeavyIntervalMinutes:    input.HeavyIntervalMinutes, Version: input.Version,
	}
	updated, err := service.store.PutRefreshPolicy(ctx, actorHash, requestID, policy, input.Version, service.now().UTC())
	if err != nil {
		return RefreshPolicy{}, err
	}
	return service.withRolloutStatus(updated), nil
}

func (service *RefreshPolicyService) withRolloutStatus(policy RefreshPolicy) RefreshPolicy {
	status := "ACTIVE"
	if !service.snapshotFirstEnabled {
		status = "SNAPSHOT_FIRST_DISABLED"
	} else if len(service.snapshotFirstTenantIDs) > 0 {
		if _, ok := service.snapshotFirstTenantIDs[policy.TenantID]; !ok {
			status = "TENANT_NOT_ENABLED"
		} else if !service.staleRevalidationEnabled {
			status = "REVALIDATION_DISABLED"
		}
	} else if !service.staleRevalidationEnabled {
		status = "REVALIDATION_DISABLED"
	}
	policy.RolloutStatus = &status
	return policy
}

func validRefreshMinutes(value *int, allowed []int) bool {
	return value == nil || slices.Contains(allowed, *value)
}
