package sentinel

import (
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
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

func TestContinuousObservationFingerprintSeparatesConditionAndResource(t *testing.T) {
	first := Observation{
		RootCause: RootCapacity, Severity: SeverityP2, SafeErrorCode: "HOST_DISK_WARNING",
		SourceKind: SourceHost, SourceID: stableSentinelID("disk:/"), ObservationMode: ObservationContinuous,
		SubjectType: SubjectHostResource, SubjectKey: "disk:/",
	}
	otherCondition := first
	otherCondition.SafeErrorCode = "HOST_MEMORY_WARNING"
	otherResource := first
	otherResource.SubjectKey = "disk:/data"
	if first.Fingerprint() == otherCondition.Fingerprint() {
		t.Fatal("continuous disk and memory conditions shared a family fingerprint")
	}
	if first.Fingerprint() == otherResource.Fingerprint() {
		t.Fatal("continuous conditions on different resources shared a family fingerprint")
	}
}

func TestTenantSubjectKeyDoesNotExposeTenantIdentifier(t *testing.T) {
	tenantID := uuid.MustParse("4a06e1c2-29cd-4b5a-81d4-b2a26c2e11ec")
	observation := ReportObservation(uuid.New(), tenantID, "FAILED", "SML_UNREACHABLE", time.Now())
	if observation == nil {
		t.Fatal("expected report observation")
	}
	if observation.SubjectType != SubjectTenant || observation.ObservationMode != ObservationDiscrete {
		t.Fatalf("subject metadata = %+v", observation)
	}
	if strings.Contains(observation.SubjectKey, tenantID.String()) || len(observation.SubjectKey) != 64 {
		t.Fatalf("subject key is not a safe hash: %q", observation.SubjectKey)
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
	for _, required := range []string{"NST-ABC123DEF456", "เชื่อมต่อ Java Web Service ไม่สำเร็จ", "เวลาไทย", "https://dashboard.nextstep-soft.com/admin/operational-incidents"} {
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

func TestTelegramMessageIncludesSanitizedTenantAndHistoricalJavaWSURLWhenExplicitlyAllowed(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", IncidentType: "SCHEDULED_REPORT_FAILED", RootCause: RootSMLConnectivity,
		Severity: SeverityP1, Status: StatusOpen, SafeErrorCode: "SML_UNREACHABLE", AffectedCount: 1, ActiveAffectedCount: 1,
		SubjectType: SubjectTenant, FirstSeenAt: time.Date(2026, 7, 19, 1, 0, 3, 0, time.UTC),
	}
	alert := Alert{Kind: "OPEN", Incident: incident, TenantContexts: []TelegramTenantContext{{
		TenantName: " ร้าน\nขอนแก่น ", EndpointURL: "http://user:secret@khonkaen.3bbddns.com:11680/path?token=secret#fragment",
		URLStatus: TelegramURLAtFailure,
	}}}
	message, result := telegramMessage(alert, "https://example.test/incidents", true)
	for _, required := range []string{"ร้าน: ร้าน ขอนแก่น", "Java Web Service Base URL ตอนเกิดเหตุ:", "http://khonkaen.3bbddns.com:11680/path"} {
		if !strings.Contains(message, required) {
			t.Fatalf("message %q does not contain %q", message, required)
		}
	}
	for _, forbidden := range []string{"user:secret", "token=secret", "#fragment"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("message %q contains unsafe value %q", message, forbidden)
		}
	}
	if result != TelegramContextIncluded {
		t.Fatalf("context result = %q", result)
	}
}

func TestTelegramMessageLabelsCurrentOnlyAndChangedConnectionWithoutGuessing(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "SML_NOT_READY", SubjectType: SubjectTenant, AffectedCount: 2, ActiveAffectedCount: 2,
		FirstSeenAt: time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC),
	}
	alert := Alert{Kind: "OPEN", Incident: incident, TenantContexts: []TelegramTenantContext{
		{TenantName: "ร้านปัจจุบัน", EndpointURL: "https://current.example.test", URLStatus: TelegramURLCurrentOnly},
		{TenantName: "ร้านเปลี่ยนค่า", EndpointURL: "http://old.example.test:8092", URLStatus: TelegramURLChangedSinceFailure},
	}}
	message, _ := telegramMessage(alert, "https://example.test/incidents", true)
	for _, required := range []string{"Java Web Service Base URL ปัจจุบัน:", "Java Web Service Base URL ตอนเกิดเหตุ:", "การตั้งค่าถูกเปลี่ยนหลังเกิดเหตุ"} {
		if !strings.Contains(message, required) {
			t.Fatalf("message %q does not contain %q", message, required)
		}
	}
}

func TestTelegramMessageBoundsAggregatedTenantContextWithoutCuttingURLs(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "SML_UNREACHABLE", SubjectType: SubjectTenant, AffectedCount: 100, ActiveAffectedCount: 100,
		FirstSeenAt: time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC),
	}
	contexts := make([]TelegramTenantContext, 0, 5)
	for index := 1; index <= 5; index++ {
		contexts = append(contexts, TelegramTenantContext{TenantName: "ร้านทดสอบ", EndpointURL: "http://shop.example.test:8092", URLStatus: TelegramURLAtFailure})
	}
	message, result := telegramMessage(Alert{Kind: "OPEN", Incident: incident, TenantContexts: contexts, AdditionalTenantCount: 95}, "https://example.test/incidents", true)
	if strings.Count(message, "ร้าน: ร้านทดสอบ") != 5 || !strings.Contains(message, "และอีก 95 ร้าน") || len(message) >= 3500 {
		t.Fatalf("bounded message = %q (len=%d)", message, len(message))
	}
	if result != TelegramContextIncluded {
		t.Fatalf("context result = %q", result)
	}
}

func TestTelegramMessageOmitsWholeURLWhenMessageBudgetIsExceeded(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "SML_UNREACHABLE", SubjectType: SubjectTenant, AffectedCount: 2, ActiveAffectedCount: 2,
		FirstSeenAt: time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC),
	}
	firstURL := "http://first.example.test/" + strings.Repeat("a", 1800)
	secondURL := "http://second.example.test/" + strings.Repeat("b", 1800)
	alert := Alert{Kind: "OPEN", Incident: incident, TenantContexts: []TelegramTenantContext{
		{TenantName: "ร้านแรก", EndpointURL: firstURL, URLStatus: TelegramURLAtFailure},
		{TenantName: "ร้านสอง", EndpointURL: secondURL, URLStatus: TelegramURLAtFailure},
	}}
	message, result := telegramMessage(alert, "https://example.test/incidents", true)
	if !strings.Contains(message, firstURL) || strings.Contains(message, secondURL) || !strings.Contains(message, "และอีก 1 ร้าน") {
		t.Fatalf("message did not preserve complete URLs: %q", message)
	}
	if result != TelegramContextMessageBudgetExceeded || len(message) >= 3500 {
		t.Fatalf("result=%q len=%d", result, len(message))
	}
}

func TestTelegramMessageOmitsTenantContextUnlessExplicitlyAllowed(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "SML_UNREACHABLE", SubjectType: SubjectTenant, AffectedCount: 1,
		FirstSeenAt: time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC),
	}
	alert := Alert{Kind: "OPEN", Incident: incident, TenantContexts: []TelegramTenantContext{{TenantName: "ร้านลับ", EndpointURL: "http://secret.example.test", URLStatus: TelegramURLAtFailure}}}
	message := TelegramMessage(alert, "https://example.test/incidents")
	if strings.Contains(message, "ร้านลับ") || strings.Contains(message, "secret.example.test") {
		t.Fatalf("default renderer disclosed tenant context: %q", message)
	}
}

func TestTelegramMessageNeverIncludesTenantContextOutsideP1(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP2, Status: StatusOpen,
		SafeErrorCode: "SML_UNREACHABLE", SubjectType: SubjectTenant, AffectedCount: 1,
		FirstSeenAt: time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC),
	}
	alert := Alert{Kind: "OPEN", Incident: incident, TenantContexts: []TelegramTenantContext{{TenantName: "ร้านลับ", EndpointURL: "http://secret.example.test", URLStatus: TelegramURLAtFailure}}}
	message, result := telegramMessage(alert, "https://example.test/incidents", true)
	if strings.Contains(message, "ร้านลับ") || strings.Contains(message, "secret.example.test") || result != TelegramContextNotTenantScoped {
		t.Fatalf("non-P1 context result=%q message=%q", result, message)
	}
}

func TestTelegramMessageFallsBackSafelyWhenTenantContextQueryFailed(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "SML_UNREACHABLE", SubjectType: SubjectTenant, AffectedCount: 1,
		FirstSeenAt: time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC),
	}
	message, result := telegramMessage(Alert{Kind: "OPEN", Incident: incident, TenantContextResult: TelegramContextQueryFailed}, "https://example.test/incidents", true)
	if result != TelegramContextQueryFailed || !strings.Contains(message, "NST-ABC123DEF456") || strings.Contains(message, "Java Web Service Base URL") {
		t.Fatalf("query failure result=%q message=%q", result, message)
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
	if !strings.Contains(message, "เชื่อมต่อ Java Web Service ไม่สำเร็จ") || strings.Contains(message, "ระบบไม่สามารถดำเนินงานนี้ได้") {
		t.Fatalf("aggregated SML message lost its root cause: %q", message)
	}
}

func TestTelegramMixedSMLCauseKeepsCompactConnectionState(t *testing.T) {
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "MULTIPLE_SAFE_ERRORS", FirstSeenAt: time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC),
		AffectedCount: 2, ActiveAffectedCount: 2,
		CauseBreakdown: []CauseBreakdown{
			{TransportPhase: "BEFORE_REQUEST_SENT", AffectedCount: 1},
			{TransportPhase: "REQUEST_SENT_RESULT_UNKNOWN", AffectedCount: 1},
		},
	}
	message := TelegramMessage(Alert{Kind: "OPEN", Incident: incident}, "https://example.test/incidents")
	if !strings.HasPrefix(message, "🔴 เชื่อมต่อ Java Web Service ไม่สำเร็จ") {
		t.Fatalf("message did not keep the compact connection state: %q", message)
	}
	for _, redundant := range []string{"พบปัญหา Java Web Service 2 รูปแบบ", "ก่อนส่งคำขอ", "ไม่ได้รับคำตอบภายในเวลา", "MULTIPLE_SAFE_ERRORS"} {
		if strings.Contains(message, redundant) {
			t.Fatalf("message %q contains redundant detail %q", message, redundant)
		}
	}
	if len(message) >= 3500 {
		t.Fatalf("mixed message exposed implementation code or exceeded budget: %q", message)
	}
}

func TestTelegramInvalidZIPShowsOnlyBoundedProtocolFacts(t *testing.T) {
	status := 200
	soap, base64OK, zipOK := true, true, false
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1, Status: StatusOpen,
		SafeErrorCode: "SML_ZIP_FORMAT_INVALID", FirstSeenAt: time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC),
		AffectedCount: 1, ActiveAffectedCount: 1, SubjectType: SubjectTenant,
	}
	message := TelegramMessage(Alert{Kind: "OPEN", Incident: incident, ProtocolEvidence: &failure.JavaWSProtocolEvidence{
		RequestRef: "NXR-ABCDEFGHIJKLMNOP", RequestCount: 1, RetryCount: 0, HTTPStatus: &status,
		SOAPValid: &soap, Base64Valid: &base64OK, ZIPSignatureValid: &zipOK,
	}}, "https://example.test/incidents")
	for _, expected := range []string{"คำตอบจาก Java Web Service ไม่สมบูรณ์", "การเชื่อมต่อ: สำเร็จ", "คำขอ: 1 ครั้ง · ไม่มี Retry", "คำตอบ: HTTP 200 แต่ข้อมูลไม่ใช่ ZIP"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("message %q does not contain %q", message, expected)
		}
	}
	for _, forbidden := range []string{"NXR-ABCDEFGHIJKLMNOP", "responseSha256", "SOAPValid"} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("message %q leaks %q", message, forbidden)
		}
	}
}

func TestTelegramJavaWSLifecycleUsesConnectionStateLanguage(t *testing.T) {
	resolvedAt := time.Date(2026, 7, 20, 10, 0, 25, 0, time.UTC)
	incident := Incident{
		AlertRef: "NST-ABC123DEF456", RootCause: RootSMLConnectivity, Severity: SeverityP1,
		Status: StatusResolved, SafeErrorCode: "SML_UNREACHABLE", SubjectType: SubjectTenant,
		FirstSeenAt: time.Date(2026, 7, 19, 10, 0, 3, 0, time.UTC), ResolvedAt: &resolvedAt,
		OccurrenceCount: 1, AffectedCount: 1,
	}
	context := TelegramTenantContext{
		TenantName: "ร้านนครการเกษตร", EndpointURL: "http://nkkankaset.example.test:8080/SMLJavaWebService/DotNetFrameWork",
		URLStatus: TelegramURLAtFailure,
	}
	reminderIncident := incident
	reminderIncident.Status = StatusOpen
	reminderIncident.ResolvedAt = nil
	reminder, _ := telegramMessage(Alert{Kind: "REMINDER", Incident: reminderIncident, TenantContexts: []TelegramTenantContext{context}}, "https://example.test/incidents", true)
	recovery, result := telegramMessage(Alert{Kind: "RECOVERY", Incident: incident, TenantContexts: []TelegramTenantContext{context}}, "https://example.test/incidents", true)

	if !strings.HasPrefix(reminder, "🟠 ยังเชื่อมต่อ Java Web Service ไม่ได้") {
		t.Fatalf("reminder=%q", reminder)
	}
	for _, required := range []string{
		"✅ เชื่อมต่อ Java Web Service ได้แล้ว",
		"ร้าน: ร้านนครการเกษตร",
		"Java Web Service Base URL ตอนเกิดเหตุ:",
		"http://nkkankaset.example.test:8080/SMLJavaWebService/DotNetFrameWork",
		"ตรวจสอบล่าสุด: 20/07/2026 17:00:25 น. เวลาไทย",
		"อ้างอิง: NST-ABC123DEF456",
	} {
		if !strings.Contains(recovery, required) {
			t.Fatalf("recovery %q does not contain %q", recovery, required)
		}
	}
	for _, forbidden := range []string{
		"ยืนยันว่าระบบฟื้นตัวแล้ว", "สาเหตุ:", "ผลกระทบ:", "ส่วนที่ได้รับผล:",
		"พบครั้งแรก:", "ข้อมูลเทคนิค:", "ตรวจสอบ: https://",
	} {
		if strings.Contains(recovery, forbidden) {
			t.Fatalf("recovery %q contains redundant text %q", recovery, forbidden)
		}
	}
	if result != TelegramContextIncluded {
		t.Fatalf("recovery context result = %q", result)
	}
}

func TestIncidentStatusPresentationUsesResolvedJavaWSState(t *testing.T) {
	resolvedAt := time.Date(2026, 7, 20, 10, 0, 25, 0, time.UTC)
	presentation := incidentStatusPresentation(Incident{
		RootCause: RootSMLConnectivity, Status: StatusResolved, ResolvedAt: &resolvedAt,
	})
	if presentation.State != LifecycleConnectionRestored || presentation.HeadlineTH != "เชื่อมต่อ Java Web Service ได้แล้ว" ||
		presentation.StatusSummaryTH != "ไม่ต้องดำเนินการ" || presentation.ActionRequired || presentation.VerifiedAt == nil || !presentation.VerifiedAt.Equal(resolvedAt) {
		t.Fatalf("presentation = %+v", presentation)
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
