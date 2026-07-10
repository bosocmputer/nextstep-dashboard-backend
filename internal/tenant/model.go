package tenant

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusDisabled Status = "DISABLED"
	StatusExpired  Status = "EXPIRED"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type ValidationError struct {
	Field   string
	Code    string
	Message string
}

func (err *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", err.Field, err.Code)
}

type CreateInput struct {
	Slug         string
	Name         string
	Timezone     string
	AccessEndsAt time.Time
	Status       Status
}

type PatchInput struct {
	Name         *string
	Timezone     *string
	Status       *Status
	AccessEndsAt *time.Time
	Version      int
}

type Tenant struct {
	ID             uuid.UUID  `json:"id"`
	Slug           string     `json:"slug"`
	Name           string     `json:"name"`
	Timezone       string     `json:"timezone"`
	Status         Status     `json:"status"`
	AccessEndsAt   time.Time  `json:"accessEndsAt"`
	Version        int        `json:"version"`
	SMLReadiness   string     `json:"smlReadiness"`
	NextScheduleAt *time.Time `json:"nextScheduleAt"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type Page struct {
	Data       []Tenant
	NextCursor string
	HasMore    bool
}

func (input CreateInput) NormalizeAndValidate(now time.Time) (CreateInput, error) {
	input.Slug = strings.TrimSpace(input.Slug)
	input.Name = strings.TrimSpace(input.Name)
	input.Timezone = strings.TrimSpace(input.Timezone)
	input.Status = StatusDisabled
	if !slugPattern.MatchString(input.Slug) || len(input.Slug) > 80 {
		return CreateInput{}, validation("slug", "INVALID_SLUG", "Slug must contain lowercase letters, numbers, and single hyphens only.")
	}
	if len(input.Name) < 1 || len(input.Name) > 160 {
		return CreateInput{}, validation("name", "INVALID_LENGTH", "Tenant name must contain 1 to 160 characters.")
	}
	if err := validateTimezone(input.Timezone); err != nil {
		return CreateInput{}, err
	}
	if !input.AccessEndsAt.After(now) {
		return CreateInput{}, validation("accessEndsAt", "MUST_BE_FUTURE", "Access end time must be in the future.")
	}
	input.AccessEndsAt = input.AccessEndsAt.UTC()
	return input, nil
}

func (input PatchInput) NormalizeAndValidate(now time.Time) (PatchInput, error) {
	if input.Version < 1 {
		return PatchInput{}, validation("version", "INVALID_VERSION", "Version must be a positive integer.")
	}
	if input.Name == nil && input.Timezone == nil && input.Status == nil && input.AccessEndsAt == nil {
		return PatchInput{}, validation("body", "NO_CHANGES", "At least one tenant field must be supplied.")
	}
	if input.Name != nil {
		value := strings.TrimSpace(*input.Name)
		if len(value) < 1 || len(value) > 160 {
			return PatchInput{}, validation("name", "INVALID_LENGTH", "Tenant name must contain 1 to 160 characters.")
		}
		input.Name = &value
	}
	if input.Timezone != nil {
		value := strings.TrimSpace(*input.Timezone)
		if err := validateTimezone(value); err != nil {
			return PatchInput{}, err
		}
		input.Timezone = &value
	}
	if input.Status != nil && *input.Status != StatusActive && *input.Status != StatusDisabled && *input.Status != StatusExpired {
		return PatchInput{}, validation("status", "INVALID_STATUS", "Tenant status is invalid.")
	}
	if input.AccessEndsAt != nil {
		value := input.AccessEndsAt.UTC()
		if !value.After(now) {
			return PatchInput{}, validation("accessEndsAt", "MUST_BE_FUTURE", "Access end time must be in the future.")
		}
		input.AccessEndsAt = &value
	}
	return input, nil
}

func EffectiveStatus(stored Status, accessEndsAt, now time.Time) Status {
	if !accessEndsAt.After(now) {
		return StatusExpired
	}
	return stored
}

func validateTimezone(value string) error {
	if len(value) < 1 || len(value) > 64 {
		return validation("timezone", "INVALID_TIMEZONE", "Timezone is invalid.")
	}
	if _, err := time.LoadLocation(value); err != nil {
		return validation("timezone", "INVALID_TIMEZONE", "Timezone must be a valid IANA timezone.")
	}
	return nil
}

func validation(field, code, message string) *ValidationError {
	return &ValidationError{Field: field, Code: code, Message: message}
}
