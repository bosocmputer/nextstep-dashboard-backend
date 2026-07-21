package failure

import (
	"strings"
	"time"
)

type ProblemArea string
type InvestigationOwner string
type LoadSignal string

const (
	ProblemCustomerNetwork       ProblemArea = "CUSTOMER_NETWORK"
	ProblemCustomerJavaWS        ProblemArea = "CUSTOMER_JAVA_WS"
	ProblemNextstepReportBuild   ProblemArea = "NEXTSTEP_REPORT_BUILD"
	ProblemNextstepReportStorage ProblemArea = "NEXTSTEP_REPORT_STORAGE"
	ProblemNextstepJobProcessing ProblemArea = "NEXTSTEP_JOB_PROCESSING"
	ProblemNextstepNotification  ProblemArea = "NEXTSTEP_NOTIFICATION"
	ProblemLineProvider          ProblemArea = "LINE_PROVIDER"
	ProblemNextstepDatabase      ProblemArea = "NEXTSTEP_DATABASE"
	ProblemNextstepCapacity      ProblemArea = "NEXTSTEP_CAPACITY"
	ProblemConfiguration         ProblemArea = "CONFIGURATION"
	ProblemUnknown               ProblemArea = "UNKNOWN"

	OwnerCustomerIT         InvestigationOwner = "CUSTOMER_IT"
	OwnerNextstepTeam       InvestigationOwner = "NEXTSTEP_TEAM"
	OwnerLineProvider       InvestigationOwner = "LINE_PROVIDER"
	OwnerJointInvestigation InvestigationOwner = "JOINT_INVESTIGATION"

	LoadNoNextstepSignal     LoadSignal = "NO_NEXTSTEP_LOAD_SIGNAL"
	LoadReviewRequired       LoadSignal = "REVIEW_REQUIRED"
	LoadInsufficientEvidence LoadSignal = "INSUFFICIENT_EVIDENCE"
)

type JavaWSProtocolEvidence struct {
	RequestRef              string     `json:"requestRef"`
	RequestCount            int        `json:"requestCount"`
	RetryCount              int        `json:"retryCount"`
	RequestSentAt           *time.Time `json:"requestSentAt,omitempty"`
	FirstResponseByteAt     *time.Time `json:"firstResponseByteAt,omitempty"`
	ResponseCompletedAt     *time.Time `json:"responseCompletedAt,omitempty"`
	HTTPStatus              *int       `json:"httpStatus,omitempty"`
	ResponseContentType     string     `json:"responseContentType,omitempty"`
	ResponseBodyBytes       *int64     `json:"responseBodyBytes,omitempty"`
	SOAPValid               *bool      `json:"soapValid,omitempty"`
	SOAPReturnCharacters    *int       `json:"soapReturnCharacters,omitempty"`
	Base64Valid             *bool      `json:"base64Valid,omitempty"`
	DecodedPayloadBytes     *int64     `json:"decodedPayloadBytes,omitempty"`
	ZIPSignatureValid       *bool      `json:"zipSignatureValid,omitempty"`
	ResponseSHA256          string     `json:"responseSha256,omitempty"`
	TenantConcurrentQueries int        `json:"tenantConcurrentQueries"`
	HostConcurrentQueries   int        `json:"hostConcurrentQueries"`
}

type Baseline struct {
	P50MS       int64 `json:"p50Ms,omitempty"`
	P90MS       int64 `json:"p90Ms,omitempty"`
	SampleCount int   `json:"sampleCount"`
}

type AdminFailureAssessment struct {
	ProblemArea        ProblemArea        `json:"problemArea"`
	InvestigationOwner InvestigationOwner `json:"investigationOwner"`
	LoadSignal         LoadSignal         `json:"loadSignal"`
	SummaryTH          string             `json:"summaryTh"`
	ProblemAreaTH      string             `json:"problemAreaTh"`
	OwnerTH            string             `json:"ownerTh"`
	LoadSignalTH       string             `json:"loadSignalTh"`
	CustomerActionTH   string             `json:"customerActionTh"`
}

func Assess(evidence Evidence, baseline Baseline) AdminFailureAssessment {
	assessment := assessmentForStage(evidence)
	assessment.LoadSignal = LoadInsufficientEvidence
	assessment.LoadSignalTH = "หลักฐานยังไม่เพียงพอสำหรับประเมินภาระจาก Nextstep"
	if evidence.Level == LevelLegacyPartial {
		assessment.InvestigationOwner = OwnerJointInvestigation
		assessment.OwnerTH = "ทีม Nextstep และผู้ดูแลระบบที่เกี่ยวข้อง"
		return assessment
	}
	protocol := evidence.ProtocolEvidence
	if evidence.Version >= 2 && evidence.Level == LevelConfirmed && protocol != nil && baseline.SampleCount >= 5 && evidence.DurationMS != nil {
		normalLoad := protocol.RequestCount == 1 && protocol.RetryCount == 0 && protocol.TenantConcurrentQueries == 1 && protocol.HostConcurrentQueries > 0 && protocol.HostConcurrentQueries <= 2 && baseline.P90MS > 0 && *evidence.DurationMS <= baseline.P90MS
		if normalLoad {
			assessment.LoadSignal = LoadNoNextstepSignal
			assessment.LoadSignalTH = "ไม่พบสัญญาณว่า Nextstep สร้างภาระผิดปกติ"
		} else {
			assessment.LoadSignal = LoadReviewRequired
			assessment.LoadSignalTH = "ควรให้ทีม Nextstep ตรวจสอบภาระและลำดับการทำงานเพิ่มเติม"
		}
	}
	return assessment
}

func assessmentForStage(evidence Evidence) AdminFailureAssessment {
	result := AdminFailureAssessment{ProblemArea: ProblemUnknown, InvestigationOwner: OwnerJointInvestigation, SummaryTH: "ยังระบุส่วนที่เกิดปัญหาไม่ได้จากหลักฐานที่มี", ProblemAreaTH: "ยังไม่ทราบส่วนที่เกิดปัญหา", OwnerTH: "ทีม Nextstep และผู้ดูแลระบบที่เกี่ยวข้อง", CustomerActionTH: "รอข้อมูลเพิ่มเติมก่อน Restart หรือเปลี่ยนการตั้งค่า"}
	switch evidence.Stage {
	case StageLoadConnection, StageResolveEndpoint:
		result.ProblemArea, result.InvestigationOwner = ProblemConfiguration, OwnerNextstepTeam
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH = "การตั้งค่าการเชื่อมต่อ SML ไม่พร้อมใช้งาน", "การตั้งค่าใน Dashboard", "ทีมดูแล Nextstep"
	case StageConnectJavaWS, StageSendRequest, StageWaitResponse:
		result.ProblemArea, result.InvestigationOwner = ProblemCustomerNetwork, OwnerCustomerIT
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH = "ติดต่อ Java Web Service ของร้านไม่สำเร็จ", "Network หรือ Java Web Service ของลูกค้า", "ผู้ดูแล Server ลูกค้า"
	case StageReadResponse, StageValidateResponse, StageDecodePayload:
		result.ProblemArea, result.InvestigationOwner = ProblemCustomerJavaWS, OwnerCustomerIT
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH = "คำตอบจาก Java Web Service ไม่สมบูรณ์", "คำตอบจาก Java Web Service", "ผู้ดูแล Java Web Service ของลูกค้า"
		if evidence.SafeErrorCode == "SML_ZIP_FORMAT_INVALID" && confirmedInvalidZIP(evidence.ProtocolEvidence) {
			result.SummaryTH = "เชื่อมต่อ Java Web Service สำเร็จ แต่ข้อมูลตอบกลับไม่ใช่ ZIP"
		}
	case StageBuildReport:
		result.ProblemArea, result.InvestigationOwner = ProblemNextstepReportBuild, OwnerNextstepTeam
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH, result.CustomerActionTH = "สร้างตัวเลขและตารางรายงานไม่สำเร็จ", "ระบบสร้างรายงานของ Nextstep", "ทีมดูแล Nextstep", "ไม่ต้องตรวจหรือ Restart Server ลูกค้า"
	case StageSaveReport:
		result.ProblemArea, result.InvestigationOwner = ProblemNextstepReportStorage, OwnerNextstepTeam
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH, result.CustomerActionTH = "บันทึกผลรายงานลง Dashboard ไม่สำเร็จ", "ระบบจัดเก็บรายงานของ Nextstep", "ทีมดูแล Nextstep", "ไม่ต้องตรวจหรือ Restart Server ลูกค้า"
	case StageQueueExecution:
		result.ProblemArea, result.InvestigationOwner = ProblemNextstepJobProcessing, OwnerNextstepTeam
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH, result.CustomerActionTH = "ระบบประมวลผลงานหยุดระหว่างทำงาน", "คิวและ Worker ของ Nextstep", "ทีมดูแล Nextstep", "ไม่ต้องตรวจหรือ Restart Server ลูกค้า"
	case StagePrepareNotification:
		result.ProblemArea, result.InvestigationOwner = ProblemNextstepNotification, OwnerNextstepTeam
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH, result.CustomerActionTH = "เตรียมชุดรายงานสำหรับ LINE ไม่สำเร็จ", "ระบบเตรียมการแจ้งเตือนของ Nextstep", "ทีมดูแล Nextstep", "ไม่ต้องตรวจหรือ Restart Server ลูกค้า"
	case StageSendLINE:
		result.ProblemArea, result.InvestigationOwner = ProblemLineProvider, OwnerLineProvider
		result.SummaryTH, result.ProblemAreaTH, result.OwnerTH = "ส่งข้อความไปยัง LINE ไม่สำเร็จ", "บริการส่งข้อความ LINE", "ทีม Nextstep และผู้ให้บริการ LINE"
	case StagePlatformCheck:
		if evidence.Category == CategoryCapacity {
			result.ProblemArea, result.SummaryTH, result.ProblemAreaTH = ProblemNextstepCapacity, "ทรัพยากร Server Nextstep ใกล้หรือเกินขีดจำกัด", "ทรัพยากร Server Nextstep"
		} else if strings.Contains(evidence.SafeErrorCode, "DATABASE") || strings.Contains(evidence.SafeErrorCode, "POSTGRES") {
			result.ProblemArea, result.SummaryTH, result.ProblemAreaTH = ProblemNextstepDatabase, "ฐานข้อมูลของ Nextstep ทำงานไม่พร้อม", "ฐานข้อมูล Nextstep"
		} else {
			result.ProblemArea, result.SummaryTH, result.ProblemAreaTH = ProblemNextstepJobProcessing, "บริการระบบของ Nextstep ทำงานไม่พร้อม", "บริการระบบ Nextstep"
		}
		result.InvestigationOwner, result.OwnerTH, result.CustomerActionTH = OwnerNextstepTeam, "ทีมดูแล Nextstep", "ไม่ต้องตรวจหรือ Restart Server ลูกค้า"
	}
	return result
}

func confirmedInvalidZIP(protocol *JavaWSProtocolEvidence) bool {
	return protocol != nil && protocol.HTTPStatus != nil && *protocol.HTTPStatus >= 200 && *protocol.HTTPStatus < 300 && protocol.SOAPValid != nil && *protocol.SOAPValid && protocol.Base64Valid != nil && *protocol.Base64Valid && protocol.ZIPSignatureValid != nil && !*protocol.ZIPSignatureValid
}
