package sentinel

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type emergencySender struct{ incidents []Incident }

func (sender *emergencySender) Send(_ context.Context, alert Alert, _ string) (string, error) {
	sender.incidents = append(sender.incidents, alert.Incident)
	return "1", nil
}

type failingEmergencySender struct{ calls int }

func (sender *failingEmergencySender) Send(_ context.Context, _ Alert, _ string) (string, error) {
	sender.calls++
	if sender.calls == 1 {
		return "", &SendError{Code: "TELEGRAM_NETWORK_ERROR"}
	}
	return "2", nil
}

type emergencyReconciler struct{ called int }

func (reconciler *emergencyReconciler) ReconcileDatabaseIncident(_ context.Context, alertRef string, startedAt, recoveredAt time.Time) (Incident, error) {
	reconciler.called++
	return Incident{AlertRef: alertRef, IncidentType: "PLATFORM_DATABASE_UNAVAILABLE", RootCause: RootPlatform, Severity: SeverityP1, Status: StatusResolved, SafeErrorCode: "DATABASE_UNAVAILABLE", FirstSeenAt: startedAt, LastSeenAt: recoveredAt, OccurrenceCount: 1, AffectedCount: 1}, nil
}

func TestEmergencyLaneSendsOnceAndRequiresEvidenceForRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sender := &emergencySender{}
	lane := NewEmergencyLane(NewEmergencyStateStore(path), sender, "https://example.test/admin/operational-incidents")
	startedAt := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	if err := lane.DatabaseUnavailable(context.Background(), startedAt); err != nil {
		t.Fatal(err)
	}
	if err := lane.DatabaseUnavailable(context.Background(), startedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(sender.incidents) != 1 || sender.incidents[0].Status != StatusOpen {
		t.Fatalf("unavailable incidents = %+v", sender.incidents)
	}
	reconciler := &emergencyReconciler{}
	if err := lane.DatabaseRecovered(context.Background(), reconciler, startedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if reconciler.called != 1 || len(sender.incidents) != 2 || sender.incidents[1].Status != StatusResolved {
		t.Fatalf("recovery = reconciled %d, incidents %+v", reconciler.called, sender.incidents)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o", info.Mode().Perm())
	}
}

func TestEmergencyStateRejectsOversizedOrUnknownData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewEmergencyStateStore(path)
	if _, err := store.Load(); err == nil {
		t.Fatal("unknown state field was accepted")
	}
	if err := os.WriteFile(path, make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("oversized state was accepted")
	}
}

func TestEmergencyLaneRetriesKnownSendFailureWithoutChangingAlertReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sender := &failingEmergencySender{}
	lane := NewEmergencyLane(NewEmergencyStateStore(path), sender, "https://example.test/admin/operational-incidents")
	now := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	if err := lane.DatabaseUnavailable(context.Background(), now); err == nil {
		t.Fatal("first Telegram failure was not returned")
	}
	first, err := lane.state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if first.RecoveryPending || first.AlertRef == "" {
		t.Fatalf("failed attempt state = %+v", first)
	}
	if err := lane.DatabaseUnavailable(context.Background(), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, err := lane.state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !second.RecoveryPending || second.AlertRef != first.AlertRef || sender.calls != 2 {
		t.Fatalf("retry state=%+v first=%+v calls=%d", second, first, sender.calls)
	}
}
