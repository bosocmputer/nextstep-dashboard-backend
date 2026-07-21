package sentinel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/google/uuid"
)

type diagnosisAdminStore struct{ record DiagnosisRecord }

func (store diagnosisAdminStore) ListIncidents(context.Context, IncidentFilter) (IncidentPage, error) {
	return IncidentPage{}, nil
}
func (store diagnosisAdminStore) GetIncident(context.Context, uuid.UUID) (IncidentDetail, error) {
	return IncidentDetail{}, nil
}
func (store diagnosisAdminStore) ListIncidentOccurrences(context.Context, uuid.UUID, OccurrenceFilter) (OccurrencePage, error) {
	return OccurrencePage{}, nil
}
func (store diagnosisAdminStore) GetOccurrenceDiagnosis(context.Context, uuid.UUID, uuid.UUID) (DiagnosisRecord, error) {
	return store.record, nil
}
func (store diagnosisAdminStore) AcknowledgeIncident(context.Context, uuid.UUID, int, time.Time) (Incident, error) {
	return Incident{}, nil
}
func (store diagnosisAdminStore) AcceptIncidentRisk(context.Context, uuid.UUID, int, string, time.Time) (Incident, error) {
	return Incident{}, nil
}

func TestDiagnosisBuildsCustomerMessageFromConfirmedBoundedEvidence(t *testing.T) {
	status := 200
	soap, base64OK, zipOK := true, true, false
	duration := int64(64)
	service := NewAdminService(diagnosisAdminStore{record: DiagnosisRecord{
		TenantName:          " ร้านทดสอบ\nจำกัด ",
		ConnectionReference: &SMLConnectionReference{EndpointURLAtFailure: "http://user:secret@example.test:8080/service?token=hidden", Status: ConnectionExactVersion},
		Evidence: failure.Complete(failure.Evidence{
			Version: 2, Level: failure.LevelConfirmed, Category: failure.CategoryJavaWSResponse, Stage: failure.StageDecodePayload,
			OccurredAt: time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC), DurationMS: &duration, SafeErrorCode: "SML_ZIP_FORMAT_INVALID",
			ProtocolEvidence: &failure.JavaWSProtocolEvidence{RequestRef: "NXR-ABCDEFGHIJKLMNOP", RequestCount: 1, RetryCount: 0, HTTPStatus: &status, SOAPValid: &soap, Base64Valid: &base64OK, ZIPSignatureValid: &zipOK, TenantConcurrentQueries: 1, HostConcurrentQueries: 1},
		}), Baseline: failure.Baseline{P50MS: 80, P90MS: 96, SampleCount: 8},
	}}, time.Now)
	diagnosis, err := service.Diagnosis(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.Assessment.LoadSignal != failure.LoadNoNextstepSignal || diagnosis.CustomerMessageTH == "" {
		t.Fatalf("diagnosis = %+v", diagnosis)
	}
	for _, unsafe := range []string{"user:secret", "token=hidden", "\nจำกัด"} {
		if strings.Contains(diagnosis.CustomerMessageTH, unsafe) {
			t.Fatalf("unsafe customer message = %q", diagnosis.CustomerMessageTH)
		}
	}
	if !strings.Contains(diagnosis.CustomerMessageTH, "Request Ref: NXR-ABCDEFGHIJKLMNOP") || !strings.Contains(diagnosis.CustomerMessageTH, "ไม่พบสัญญาณว่า Nextstep สร้างภาระผิดปกติ") {
		t.Fatalf("customer message = %q", diagnosis.CustomerMessageTH)
	}
}

func TestLegacyDiagnosisDoesNotCreateDefinitiveCustomerMessage(t *testing.T) {
	service := NewAdminService(diagnosisAdminStore{record: DiagnosisRecord{Evidence: failure.Evidence{Version: 0, Level: failure.LevelLegacyPartial, Category: failure.CategoryJavaWSResponse, Stage: failure.StageDecodePayload, OccurredAt: time.Now(), SafeErrorCode: "SML_ZIP_FORMAT_INVALID"}}}, time.Now)
	diagnosis, err := service.Diagnosis(context.Background(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if diagnosis.CustomerMessageTH != "" || diagnosis.Assessment.LoadSignal != failure.LoadInsufficientEvidence {
		t.Fatalf("legacy diagnosis = %+v", diagnosis)
	}
}
