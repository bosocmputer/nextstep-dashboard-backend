package report

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type policyStoreFake struct {
	policy RefreshPolicy
	err    error
}

func (store *policyStoreFake) GetRefreshPolicy(context.Context, uuid.UUID) (RefreshPolicy, error) {
	return store.policy, store.err
}

func (store *policyStoreFake) PutRefreshPolicy(_ context.Context, _ []byte, _ string, policy RefreshPolicy, _ int, _ time.Time) (RefreshPolicy, error) {
	store.policy = policy
	return policy, store.err
}

func TestRefreshPolicyDefaultsAndSafeOptions(t *testing.T) {
	tenantID := uuid.New()
	service := NewRefreshPolicyService(&policyStoreFake{policy: DefaultRefreshPolicy(tenantID)}, func() time.Time { return time.Now() })
	policy, err := service.Get(context.Background(), tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if policy.FastIntervalMinutes == nil || *policy.FastIntervalMinutes != 5 || policy.StandardIntervalMinutes == nil || *policy.StandardIntervalMinutes != 15 || policy.HeavyIntervalMinutes == nil || *policy.HeavyIntervalMinutes != 30 {
		t.Fatalf("defaults = %+v", policy)
	}
	if interval, enabled := policy.IntervalFor(Definition{RefreshClass: RefreshHeavy}); !enabled || interval != 30*time.Minute {
		t.Fatalf("heavy interval = %s enabled=%v", interval, enabled)
	}
}

func TestRefreshPolicyRejectsUnsafeIntervals(t *testing.T) {
	value := 1
	service := NewRefreshPolicyService(&policyStoreFake{}, time.Now)
	_, err := service.Put(context.Background(), nil, "request", uuid.New(), RefreshPolicyInput{FastIntervalMinutes: &value, StandardIntervalMinutes: &value, HeavyIntervalMinutes: &value, Version: 1})
	if !errors.Is(err, ErrRefreshPolicyInvalid) {
		t.Fatalf("Put() error = %v", err)
	}
}

func TestRefreshPolicySupportsDisablingAClass(t *testing.T) {
	standard, heavy := 30, 60
	service := NewRefreshPolicyService(&policyStoreFake{}, time.Now)
	policy, err := service.Put(context.Background(), nil, "request", uuid.New(), RefreshPolicyInput{FastIntervalMinutes: nil, StandardIntervalMinutes: &standard, HeavyIntervalMinutes: &heavy, Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if interval, enabled := policy.IntervalFor(Definition{RefreshClass: RefreshFast}); enabled || interval != 0 {
		t.Fatalf("disabled fast interval = %s enabled=%v", interval, enabled)
	}
}
