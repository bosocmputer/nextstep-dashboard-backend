package sentinel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type monitorStoreStub struct {
	observations    []Observation
	maintenance     bool
	recordErr       error
	recorded        bool
	enqueued        bool
	advanced        bool
	lifecycle       bool
	lifecycleOutbox bool
	claimed         bool
}

type monitorSenderStub struct{}

func (*monitorSenderStub) Send(context.Context, Alert, string) (string, error) { return "test", nil }

func (store *monitorStoreStub) ScanObservations(context.Context, time.Time, int, time.Duration) ([]Observation, error) {
	return store.observations, nil
}
func (store *monitorStoreStub) RecordObservations(_ context.Context, _ []Observation, _ time.Time, _ time.Duration, enqueue bool) error {
	store.recorded = true
	store.enqueued = enqueue
	return store.recordErr
}
func (store *monitorStoreStub) AdvanceObservationCursors(context.Context, time.Time) error {
	store.advanced = true
	return nil
}
func (store *monitorStoreStub) AdvanceLifecycle(_ context.Context, _ []Observation, _ bool, _ time.Time, enqueue bool) error {
	store.lifecycle = true
	store.lifecycleOutbox = enqueue
	return nil
}
func (store *monitorStoreStub) MaintenanceActive(context.Context, time.Time) (bool, error) {
	return store.maintenance, nil
}

func (store *monitorStoreStub) ClaimAlert(context.Context, string, time.Duration, time.Time) (Alert, error) {
	store.claimed = true
	return Alert{}, ErrNoAlertReady
}
func (*monitorStoreStub) CompleteAlert(context.Context, uuid.UUID, string, time.Time) error {
	return nil
}
func (*monitorStoreStub) RetryAlert(context.Context, uuid.UUID, string, string, time.Time, time.Time, bool) error {
	return nil
}

func TestMonitorAdvancesCursorOnlyAfterDurableObservationWrite(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	store := &monitorStoreStub{}
	monitor := NewMonitor(store, nil, ModeObserve, "test", "https://example.test/admin/operational-incidents", func() time.Time { return now })
	if err := monitor.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.recorded || !store.advanced || !store.lifecycle {
		t.Fatalf("recorded=%v advanced=%v lifecycle=%v", store.recorded, store.advanced, store.lifecycle)
	}

	store = &monitorStoreStub{recordErr: errors.New("write unavailable")}
	monitor = NewMonitor(store, nil, ModeObserve, "test", "https://example.test/admin/operational-incidents", func() time.Time { return now })
	if err := monitor.Process(context.Background()); err == nil {
		t.Fatal("observation write failure was ignored")
	}
	if store.advanced || store.lifecycle {
		t.Fatalf("cursor/lifecycle advanced after failed write: advanced=%v lifecycle=%v", store.advanced, store.lifecycle)
	}
}

func TestMaintenanceRecordsPendingOutboxButSuppressesSender(t *testing.T) {
	now := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	store := &monitorStoreStub{maintenance: true, observations: []Observation{{
		IncidentType: "WORKER_HEARTBEAT_MISSING", RootCause: RootPlatform, Severity: SeverityP1,
		SourceKind: SourceWorker, SourceID: uuid.New(), SafeErrorCode: "WORKER_HEARTBEAT_STALE", ObservedAt: now,
	}}}
	monitor := NewMonitor(store, &monitorSenderStub{}, ModeSend, "test", "https://example.test/admin/operational-incidents", func() time.Time { return now })
	if err := monitor.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.recorded || !store.advanced || !store.lifecycle {
		t.Fatalf("maintenance suppressed durable observation: %+v", store)
	}
	if !store.enqueued || !store.lifecycleOutbox {
		t.Fatalf("maintenance discarded pending alert intent: %+v", store)
	}
	if store.claimed {
		t.Fatalf("maintenance allowed a provider claim: %+v", store)
	}
}

func TestAcceptedRiskReasonRejectsPotentialCustomerOrSecretData(t *testing.T) {
	for _, reason := range []string{
		"ติดต่อ https://customer.example เพื่อตรวจสอบ",
		"ปิดร้าน 11111111-1111-4111-8111-111111111111 ชั่วคราว",
		"ติดต่อหมายเลข 0812345678 แล้ว",
	} {
		if validOperatorReason(reason) {
			t.Fatalf("unsafe reason %q was not detected", reason)
		}
	}
	if !validOperatorReason("ปิดบริการนี้ถาวรตามนโยบายปฏิบัติการ") {
		t.Fatal("safe operational reason was rejected")
	}
}

func TestSanitizeConnectionReferenceRemovesSecretsAndClassifiesHTTP(t *testing.T) {
	reference := sanitizeConnectionReference(SMLConnectionReference{
		EndpointURLAtFailure: "http://user:password@example.test:8092/service?token=secret#fragment",
		CurrentEndpointURL:   "https://example.test/current?ignored=yes",
		Status:               ConnectionChanged,
	})
	if reference.EndpointURLAtFailure != "http://example.test:8092/service" || reference.CurrentEndpointURL != "https://example.test/current" {
		t.Fatalf("unsafe URL was not sanitized: %+v", reference)
	}
	if reference.EndpointHost != "example.test" || reference.SchemeSecurity != SchemeHTTP {
		t.Fatalf("reference classification = %+v", reference)
	}
}

func TestSanitizeConnectionReferenceFallsBackToCurrentWithoutGuessing(t *testing.T) {
	reference := sanitizeConnectionReference(SMLConnectionReference{EndpointURLAtFailure: "ftp://unsafe.test/file", CurrentEndpointURL: "https://current.test", Status: ConnectionChanged})
	if reference.Status != ConnectionCurrentOnly || reference.EndpointURLAtFailure != "" {
		t.Fatalf("reference = %+v", reference)
	}
}
