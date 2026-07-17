package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const maximumEmergencyStateBytes = 4096

var alertReferencePattern = regexp.MustCompile(`^NST-[A-Z0-9]{12}$`)

type EmergencyState struct {
	AlertRef                 string     `json:"alertRef,omitempty"`
	DatabaseFailureStartedAt *time.Time `json:"databaseFailureStartedAt,omitempty"`
	LastEmergencySentAt      *time.Time `json:"lastEmergencySentAt,omitempty"`
	RecoveryPending          bool       `json:"recoveryPending"`
}

type EmergencyStateStore struct{ path string }

func NewEmergencyStateStore(path string) *EmergencyStateStore {
	return &EmergencyStateStore{path: path}
}

func (store *EmergencyStateStore) Load() (EmergencyState, error) {
	file, err := os.Open(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return EmergencyState{}, nil
	}
	if err != nil {
		return EmergencyState{}, fmt.Errorf("open Sentinel emergency state: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return EmergencyState{}, fmt.Errorf("stat Sentinel emergency state: %w", err)
	}
	if info.Size() > maximumEmergencyStateBytes {
		return EmergencyState{}, errors.New("Sentinel emergency state is oversized")
	}
	decoder := json.NewDecoder(io.LimitReader(file, maximumEmergencyStateBytes+1))
	decoder.DisallowUnknownFields()
	var state EmergencyState
	if err := decoder.Decode(&state); err != nil {
		return EmergencyState{}, errors.New("Sentinel emergency state is invalid")
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return EmergencyState{}, errors.New("Sentinel emergency state contains trailing data")
	}
	if err := validateEmergencyState(state); err != nil {
		return EmergencyState{}, err
	}
	return state, nil
}

func validateEmergencyState(state EmergencyState) error {
	if state.AlertRef != "" && !alertReferencePattern.MatchString(state.AlertRef) {
		return errors.New("Sentinel emergency alert reference is invalid")
	}
	if state.RecoveryPending && (state.AlertRef == "" || state.DatabaseFailureStartedAt == nil || state.LastEmergencySentAt == nil) {
		return errors.New("Sentinel emergency recovery state is incomplete")
	}
	return nil
}

func (store *EmergencyStateStore) Save(state EmergencyState) error {
	if store.path == "" || !filepath.IsAbs(store.path) {
		return errors.New("Sentinel emergency state path must be absolute")
	}
	if err := validateEmergencyState(state); err != nil {
		return err
	}
	body, err := json.Marshal(state)
	if err != nil || len(body) > maximumEmergencyStateBytes {
		return errors.New("Sentinel emergency state cannot be encoded safely")
	}
	directory := filepath.Dir(store.path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Sentinel state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".sentinel-state-*")
	if err != nil {
		return fmt.Errorf("create Sentinel temporary state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect Sentinel temporary state: %w", err)
	}
	if _, err := io.Copy(temporary, bytes.NewReader(body)); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write Sentinel temporary state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync Sentinel temporary state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Sentinel temporary state: %w", err)
	}
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return fmt.Errorf("publish Sentinel emergency state: %w", err)
	}
	return os.Chmod(store.path, 0o600)
}

type DatabaseIncidentReconciler interface {
	ReconcileDatabaseIncident(context.Context, string, time.Time, time.Time) (Incident, error)
}

type EmergencyLane struct {
	state            *EmergencyStateStore
	sender           Sender
	adminIncidentURL string
}

func NewEmergencyLane(state *EmergencyStateStore, sender Sender, adminIncidentURL string) *EmergencyLane {
	return &EmergencyLane{state: state, sender: sender, adminIncidentURL: adminIncidentURL}
}

func (lane *EmergencyLane) DatabaseUnavailable(ctx context.Context, now time.Time) error {
	state, err := lane.state.Load()
	if err != nil {
		return err
	}
	if state.RecoveryPending {
		return nil
	}
	if state.AlertRef == "" {
		state.AlertRef, err = NewAlertReference()
		if err != nil {
			return err
		}
	}
	startedAt := now.UTC()
	if state.DatabaseFailureStartedAt != nil {
		startedAt = state.DatabaseFailureStartedAt.UTC()
	} else {
		state.DatabaseFailureStartedAt = &startedAt
	}
	sentAt := now.UTC()
	state.LastEmergencySentAt = &sentAt
	state.RecoveryPending = true
	// Persist before the network call. Telegram has no idempotency key, so this
	// deliberately prefers at-most-once emergency delivery over a message storm.
	if err := lane.state.Save(state); err != nil {
		return err
	}
	_, err = lane.sender.Send(ctx, Alert{Kind: "OPEN", Incident: Incident{
		AlertRef: state.AlertRef, IncidentType: "PLATFORM_DATABASE_UNAVAILABLE", RootCause: RootPlatform,
		Severity: SeverityP1, Status: StatusOpen, SafeErrorCode: "DATABASE_UNAVAILABLE",
		OccurrenceCount: 1, AffectedCount: 1, FirstSeenAt: startedAt, LastSeenAt: now.UTC(), Version: 1,
	}}, lane.adminIncidentURL)
	if err == nil {
		return nil
	}
	// A known send failure is safe to retry on the next monitoring cycle. The
	// pre-send state above still prevents a duplicate if the process crashes
	// after Telegram accepted the message but before returning a response.
	state.RecoveryPending = false
	state.LastEmergencySentAt = nil
	if saveErr := lane.state.Save(state); saveErr != nil {
		return saveErr
	}
	return err
}

func (lane *EmergencyLane) DatabaseRecovered(ctx context.Context, reconciler DatabaseIncidentReconciler, now time.Time) error {
	state, err := lane.state.Load()
	if err != nil || !state.RecoveryPending {
		return err
	}
	if state.DatabaseFailureStartedAt == nil {
		return errors.New("Sentinel emergency recovery has no failure start")
	}
	incident, err := reconciler.ReconcileDatabaseIncident(ctx, state.AlertRef, *state.DatabaseFailureStartedAt, now.UTC())
	if err != nil {
		return err
	}
	if _, err := lane.sender.Send(ctx, Alert{Kind: "RECOVERY", Incident: incident}, lane.adminIncidentURL); err != nil {
		return err
	}
	return lane.state.Save(EmergencyState{})
}
