package sentinel

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestScheduledNotificationPolicyExcludesHistoricalAndTestRuns(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	for _, trigger := range []TriggerKind{TriggerUnknown, TriggerTest} {
		if observation := NotificationObservation(uuid.New(), uuid.New(), trigger, "FAILED", "REPORT_SET_INCOMPLETE", now); observation != nil {
			t.Fatalf("trigger %s produced an observation: %+v", trigger, observation)
		}
	}
	observation := NotificationObservation(uuid.New(), uuid.New(), TriggerScheduled, "FAILED", "REPORT_SET_INCOMPLETE", now)
	if observation == nil || observation.Severity != SeverityP1 || observation.RootCause != RootReportData {
		t.Fatalf("scheduled failure observation = %+v", observation)
	}
}

func TestScheduledReportFailurePolicy(t *testing.T) {
	now := time.Date(2026, 7, 16, 8, 0, 0, 0, time.UTC)
	if observation := ReportObservation(uuid.New(), uuid.New(), "SUCCEEDED", "", now); observation != nil {
		t.Fatalf("successful report produced an observation: %+v", observation)
	}
	observation := ReportObservation(uuid.New(), uuid.New(), "FAILED", "SML_TIMEOUT", now)
	if observation == nil || observation.Severity != SeverityP1 || observation.RootCause != RootSMLConnectivity || observation.SourceKind != SourceReport {
		t.Fatalf("scheduled report failure observation = %+v", observation)
	}
}

func TestObservationFingerprintAggregatesSameRootCauseAcrossTenants(t *testing.T) {
	first := NotificationObservation(uuid.New(), uuid.New(), TriggerScheduled, "FAILED", "SML_UNREACHABLE", time.Now())
	second := NotificationObservation(uuid.New(), uuid.New(), TriggerScheduled, "FAILED", "SML_TIMEOUT", time.Now())
	if first == nil || second == nil || first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("same safe root cause was not aggregated: %v != %v", first, second)
	}
	different := NotificationObservation(uuid.New(), uuid.New(), TriggerScheduled, "FAILED", "REPORT_OUTPUT_INVALID", time.Now())
	if different == nil || different.Fingerprint() == first.Fingerprint() {
		t.Fatalf("different root causes shared a fingerprint: %v == %v", different, first)
	}
}

func TestDownstreamNotificationUsesProvenReportRootFingerprint(t *testing.T) {
	reportFailure := ReportObservation(uuid.New(), uuid.New(), "FAILED", "SML_UNREACHABLE", time.Now())
	notification := NotificationObservation(uuid.New(), uuid.New(), TriggerScheduled, "FAILED", "REPORT_SET_INCOMPLETE", time.Now())
	if reportFailure == nil || notification == nil {
		t.Fatal("expected report and notification observations")
	}
	notification.Downstream = true
	notification.RootCause = reportFailure.RootCause
	if notification.Fingerprint() != reportFailure.Fingerprint() {
		t.Fatalf("downstream fingerprint %s does not match root %s", notification.Fingerprint(), reportFailure.Fingerprint())
	}
}

func TestTelegramMessageContainsOnlySafeOperationalContext(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", IncidentType: "SCHEDULED_NOTIFICATION_FAILED", RootCause: RootSMLConnectivity,
		Severity: SeverityP1, Status: StatusOpen, SafeErrorCode: "SML_UNREACHABLE", AffectedCount: 100,
		FirstSeenAt: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC), LastSeenAt: time.Date(2026, 7, 16, 1, 0, 30, 0, time.UTC),
	}
	message := TelegramMessage(Alert{Kind: "OPEN", Incident: incident}, "https://dashboard.nextstep-soft.com/admin/operational-incidents")
	for _, required := range []string{"NST-ABC123DEF456", "ติดต่อ Java Web Service", "100", "เวลาไทย", "https://dashboard.nextstep-soft.com/admin/operational-incidents"} {
		if !strings.Contains(message, required) {
			t.Fatalf("message %q does not contain %q", message, required)
		}
	}
	for _, forbidden := range []string{"tenantId", "recipient", "SQL", "payload"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("message %q leaked forbidden text %q", message, forbidden)
		}
	}
}

func TestTelegramMessageRejectsUnsafeErrorCode(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", IncidentType: "PLATFORM_FAILURE", RootCause: RootPlatform,
		Severity: SeverityP1, Status: StatusOpen, SafeErrorCode: "SAFE\ncustomer-data", OccurrenceCount: 1, AffectedCount: 1,
		FirstSeenAt: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC),
	}
	message := TelegramMessage(Alert{Kind: "OPEN", Incident: incident}, "https://dashboard.nextstep-soft.com/admin/operational-incidents")
	if strings.Contains(message, "customer-data") || !strings.Contains(message, "UNKNOWN") {
		t.Fatalf("unsafe error code reached Telegram message: %q", message)
	}
}

func TestAggregatedSMLIncidentKeepsThaiJavaWSCause(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "MULTIPLE_SAFE_ERRORS", FirstSeenAt: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC),
		OccurrenceCount: 2, AffectedCount: 2,
	}
	message := TelegramMessage(Alert{Kind: "OPEN", Incident: incident}, "https://example.test/incidents")
	if !strings.Contains(message, "ติดต่อ Java Web Service") || strings.Contains(message, "ระบบไม่สามารถดำเนินงานนี้ได้") {
		t.Fatalf("aggregated SML message lost its root cause: %q", message)
	}
}

func TestTelegramLifecycleUsesDistinctThaiHeadings(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "SML_UNREACHABLE", FirstSeenAt: time.Date(2026, 7, 16, 11, 0, 0, 0, time.UTC),
		OccurrenceCount: 1, AffectedCount: 1,
	}
	reminder := TelegramMessage(Alert{Kind: "REMINDER", Incident: incident}, "https://example.test/incidents")
	recovery := TelegramMessage(Alert{Kind: "RECOVERY", Incident: incident}, "https://example.test/incidents")
	if !strings.HasPrefix(reminder, "แจ้งเตือนซ้ำ · ปัญหายังไม่หาย") || !strings.Contains(recovery, "ยืนยันว่าระบบฟื้นตัวแล้ว") || reminder == recovery {
		t.Fatalf("reminder=%q recovery=%q", reminder, recovery)
	}
}

func TestParseModeFailsClosed(t *testing.T) {
	for value, expected := range map[string]Mode{"": ModeOff, "off": ModeOff, "observe": ModeObserve, "send": ModeSend} {
		actual, err := ParseMode(value)
		if err != nil || actual != expected {
			t.Fatalf("ParseMode(%q) = %q, %v", value, actual, err)
		}
	}
	if _, err := ParseMode("enabled"); err == nil {
		t.Fatal("unknown mode was accepted")
	}
}
