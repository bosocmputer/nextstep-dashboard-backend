package failure

import (
	"sort"
	"strings"
	"time"
)

type Category string

const (
	CategorySMLConfiguration   Category = "SML_CONFIGURATION"
	CategoryJavaWSConnectivity Category = "JAVA_WS_CONNECTIVITY"
	CategoryJavaWSResponse     Category = "JAVA_WS_RESPONSE"
	CategoryReportProcessing   Category = "REPORT_PROCESSING"
	CategoryQueueWorker        Category = "QUEUE_WORKER"
	CategoryNotification       Category = "NOTIFICATION"
	CategoryLineDelivery       Category = "LINE_DELIVERY"
	CategoryPlatform           Category = "PLATFORM"
	CategoryCapacity           Category = "CAPACITY"
)

type Stage string

const (
	StageLoadConnection      Stage = "LOAD_CONNECTION"
	StageResolveEndpoint     Stage = "RESOLVE_ENDPOINT"
	StageConnectJavaWS       Stage = "CONNECT_JAVA_WS"
	StageSendRequest         Stage = "SEND_REQUEST"
	StageWaitResponse        Stage = "WAIT_RESPONSE"
	StageReadResponse        Stage = "READ_RESPONSE"
	StageValidateResponse    Stage = "VALIDATE_RESPONSE"
	StageDecodePayload       Stage = "DECODE_PAYLOAD"
	StageBuildReport         Stage = "BUILD_REPORT"
	StageSaveReport          Stage = "SAVE_REPORT"
	StagePrepareNotification Stage = "PREPARE_NOTIFICATION"
	StageSendLINE            Stage = "SEND_LINE"
	StageQueueExecution      Stage = "QUEUE_EXECUTION"
	StagePlatformCheck       Stage = "PLATFORM_CHECK"
)

type TransportPhase string

const (
	PhaseBeforeRequestSent        TransportPhase = "BEFORE_REQUEST_SENT"
	PhaseRequestSentResultUnknown TransportPhase = "REQUEST_SENT_RESULT_UNKNOWN"
	PhaseResponseStarted          TransportPhase = "RESPONSE_STARTED"
)

type EvidenceLevel string

const (
	LevelConfirmed     EvidenceLevel = "CONFIRMED"
	LevelLegacyPartial EvidenceLevel = "LEGACY_PARTIAL"
)

type NotificationOutcome string

const (
	NotificationNotApplicable           NotificationOutcome = "NOT_APPLICABLE"
	NotificationNotCreatedIncompleteSet NotificationOutcome = "NOT_CREATED_INCOMPLETE_REPORT_SET"
	NotificationCreated                 NotificationOutcome = "CREATED"
	NotificationOutcomeUnknown          NotificationOutcome = "UNKNOWN"
)

type Evidence struct {
	Version            int            `json:"version"`
	Level              EvidenceLevel  `json:"level"`
	Category           Category       `json:"category"`
	Stage              Stage          `json:"stage"`
	TransportPhase     TransportPhase `json:"transportPhase,omitempty"`
	OccurredAt         time.Time      `json:"occurredAt"`
	StartedAt          *time.Time     `json:"startedAt,omitempty"`
	FinishedAt         *time.Time     `json:"finishedAt,omitempty"`
	DurationMS         *int64         `json:"durationMs,omitempty"`
	Attempt            *int           `json:"attempt,omitempty"`
	Retryable          bool           `json:"retryable"`
	RemoteStateUnknown bool           `json:"remoteStateUnknown"`
	ConnectionVersion  *int           `json:"connectionVersion,omitempty"`
	SafeErrorCode      string         `json:"safeErrorCode"`
	Presentation       Presentation   `json:"presentation"`
}

type Impact struct {
	ReportsTotal     int                 `json:"reportsTotal"`
	ReportsSucceeded int                 `json:"reportsSucceeded"`
	ReportsFailed    int                 `json:"reportsFailed"`
	ReportsCancelled int                 `json:"reportsCancelled"`
	Notification     NotificationOutcome `json:"notificationOutcome"`
}

type Presentation struct {
	TitleTH        string   `json:"titleTh"`
	SummaryTH      string   `json:"summaryTh"`
	StageTH        string   `json:"stageTh"`
	EvidenceNoteTH string   `json:"evidenceNoteTh,omitempty"`
	NextActionsTH  []string `json:"nextActionsTh"`
}

const (
	CodeSMLUnreachable = "SML_UNREACHABLE"
	CodeSMLTimeout     = "SML_TIMEOUT"
)

var knownCodes = []string{
	"SML_NOT_CONFIGURED", "SML_CONNECTION_LOAD_FAILED", "SML_CREDENTIAL_DECRYPT_FAILED",
	"SML_CONFIGURATION_INVALID", "SML_ENDPOINT_DENIED", "SML_QUERY_ENCODING_FAILED", "SML_REQUEST_INVALID",
	CodeSMLUnreachable, CodeSMLTimeout, "SML_QUERY_FAILED", "SML_RESPONSE_READ_FAILED", "SML_RESPONSE_TOO_LARGE",
	"SML_SOAP_INVALID", "SML_RETURN_NOT_BASE64", "SML_ZIP_FORMAT_INVALID", "SML_ZIP_EMPTY",
	"SML_ZIP_TOO_LARGE", "SML_ZIP_READ_FAILED", "SML_ZIP_INVALID", "SML_RESULT_INVALID",
	"REPORT_QUERY_RENDER_FAILED", "REPORT_CONTRACT_INVALID", "REPORT_OUTPUT_INVALID", "REPORT_ROW_LIMIT_EXCEEDED",
	"REPORT_PROGRESS_FAILED", "REPORT_CHUNK_STORE_UNAVAILABLE", "REPORT_CHUNK_MANIFEST_PERSIST_FAILED",
	"REPORT_CHUNK_PROGRESS_FAILED", "REPORT_LEASE_EXPIRED", "REPORT_SET_INCOMPLETE", "ALL_REPORTS_FAILED",
	"NO_ELIGIBLE_RECIPIENTS", "FLEX_RENDER_FAILED", "FLEX_REPORT_CONTEXT_INVALID",
	"LINE_PUSH_RETRY_EXHAUSTED", "LINE_PUSH_UNCERTAIN", "LINE_DELIVERY_FAILED_PERMANENT", "BLOCKED_QUOTA",
	"SCHEDULE_QUEUE_AGE_EXCEEDED", "SCHEDULED_REPORT_SLOW", "WORKER_HEARTBEAT_STALE", "SML_CIRCUIT_OPEN",
	"DATABASE_UNAVAILABLE", "DATABASE_CONNECTIONS_CRITICAL", "HOST_DISK_CRITICAL", "HOST_INODE_CRITICAL",
	"HOST_MEMORY_CRITICAL", "NEXTSTEP_CONTAINER_UNHEALTHY", "BACKUP_CHECKSUM_INVALID", "BACKUP_OVERDUE",
	"RESTORE_VERIFICATION_OVERDUE",
}

func KnownCodes() []string {
	result := append([]string(nil), knownCodes...)
	sort.Strings(result)
	return result
}

func EvidenceForCode(code string) Evidence {
	category, stage := classify(code)
	return Evidence{Version: 1, Level: LevelConfirmed, Category: category, Stage: stage, SafeErrorCode: code}
}

func Complete(evidence Evidence) Evidence {
	if evidence.Level == "" {
		evidence.Level = LevelConfirmed
	}
	if evidence.Version == 0 && evidence.Level == LevelConfirmed {
		evidence.Version = 1
	}
	if evidence.Category == "" || evidence.Stage == "" {
		category, stage := classify(evidence.SafeErrorCode)
		if evidence.Category == "" {
			evidence.Category = category
		}
		if evidence.Stage == "" {
			evidence.Stage = stage
		}
	}
	evidence.Presentation = PresentationFor(evidence)
	return evidence
}

func SafeMessage(code string) string {
	return PresentationFor(EvidenceForCode(code)).SummaryTH
}

func PresentationFor(evidence Evidence) Presentation {
	if !isRecognizedCode(evidence.SafeErrorCode) && evidence.Category == "" && evidence.Stage == "" {
		return Presentation{
			TitleTH: "ระบบไม่สามารถดำเนินงานนี้ได้", SummaryTH: "ระบบหยุดงานนี้อย่างปลอดภัย กรุณาตรวจสอบหลักฐานและลองดำเนินการอีกครั้งเมื่อพร้อม",
			StageTH: "ตรวจสอบระบบส่วนกลาง", NextActionsTH: []string{"แจ้งทีมดูแลระบบพร้อมรหัสอ้างอิงเหตุสำคัญ"},
		}
	}
	if evidence.Category == "" || evidence.Stage == "" {
		category, stage := classify(evidence.SafeErrorCode)
		if evidence.Category == "" {
			evidence.Category = category
		}
		if evidence.Stage == "" {
			evidence.Stage = stage
		}
	}
	presentation := Presentation{
		TitleTH: "ระบบไม่สามารถดำเนินงานนี้ได้", SummaryTH: "ระบบหยุดงานนี้อย่างปลอดภัย กรุณาตรวจสอบหลักฐานและลองดำเนินการอีกครั้งเมื่อพร้อม",
		StageTH: stageLabel(evidence.Stage), NextActionsTH: []string{"ตรวจสอบสถานะระบบและลองใหม่เมื่อสาเหตุได้รับการแก้ไขแล้ว"},
	}
	switch evidence.Category {
	case CategorySMLConfiguration:
		presentation.TitleTH = "การตั้งค่าการเชื่อมต่อ SML ไม่พร้อมใช้งาน"
		presentation.SummaryTH = "ระบบไม่สามารถโหลดหรือตรวจสอบการตั้งค่า SML ของร้านสำหรับงานนี้ได้"
		presentation.NextActionsTH = []string{"เปิดแท็บการเชื่อมต่อ SML ของร้านและตรวจสอบค่าที่บันทึกไว้", "ทดสอบการเชื่อมต่อหลังแก้ไข โดยไม่กดส่ง LINE ซ้ำระหว่างที่ยังไม่พร้อม"}
	case CategoryJavaWSConnectivity:
		presentation.TitleTH = "ติดต่อ Java Web Service ของร้านไม่สำเร็จ"
		presentation.SummaryTH = connectivitySummary(evidence)
		presentation.NextActionsTH = []string{"ตรวจสอบว่า Java Web Service ของลูกค้ากำลังทำงาน", "ตรวจสอบ Network และ Port ระหว่าง Server Dashboard กับ Server ลูกค้า"}
	case CategoryJavaWSResponse:
		presentation.TitleTH = "คำตอบจาก Java Web Service ไม่สมบูรณ์"
		presentation.SummaryTH = "ระบบได้รับคำตอบจาก Server ลูกค้า แต่ไม่สามารถอ่านหรือตรวจสอบข้อมูลตอบกลับได้อย่างปลอดภัย"
		presentation.NextActionsTH = []string{"ทดสอบการเชื่อมต่อ SML อีกครั้ง", "หากเกิดซ้ำ ให้ทีมเทคนิคตรวจ Java Web Service และรูปแบบข้อมูลตอบกลับ"}
	case CategoryReportProcessing:
		presentation.TitleTH = "ระบบสร้างรายงานจากข้อมูลที่ได้รับไม่สำเร็จ"
		presentation.SummaryTH = "ระบบหยุดสร้างรายงานเพื่อป้องกันการแสดงหรือส่งข้อมูลที่ไม่ครบหรือไม่ถูกต้อง"
		presentation.NextActionsTH = []string{"ตรวจสอบชื่อรายงาน ช่วงข้อมูล และหลักฐานด้านล่าง", "หากเกิดซ้ำ ให้ส่งรหัสอ้างอิงเหตุสำคัญแก่ทีมดูแลระบบ"}
	case CategoryQueueWorker:
		presentation.TitleTH = "ระบบประมวลผลงานไม่สำเร็จ"
		presentation.SummaryTH = "งานหยุดระหว่างรอคิวหรือประมวลผล ระบบปิดงานอย่างปลอดภัยเพื่อป้องกันงานซ้ำ"
		presentation.NextActionsTH = []string{"ตรวจสอบสถานะ Worker และคิวงาน", "รอให้ระบบฟื้นตัวก่อนเริ่มงานใหม่"}
	case CategoryNotification:
		presentation.TitleTH = "สร้างรายงานสำหรับรอบส่ง LINE ไม่ครบ"
		presentation.SummaryTH = "มีรายงานอย่างน้อยหนึ่งรายการไม่สำเร็จ ระบบจึงไม่สร้างข้อความ LINE จากข้อมูลที่ไม่ครบ"
		presentation.NextActionsTH = []string{"เปิดดูรายงานที่ล้มเหลวในรอบเดียวกัน", "แก้ต้นเหตุของรายงานก่อนทดสอบส่งใหม่"}
	case CategoryLineDelivery:
		presentation.TitleTH = "ส่งข้อความไปยัง LINE ไม่สำเร็จ"
		presentation.SummaryTH = "สร้างรายงานครบแล้ว แต่ระบบไม่สามารถยืนยันการส่งข้อความไปยัง LINE ได้"
		presentation.NextActionsTH = []string{"ตรวจสอบสถานะ LINE และโควตาการส่ง", "หลีกเลี่ยงการส่งซ้ำทันทีหากสถานะยังไม่แน่นอน"}
	case CategoryPlatform:
		presentation.TitleTH = "ระบบส่วนกลางทำงานผิดปกติ"
		presentation.SummaryTH = "ระบบตรวจพบปัญหาที่อาจกระทบการประมวลผลรายงานหรือการแจ้งเตือน"
		presentation.NextActionsTH = []string{"ตรวจสอบสถานะระบบและ Worker", "แจ้งทีมดูแลระบบพร้อมรหัสอ้างอิงเหตุสำคัญ"}
	case CategoryCapacity:
		presentation.TitleTH = "ทรัพยากรของระบบใกล้หรือเกินขีดจำกัด"
		presentation.SummaryTH = "ระบบตรวจพบว่าทรัพยากรสำคัญไม่เพียงพอสำหรับการทำงานตามปกติ"
		presentation.NextActionsTH = []string{"ตรวจสอบพื้นที่จัดเก็บ หน่วยความจำ และการเชื่อมต่อฐานข้อมูล", "ลดภาระระบบหรือเพิ่มทรัพยากรก่อนเกิดงานล้มเหลวเพิ่มเติม"}
	}
	switch strings.ToUpper(strings.TrimSpace(evidence.SafeErrorCode)) {
	case "REPORT_OUTPUT_INVALID":
		presentation.SummaryTH = "ข้อมูลตัวเลขจาก SML อยู่ในรูปแบบที่ระบบไม่รองรับ"
	case "REPORT_SET_INCOMPLETE":
		presentation.SummaryTH = "สร้างรายงานในรอบนี้ไม่ครบ ระบบจึงไม่ส่ง LINE"
	case "SML_ZIP_FORMAT_INVALID":
		presentation.SummaryTH = "Server ลูกค้าส่งผลลัพธ์กลับมาในรูปแบบ ZIP ที่ไม่ถูกต้อง"
	case "SML_ZIP_EMPTY":
		presentation.SummaryTH = "Server ลูกค้าส่งผลลัพธ์ ZIP ที่ไม่มีข้อมูลกลับมา"
	case "SML_ZIP_TOO_LARGE":
		presentation.SummaryTH = "ผลลัพธ์จาก Server ลูกค้ามีขนาดใหญ่เกินขอบเขตที่ปลอดภัย"
	case "SML_ZIP_READ_FAILED":
		presentation.SummaryTH = "ระบบอ่านผลลัพธ์ ZIP จาก Server ลูกค้าไม่สำเร็จ"
	case "SML_ZIP_INVALID":
		presentation.SummaryTH = "ผลลัพธ์ ZIP จาก Server ลูกค้าไม่สมบูรณ์"
	}
	if evidence.Level == LevelLegacyPartial {
		presentation.EvidenceNoteTH = "ระบบรุ่นเดิมไม่ได้บันทึกรายละเอียดขั้นตอนการเชื่อมต่อ จึงแสดงเฉพาะข้อเท็จจริงที่ตรวจสอบย้อนหลังได้"
	}
	return presentation
}

func isRecognizedCode(code string) bool {
	upper := strings.ToUpper(strings.TrimSpace(code))
	if strings.HasPrefix(upper, "SML_HTTP_") {
		return true
	}
	for _, known := range knownCodes {
		if upper == known {
			return true
		}
	}
	return false
}

func connectivitySummary(evidence Evidence) string {
	switch evidence.TransportPhase {
	case PhaseBeforeRequestSent:
		return "ระบบไม่สามารถเริ่มส่งคำขอไปยัง Server ลูกค้าได้"
	case PhaseRequestSentResultUnknown:
		return "ระบบส่งคำขอแล้ว แต่ไม่ได้รับผลยืนยันภายในเวลา ฝั่งลูกค้าอาจยังประมวลผลอยู่ ระบบจึงไม่ส่งซ้ำอัตโนมัติ"
	case PhaseResponseStarted:
		return "ระบบเริ่มได้รับคำตอบจาก Server ลูกค้า แต่การเชื่อมต่อสิ้นสุดก่อนรับข้อมูลครบ"
	default:
		if evidence.SafeErrorCode == CodeSMLTimeout {
			return "Java Web Service ของร้านตอบกลับไม่ทันเวลาที่กำหนด ระบบหยุดงานนี้โดยไม่ส่งซ้ำอัตโนมัติ"
		}
		return "ระบบไม่สามารถติดต่อ Java Web Service ตามการตั้งค่าของร้านในเวลาที่ระบุได้"
	}
}

func classify(code string) (Category, Stage) {
	upper := strings.ToUpper(strings.TrimSpace(code))
	switch upper {
	case "SML_NOT_CONFIGURED", "SML_CONNECTION_LOAD_FAILED", "SML_CREDENTIAL_DECRYPT_FAILED", "SML_CONFIGURATION_INVALID":
		return CategorySMLConfiguration, StageLoadConnection
	case "SML_ENDPOINT_DENIED":
		return CategorySMLConfiguration, StageResolveEndpoint
	case "SML_QUERY_ENCODING_FAILED", "SML_REQUEST_INVALID":
		return CategoryJavaWSConnectivity, StageSendRequest
	case CodeSMLUnreachable:
		return CategoryJavaWSConnectivity, StageConnectJavaWS
	case CodeSMLTimeout, "SML_QUERY_FAILED":
		return CategoryJavaWSConnectivity, StageWaitResponse
	case "SML_RESPONSE_READ_FAILED", "SML_RESPONSE_TOO_LARGE":
		return CategoryJavaWSResponse, StageReadResponse
	case "SML_SOAP_INVALID", "SML_RESULT_INVALID":
		return CategoryJavaWSResponse, StageValidateResponse
	case "SML_RETURN_NOT_BASE64", "SML_ZIP_FORMAT_INVALID", "SML_ZIP_EMPTY", "SML_ZIP_TOO_LARGE", "SML_ZIP_READ_FAILED", "SML_ZIP_INVALID":
		return CategoryJavaWSResponse, StageDecodePayload
	case "REPORT_PROGRESS_FAILED", "REPORT_CHUNK_STORE_UNAVAILABLE", "REPORT_CHUNK_MANIFEST_PERSIST_FAILED", "REPORT_CHUNK_PROGRESS_FAILED", "REPORT_LEASE_EXPIRED":
		return CategoryQueueWorker, StageQueueExecution
	case "REPORT_SET_INCOMPLETE", "ALL_REPORTS_FAILED", "NO_ELIGIBLE_RECIPIENTS", "FLEX_RENDER_FAILED", "FLEX_REPORT_CONTEXT_INVALID":
		return CategoryNotification, StagePrepareNotification
	case "LINE_PUSH_RETRY_EXHAUSTED", "LINE_PUSH_UNCERTAIN", "LINE_DELIVERY_FAILED_PERMANENT", "BLOCKED_QUOTA":
		return CategoryLineDelivery, StageSendLINE
	case "SCHEDULE_QUEUE_AGE_EXCEEDED", "SCHEDULED_REPORT_SLOW", "WORKER_HEARTBEAT_STALE", "SML_CIRCUIT_OPEN":
		return CategoryQueueWorker, StageQueueExecution
	case "DATABASE_CONNECTIONS_CRITICAL", "HOST_DISK_CRITICAL", "HOST_INODE_CRITICAL", "HOST_MEMORY_CRITICAL":
		return CategoryCapacity, StagePlatformCheck
	case "DATABASE_UNAVAILABLE", "NEXTSTEP_CONTAINER_UNHEALTHY", "BACKUP_CHECKSUM_INVALID", "BACKUP_OVERDUE", "RESTORE_VERIFICATION_OVERDUE":
		return CategoryPlatform, StagePlatformCheck
	default:
		if strings.HasPrefix(upper, "SML_HTTP_") {
			return CategoryJavaWSResponse, StageValidateResponse
		}
		if strings.HasPrefix(upper, "SML_") {
			return CategoryJavaWSResponse, StageValidateResponse
		}
		if strings.HasPrefix(upper, "REPORT_") {
			return CategoryReportProcessing, StageBuildReport
		}
		if strings.HasPrefix(upper, "LINE_") {
			return CategoryLineDelivery, StageSendLINE
		}
		return CategoryPlatform, StagePlatformCheck
	}
}

func stageLabel(stage Stage) string {
	return map[Stage]string{
		StageLoadConnection: "โหลดการตั้งค่าการเชื่อมต่อ", StageResolveEndpoint: "ตรวจสอบปลายทาง Server ลูกค้า",
		StageConnectJavaWS: "เชื่อมต่อ Java Web Service", StageSendRequest: "เตรียมและส่งคำขอ",
		StageWaitResponse: "รอคำตอบจาก Java Web Service", StageReadResponse: "อ่านคำตอบจาก Server ลูกค้า",
		StageValidateResponse: "ตรวจสอบรูปแบบคำตอบ", StageDecodePayload: "เปิดและอ่านข้อมูลรายงาน",
		StageBuildReport: "คำนวณและสร้างรายงาน", StageSaveReport: "บันทึกผลรายงาน",
		StagePrepareNotification: "ตรวจความครบถ้วนก่อนส่ง LINE", StageSendLINE: "ส่งข้อความไปยัง LINE",
		StageQueueExecution: "คิวและ Worker ประมวลผล", StagePlatformCheck: "ตรวจสอบระบบส่วนกลาง",
	}[stage]
}
