package tenant

import (
	"errors"
	"testing"
	"time"
)

func TestCreateInputNormalizeAndValidate(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	input := CreateInput{
		Slug:         "  shop-one ",
		Name:         "  ร้านหนึ่ง  ",
		Timezone:     "Asia/Bangkok",
		AccessEndsAt: now.AddDate(1, 0, 0),
	}

	normalized, err := input.NormalizeAndValidate(now)
	if err != nil {
		t.Fatalf("NormalizeAndValidate() error = %v", err)
	}
	if normalized.Slug != "shop-one" || normalized.Name != "ร้านหนึ่ง" || normalized.Status != StatusDisabled {
		t.Fatalf("normalized input = %+v", normalized)
	}
}

func TestCreateInputRejectsInvalidProductionFields(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		input CreateInput
		field string
	}{
		{name: "slug", input: CreateInput{Slug: "Shop One", Name: "Shop", Timezone: "Asia/Bangkok", AccessEndsAt: now.Add(time.Hour)}, field: "slug"},
		{name: "name", input: CreateInput{Slug: "shop", Name: " ", Timezone: "Asia/Bangkok", AccessEndsAt: now.Add(time.Hour)}, field: "name"},
		{name: "timezone", input: CreateInput{Slug: "shop", Name: "Shop", Timezone: "Mars/Olympus", AccessEndsAt: now.Add(time.Hour)}, field: "timezone"},
		{name: "non Thailand timezone", input: CreateInput{Slug: "shop", Name: "Shop", Timezone: "Asia/Tokyo", AccessEndsAt: now.Add(time.Hour)}, field: "timezone"},
		{name: "expiry", input: CreateInput{Slug: "shop", Name: "Shop", Timezone: "Asia/Bangkok", AccessEndsAt: now}, field: "accessEndsAt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.input.NormalizeAndValidate(now)
			var validationError *ValidationError
			if !errors.As(err, &validationError) || validationError.Field != test.field {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEffectiveStatusExpiresAccessWithoutTrustingStoredStatus(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	if got := EffectiveStatus(StatusActive, now.Add(-time.Second), now); got != StatusExpired {
		t.Fatalf("EffectiveStatus() = %s", got)
	}
	if got := EffectiveStatus(StatusDisabled, now.Add(time.Hour), now); got != StatusDisabled {
		t.Fatalf("EffectiveStatus() = %s", got)
	}
}

func TestPatchInputRequiresChangeAndValidVersion(t *testing.T) {
	now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	if _, err := (PatchInput{Version: 1}).NormalizeAndValidate(now); err == nil {
		t.Fatal("empty patch was accepted")
	}
	name := " New name "
	status := StatusActive
	patch, err := (PatchInput{Version: 2, Name: &name, Status: &status}).NormalizeAndValidate(now)
	if err != nil {
		t.Fatalf("NormalizeAndValidate() error = %v", err)
	}
	if *patch.Name != "New name" || patch.Version != 2 {
		t.Fatalf("patch = %+v", patch)
	}
}
