package sentinel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/google/uuid"
)

var safeErrorCodePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_:-]{1,95}$`)

type Mode string

const (
	ModeOff     Mode = "off"
	ModeObserve Mode = "observe"
	ModeSend    Mode = "send"
)

func ParseMode(value string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(value))) {
	case "", ModeOff:
		return ModeOff, nil
	case ModeObserve:
		return ModeObserve, nil
	case ModeSend:
		return ModeSend, nil
	default:
		return "", errors.New("OPERATIONAL_ALERTS_MODE must be off, observe, or send")
	}
}

type TriggerKind string

const (
	TriggerUnknown   TriggerKind = "UNKNOWN"
	TriggerScheduled TriggerKind = "SCHEDULED"
	TriggerTest      TriggerKind = "TEST"
)

type Severity string

const (
	SeverityP1 Severity = "P1"
	SeverityP2 Severity = "P2"
)

type RootCause string

const (
	RootSMLConnectivity RootCause = "SML_CONNECTIVITY"
	RootReportData      RootCause = "REPORT_DATA"
	RootLineDelivery    RootCause = "LINE_DELIVERY"
	RootPlatform        RootCause = "PLATFORM"
	RootCapacity        RootCause = "CAPACITY"
)

type Status string

const (
	StatusOpen           Status = "OPEN"
	StatusAcknowledged   Status = "ACKNOWLEDGED"
	StatusResolved       Status = "RESOLVED"
	StatusClosedAccepted Status = "CLOSED_ACCEPTED"
)

type SourceKind string

const (
	SourceNotification SourceKind = "NOTIFICATION"
	SourceDelivery     SourceKind = "DELIVERY"
	SourceReport       SourceKind = "REPORT"
	SourceWorker       SourceKind = "WORKER"
	SourceSMLCircuit   SourceKind = "SML_CIRCUIT"
	SourceHost         SourceKind = "HOST"
	SourceBackup       SourceKind = "BACKUP"
	SourceDatabase     SourceKind = "DATABASE"
)

type ObservationMode string

const (
	ObservationDiscrete   ObservationMode = "DISCRETE"
	ObservationContinuous ObservationMode = "CONTINUOUS"
)

type SubjectType string

const (
	SubjectTenant       SubjectType = "TENANT"
	SubjectHostResource SubjectType = "HOST_RESOURCE"
	SubjectBackupPolicy SubjectType = "BACKUP_POLICY"
	SubjectDatabase     SubjectType = "DATABASE"
	SubjectContainer    SubjectType = "CONTAINER"
	SubjectLineProvider SubjectType = "LINE_PROVIDER"
)

type InvestigationScope string

const (
	ScopeCustomerSystem   InvestigationScope = "CUSTOMER_SYSTEM"
	ScopeNextstepPlatform InvestigationScope = "NEXTSTEP_PLATFORM"
	ScopeLineProvider     InvestigationScope = "LINE_PROVIDER"
	ScopeConfiguration    InvestigationScope = "CONFIGURATION"
	ScopeUnknown          InvestigationScope = "UNKNOWN"
)

type MeasurementKind string
type MeasurementUnit string

const (
	MeasurementDiskUsedPercent            MeasurementKind = "DISK_USED_PERCENT"
	MeasurementMemoryAvailablePercent     MeasurementKind = "MEMORY_AVAILABLE_PERCENT"
	MeasurementDatabaseConnectionsPercent MeasurementKind = "DATABASE_CONNECTIONS_PERCENT"
	MeasurementQueueAgeSeconds            MeasurementKind = "QUEUE_AGE_SECONDS"
	MeasurementPercent                    MeasurementUnit = "PERCENT"
	MeasurementSeconds                    MeasurementUnit = "SECONDS"
	MeasurementCount                      MeasurementUnit = "COUNT"
)

type Measurement struct {
	Kind      MeasurementKind `json:"kind"`
	Value     float64         `json:"value"`
	Threshold float64         `json:"threshold"`
	Unit      MeasurementUnit `json:"unit"`
}

type Observation struct {
	CursorKey       string
	IncidentType    string
	RootCause       RootCause
	Severity        Severity
	SourceKind      SourceKind
	SourceID        uuid.UUID
	TenantID        *uuid.UUID
	SafeErrorCode   string
	ObservedAt      time.Time
	CorrelationKey  string
	Downstream      bool
	Evidence        *failure.Evidence
	ReportKey       string
	TriggerKind     TriggerKind
	Impact          *failure.Impact
	ObservationMode ObservationMode
	SubjectType     SubjectType
	SubjectKey      string
	Measurement     *Measurement
}

func (observation Observation) Fingerprint() string {
	// The fingerprint deliberately groups by root cause and severity. A single
	// JavaWS or platform outage can surface through several safe error codes;
	// splitting those codes would create a Telegram storm during a broad outage.
	parts := []string{string(observation.RootCause), string(observation.Severity)}
	if observation.ObservationMode == ObservationContinuous {
		parts = append(parts, observation.SafeErrorCode, string(observation.SubjectType), observation.SubjectKey)
	}
	canonical := strings.Join(parts, "|")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func TenantSubjectKey(tenantID uuid.UUID) string {
	digest := sha256.Sum256([]byte("nextstep-incident-tenant:" + tenantID.String()))
	return hex.EncodeToString(digest[:])
}

func ResourceSubjectKey(subjectType SubjectType, resource string) string {
	digest := sha256.Sum256([]byte("nextstep-incident-resource:" + string(subjectType) + ":" + resource))
	return hex.EncodeToString(digest[:])
}

func OccurrenceCorrelationKey(occurrenceID uuid.UUID) string {
	digest := sha256.Sum256([]byte("nextstep-notification-occurrence:" + occurrenceID.String()))
	return hex.EncodeToString(digest[:])
}

type Incident struct {
	ID                  uuid.UUID                  `json:"id"`
	AlertRef            string                     `json:"alertRef"`
	IncidentType        string                     `json:"incidentType"`
	RootCause           RootCause                  `json:"rootCause"`
	Severity            Severity                   `json:"severity"`
	Status              Status                     `json:"status"`
	SafeErrorCode       string                     `json:"safeErrorCode,omitempty"`
	OccurrenceCount     int                        `json:"occurrenceCount"`
	AffectedCount       int                        `json:"affectedCount"`
	TenantExamples      []string                   `json:"tenantExamples,omitempty"`
	FirstSeenAt         time.Time                  `json:"firstSeenAt"`
	LastSeenAt          time.Time                  `json:"lastSeenAt"`
	AcknowledgedAt      *time.Time                 `json:"acknowledgedAt,omitempty"`
	ResolvedAt          *time.Time                 `json:"resolvedAt,omitempty"`
	AcceptedAt          *time.Time                 `json:"acceptedAt,omitempty"`
	AcceptedReason      string                     `json:"acceptedReason,omitempty"`
	Version             int                        `json:"version"`
	Presentation        failure.Presentation       `json:"presentation"`
	IsDownstream        bool                       `json:"isDownstream"`
	CausedByAlertRef    string                     `json:"causedByAlertRef,omitempty"`
	ObservationMode     ObservationMode            `json:"observationMode"`
	SubjectType         SubjectType                `json:"subjectType"`
	ActiveAffectedCount int                        `json:"activeAffectedCount"`
	CauseBreakdown      []CauseBreakdown           `json:"causeBreakdown,omitempty"`
	Measurement         *Measurement               `json:"measurement,omitempty"`
	StatusPresentation  IncidentStatusPresentation `json:"statusPresentation"`
}

type IncidentLifecycleState string

const (
	LifecycleConnectionFailed   IncidentLifecycleState = "CONNECTION_FAILED"
	LifecycleConnectionRestored IncidentLifecycleState = "CONNECTION_RESTORED"
	LifecycleActiveProblem      IncidentLifecycleState = "ACTIVE_PROBLEM"
	LifecycleResolved           IncidentLifecycleState = "RESOLVED"
	LifecycleAcceptedRisk       IncidentLifecycleState = "ACCEPTED_RISK"
)

type IncidentStatusPresentation struct {
	State           IncidentLifecycleState `json:"state"`
	HeadlineTH      string                 `json:"headlineTh"`
	StatusSummaryTH string                 `json:"statusSummaryTh"`
	VerifiedAt      *time.Time             `json:"verifiedAt,omitempty"`
	ActionRequired  bool                   `json:"actionRequired"`
}

type CauseBreakdown struct {
	Presentation        failure.Presentation   `json:"presentation"`
	Category            failure.Category       `json:"category,omitempty"`
	Stage               failure.Stage          `json:"stage,omitempty"`
	TransportPhase      failure.TransportPhase `json:"transportPhase,omitempty"`
	InvestigationScope  InvestigationScope     `json:"investigationScope"`
	SubjectType         SubjectType            `json:"subjectType"`
	OccurrenceCount     int                    `json:"occurrenceCount"`
	AffectedCount       int                    `json:"affectedCount"`
	ActiveAffectedCount int                    `json:"activeAffectedCount"`
	AffectedLabelTH     string                 `json:"affectedLabelTh"`
	FirstSeenAt         time.Time              `json:"firstSeenAt"`
	LastSeenAt          time.Time              `json:"lastSeenAt"`
}

func NewAlertReference() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate operational alert reference: %w", err)
	}
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return "NST-" + encoded[:12], nil
}

func NotificationObservation(sourceID, tenantID uuid.UUID, trigger TriggerKind, status, safeErrorCode string, observedAt time.Time) *Observation {
	if trigger != TriggerScheduled {
		return nil
	}
	if status != "FAILED" && status != "PARTIAL_FAILED" && status != "BLOCKED_QUOTA" {
		return nil
	}
	root := rootCauseFor(safeErrorCode)
	if status == "BLOCKED_QUOTA" {
		root = RootLineDelivery
	}
	return &Observation{
		IncidentType: "SCHEDULED_NOTIFICATION_" + status,
		RootCause:    root, Severity: SeverityP1, SourceKind: SourceNotification,
		SourceID: sourceID, TenantID: &tenantID, SafeErrorCode: safeErrorCode, ObservedAt: observedAt.UTC(),
		ObservationMode: ObservationDiscrete, SubjectType: SubjectTenant, SubjectKey: TenantSubjectKey(tenantID),
	}
}

func DeliveryObservation(sourceID, tenantID uuid.UUID, status, safeErrorCode string, observedAt time.Time) *Observation {
	if status != "FAILED_PERMANENT" {
		return nil
	}
	return &Observation{
		IncidentType: "LINE_DELIVERY_FAILED_PERMANENT", RootCause: RootLineDelivery,
		Severity: SeverityP1, SourceKind: SourceDelivery, SourceID: sourceID, TenantID: &tenantID,
		SafeErrorCode: safeErrorCode, ObservedAt: observedAt.UTC(),
		ObservationMode: ObservationDiscrete, SubjectType: SubjectLineProvider,
		SubjectKey: ResourceSubjectKey(SubjectLineProvider, tenantID.String()),
	}
}

func ReportObservation(sourceID, tenantID uuid.UUID, status, safeErrorCode string, observedAt time.Time) *Observation {
	if status != "FAILED" {
		return nil
	}
	return &Observation{
		IncidentType: "SCHEDULED_REPORT_FAILED", RootCause: rootCauseFor(safeErrorCode),
		Severity: SeverityP1, SourceKind: SourceReport, SourceID: sourceID, TenantID: &tenantID,
		SafeErrorCode: safeErrorCode, ObservedAt: observedAt.UTC(),
		ObservationMode: ObservationDiscrete, SubjectType: SubjectTenant, SubjectKey: TenantSubjectKey(tenantID),
	}
}

func rootCauseFor(safeErrorCode string) RootCause {
	upper := strings.ToUpper(safeErrorCode)
	switch {
	case strings.HasPrefix(upper, "SML_"), strings.Contains(upper, "NETWORK"), strings.Contains(upper, "TIMEOUT"):
		return RootSMLConnectivity
	case strings.HasPrefix(upper, "LINE_"), strings.Contains(upper, "QUOTA"), strings.Contains(upper, "DELIVERY"):
		return RootLineDelivery
	case strings.Contains(upper, "REPORT"), strings.Contains(upper, "OUTPUT"), strings.Contains(upper, "FLEX"):
		return RootReportData
	default:
		return RootPlatform
	}
}

func TelegramMessage(alert Alert, adminBaseURL string) string {
	message, _ := telegramMessage(alert, adminBaseURL, false)
	return message
}

func telegramMessage(alert Alert, adminBaseURL string, includeTenantContext bool) (string, TelegramContextResult) {
	incident := alert.Incident
	adminURL := strings.TrimRight(adminBaseURL, "/")
	if incident.ID != uuid.Nil {
		adminURL += "/" + incident.ID.String()
	}
	if incident.RootCause == RootSMLConnectivity {
		return telegramSMLMessage(alert, adminURL, includeTenantContext)
	}
	heading := "Nextstep Sentinel " + string(incident.Severity)
	switch alert.Kind {
	case "UPDATE":
		heading = "อัปเดตเหตุสำคัญ · ผลกระทบเพิ่มขึ้น"
	case "REMINDER":
		heading = "แจ้งเตือนซ้ำ · ปัญหายังไม่หาย"
	case "RECOVERY":
		heading = "Nextstep Sentinel · ยืนยันว่าระบบฟื้นตัวแล้ว"
	}
	presentation := incident.Presentation
	if presentation.TitleTH == "" {
		presentation = incidentPresentation(incident)
	}
	impact := telegramImpact(incident.SafeErrorCode, presentation)
	thaiTime := incident.FirstSeenAt.In(time.FixedZone("Asia/Bangkok", 7*60*60)).Format("02/01/2006 15:04:05")
	affectedCount := incident.ActiveAffectedCount
	if affectedCount == 0 {
		affectedCount = incident.AffectedCount
	}
	header := ""
	if incident.RootCause == RootSMLConnectivity && len(incident.CauseBreakdown) > 1 {
		causeLines := make([]string, 0, min(3, len(incident.CauseBreakdown)))
		for _, cause := range incident.CauseBreakdown {
			if len(causeLines) == 3 {
				break
			}
			label := cause.Presentation.TitleTH
			switch cause.TransportPhase {
			case failure.PhaseBeforeRequestSent:
				label = "เชื่อมต่อไม่สำเร็จก่อนส่งคำขอ"
			case failure.PhaseRequestSentResultUnknown:
				label = "ส่งคำขอแล้วแต่ไม่ได้รับคำตอบภายในเวลา"
			case failure.PhaseResponseStarted:
				label = "เริ่มรับคำตอบแล้วแต่ข้อมูลไม่ครบ"
			}
			count := cause.ActiveAffectedCount
			if count == 0 {
				count = cause.AffectedCount
			}
			causeLines = append(causeLines, fmt.Sprintf("• %s: %d ร้าน", label, count))
		}
		header = fmt.Sprintf("%s\nอ้างอิง: %s\nสาเหตุ: พบปัญหา Java Web Service %d รูปแบบ\n%s",
			heading, incident.AlertRef, len(incident.CauseBreakdown), strings.Join(causeLines, "\n"))
	} else {
		header = fmt.Sprintf("%s\nอ้างอิง: %s\nสาเหตุ: %s", heading, incident.AlertRef, presentation.TitleTH)
	}
	tail := fmt.Sprintf("ผลกระทบ: %s\nส่วนที่ได้รับผล: %d\nพบครั้งแรก: %s น. เวลาไทย", impact, affectedCount, thaiTime)
	if incident.SafeErrorCode != "MULTIPLE_SAFE_ERRORS" {
		tail += "\nข้อมูลเทคนิค: " + safeText(incident.SafeErrorCode)
	}
	tail += "\nตรวจสอบ: " + adminURL
	contextBudget := 3499 - len(header) - len(tail) - 4
	contextBlock, result := telegramTenantContextBlock(alert, includeTenantContext, contextBudget)
	message := header + "\n"
	if contextBlock != "" {
		message += "\n" + contextBlock + "\n"
	}
	message += "\n" + tail
	if len(message) > 3499 {
		message = truncateUTF8Bytes(message, 3499)
	}
	return message, result
}

func telegramSMLMessage(alert Alert, adminURL string, includeTenantContext bool) (string, TelegramContextResult) {
	incident := alert.Incident
	heading := "🔴 เชื่อมต่อ Java Web Service ไม่สำเร็จ"
	switch alert.Kind {
	case "UPDATE":
		heading = "🔴 ยังเชื่อมต่อ Java Web Service ไม่ได้ · พบร้านเพิ่ม"
	case "REMINDER":
		heading = "🟠 ยังเชื่อมต่อ Java Web Service ไม่ได้"
	case "RECOVERY":
		heading = "✅ เชื่อมต่อ Java Web Service ได้แล้ว"
	}

	contextBudget := 3499 - len(heading) - 800
	contextBlock, result := telegramTenantContextBlock(alert, includeTenantContext, contextBudget)
	parts := []string{heading}
	if contextBlock != "" {
		parts = append(parts, contextBlock)
	}
	if alert.Kind == "RECOVERY" {
		verifiedAt := incident.ResolvedAt
		if verifiedAt == nil && !incident.LastSeenAt.IsZero() {
			verifiedAt = &incident.LastSeenAt
		}
		if verifiedAt != nil {
			parts = append(parts, "ตรวจสอบล่าสุด: "+formatTelegramThaiTime(*verifiedAt))
		}
		parts = append(parts, "อ้างอิง: "+incident.AlertRef)
	} else {
		parts = append(parts,
			"ตรวจพบเมื่อ: "+formatTelegramThaiTime(incident.FirstSeenAt),
			"ผลกระทบ: "+telegramImpact(incident.SafeErrorCode, incidentPresentation(incident)),
			"อ้างอิง: "+incident.AlertRef,
			"ตรวจสอบ: "+adminURL,
		)
	}
	message := strings.Join(parts, "\n\n")
	if len(message) > 3499 {
		message = truncateUTF8Bytes(message, 3499)
	}
	return message, result
}

func formatTelegramThaiTime(value time.Time) string {
	return value.In(time.FixedZone("Asia/Bangkok", 7*60*60)).Format("02/01/2006 15:04:05") + " น. เวลาไทย"
}

func telegramTenantContextBlock(alert Alert, include bool, budget int) (string, TelegramContextResult) {
	if alert.Incident.SubjectType != SubjectTenant || alert.Incident.Severity != SeverityP1 {
		return "", TelegramContextNotTenantScoped
	}
	if !include {
		return "", TelegramContextNotTenantScoped
	}
	if alert.TenantContextResult == TelegramContextQueryFailed {
		return "", TelegramContextQueryFailed
	}
	if budget <= 0 {
		return "", TelegramContextMessageBudgetExceeded
	}
	blocks := make([]string, 0, min(5, len(alert.TenantContexts)))
	omitted := alert.AdditionalTenantCount
	budgetOmitted := false
	urlUnavailable := false
	includeURL := alert.Incident.RootCause == RootSMLConnectivity
	for index, tenantContext := range alert.TenantContexts {
		if index >= 5 {
			omitted++
			continue
		}
		name := sanitizeTelegramTenantName(tenantContext.TenantName)
		if name == "" {
			omitted++
			continue
		}
		lines := []string{"ร้าน: " + name}
		if includeURL {
			endpointURL := sanitizeEndpointURL(tenantContext.EndpointURL)
			if endpointURL == "" {
				lines = append(lines, "Java Web Service Base URL: ไม่พบ URL ที่ปลอดภัยสำหรับแสดง")
				urlUnavailable = true
			} else {
				switch tenantContext.URLStatus {
				case TelegramURLAtFailure, TelegramURLChangedSinceFailure:
					lines = append(lines, "Java Web Service Base URL ตอนเกิดเหตุ:", endpointURL)
				default:
					lines = append(lines, "Java Web Service Base URL ปัจจุบัน:", endpointURL)
				}
				if tenantContext.URLStatus == TelegramURLChangedSinceFailure {
					lines = append(lines, "การตั้งค่าถูกเปลี่ยนหลังเกิดเหตุ")
				}
			}
		}
		candidate := strings.Join(lines, "\n")
		joined := strings.Join(append(append([]string(nil), blocks...), candidate), "\n\n")
		if len(joined) > budget {
			omitted++
			budgetOmitted = true
			continue
		}
		blocks = append(blocks, candidate)
	}
	if omitted > 0 {
		remainder := fmt.Sprintf("และอีก %d ร้าน — เปิดรายละเอียดเพื่อดูทั้งหมด", omitted)
		joined := strings.Join(append(append([]string(nil), blocks...), remainder), "\n\n")
		if len(joined) <= budget {
			blocks = append(blocks, remainder)
		} else {
			return strings.Join(blocks, "\n\n"), TelegramContextMessageBudgetExceeded
		}
	}
	if len(blocks) == 0 {
		return "", TelegramContextURLUnavailable
	}
	if budgetOmitted {
		return strings.Join(blocks, "\n\n"), TelegramContextMessageBudgetExceeded
	}
	if urlUnavailable {
		return strings.Join(blocks, "\n\n"), TelegramContextURLUnavailable
	}
	return strings.Join(blocks, "\n\n"), TelegramContextIncluded
}

func truncateUTF8Bytes(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	for maximum > 0 && !utf8.ValidString(value[:maximum]) {
		maximum--
	}
	return value[:maximum]
}

func incidentPresentation(incident Incident) failure.Presentation {
	if strings.TrimSpace(incident.SafeErrorCode) != "MULTIPLE_SAFE_ERRORS" {
		return failure.PresentationFor(failure.EvidenceForCode(incident.SafeErrorCode))
	}
	evidence := failure.Evidence{Level: failure.LevelConfirmed}
	switch incident.RootCause {
	case RootSMLConnectivity:
		evidence.Category, evidence.Stage = failure.CategoryJavaWSConnectivity, failure.StageConnectJavaWS
	case RootReportData:
		evidence.Category, evidence.Stage = failure.CategoryReportProcessing, failure.StageBuildReport
	case RootLineDelivery:
		evidence.Category, evidence.Stage = failure.CategoryLineDelivery, failure.StageSendLINE
	case RootCapacity:
		evidence.Category, evidence.Stage = failure.CategoryCapacity, failure.StagePlatformCheck
	default:
		evidence.Category, evidence.Stage = failure.CategoryPlatform, failure.StagePlatformCheck
	}
	return failure.PresentationFor(evidence)
}

func incidentStatusPresentation(incident Incident) IncidentStatusPresentation {
	if incident.Status == StatusClosedAccepted {
		return IncidentStatusPresentation{
			State: LifecycleAcceptedRisk, HeadlineTH: "Admin ปิดการติดตามเหตุนี้",
			StatusSummaryTH: "ปิดโดยยอมรับความเสี่ยง ไม่ได้ยืนยันว่าปัญหาหายแล้ว", ActionRequired: false,
		}
	}
	if incident.RootCause == RootSMLConnectivity {
		if incident.Status == StatusResolved {
			return IncidentStatusPresentation{
				State: LifecycleConnectionRestored, HeadlineTH: "เชื่อมต่อ Java Web Service ได้แล้ว",
				StatusSummaryTH: "ไม่ต้องดำเนินการ", VerifiedAt: incident.ResolvedAt, ActionRequired: false,
			}
		}
		return IncidentStatusPresentation{
			State: LifecycleConnectionFailed, HeadlineTH: "เชื่อมต่อ Java Web Service ไม่สำเร็จ",
			StatusSummaryTH: "ควรตรวจสอบ Java Web Service, Network และ Server ของลูกค้า", ActionRequired: true,
		}
	}
	if incident.Status == StatusResolved {
		return IncidentStatusPresentation{
			State: LifecycleResolved, HeadlineTH: "ระบบกลับมาทำงานปกติแล้ว",
			StatusSummaryTH: "ไม่ต้องดำเนินการ", VerifiedAt: incident.ResolvedAt, ActionRequired: false,
		}
	}
	presentation := incidentPresentation(incident)
	return IncidentStatusPresentation{
		State: LifecycleActiveProblem, HeadlineTH: presentation.TitleTH,
		StatusSummaryTH: "ควรเปิดรายละเอียดและตรวจสอบ", ActionRequired: true,
	}
}

func telegramImpact(code string, presentation failure.Presentation) string {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "REPORT_SET_INCOMPLETE", "ALL_REPORTS_FAILED":
		return "สร้างรายงานไม่ครบ ระบบไม่ส่ง LINE"
	case "LINE_DELIVERY_FAILED_PERMANENT", "LINE_PUSH_RETRY_EXHAUSTED":
		return "สร้างรายงานแล้ว แต่ส่งข้อความ LINE ไม่สำเร็จ"
	}
	if strings.Contains(presentation.TitleTH, "Java Web Service") {
		return "รายงานที่เกี่ยวข้องสร้างไม่สำเร็จ และรอบส่ง LINE จะหยุดหากชุดรายงานไม่ครบ"
	}
	return "งานที่เกี่ยวข้องหยุดอย่างปลอดภัย กรุณาเปิดรายละเอียดเพื่อตรวจสอบ"
}

func safeText(value string) string {
	value = strings.TrimSpace(value)
	if !safeErrorCodePattern.MatchString(value) {
		return "UNKNOWN"
	}
	return value
}
