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
	store RefreshPolicyStore
	now   func() time.Time
}

func NewRefreshPolicyService(store RefreshPolicyStore, now func() time.Time) *RefreshPolicyService {
	return &RefreshPolicyService{store: store, now: now}
}

func (service *RefreshPolicyService) Get(ctx context.Context, tenantID uuid.UUID) (RefreshPolicy, error) {
	return service.store.GetRefreshPolicy(ctx, tenantID)
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
	return service.store.PutRefreshPolicy(ctx, actorHash, requestID, policy, input.Version, service.now().UTC())
}

func validRefreshMinutes(value *int, allowed []int) bool {
	return value == nil || slices.Contains(allowed, *value)
}
