package sentinel

import (
	"fmt"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
)

type MatchingSuccess struct {
	FinishedAt time.Time `json:"finishedAt"`
	DurationMS int64     `json:"durationMs"`
}

type IncidentDiagnosis struct {
	Assessment                failure.AdminFailureAssessment  `json:"assessment"`
	ProtocolEvidence          *failure.JavaWSProtocolEvidence `json:"protocolEvidence,omitempty"`
	PriorMatchingSuccess      *MatchingSuccess                `json:"priorMatchingSuccess,omitempty"`
	SubsequentMatchingSuccess *MatchingSuccess                `json:"subsequentMatchingSuccess,omitempty"`
	Baseline                  failure.Baseline                `json:"baseline"`
	CustomerMessageTH         string                          `json:"customerMessageTh,omitempty"`
}

type DiagnosisRecord struct {
	Evidence                  failure.Evidence
	TenantName                string
	ConnectionReference       *SMLConnectionReference
	PriorMatchingSuccess      *MatchingSuccess
	SubsequentMatchingSuccess *MatchingSuccess
	Baseline                  failure.Baseline
}

func presentDiagnosis(record DiagnosisRecord) IncidentDiagnosis {
	evidence := failure.Complete(record.Evidence)
	result := IncidentDiagnosis{
		Assessment: failure.Assess(evidence, record.Baseline), ProtocolEvidence: evidence.ProtocolEvidence,
		PriorMatchingSuccess: record.PriorMatchingSuccess, SubsequentMatchingSuccess: record.SubsequentMatchingSuccess,
		Baseline: record.Baseline,
	}
	if evidence.Version >= 2 && evidence.Level == failure.LevelConfirmed && evidence.ProtocolEvidence != nil {
		result.CustomerMessageTH = customerMessage(record, result.Assessment)
	}
	return result
}

func customerMessage(record DiagnosisRecord, assessment failure.AdminFailureAssessment) string {
	evidence := record.Evidence
	protocol := evidence.ProtocolEvidence
	if protocol == nil {
		return ""
	}
	name := sanitizeCustomerName(record.TenantName)
	if name == "" {
		name = "ร้านที่เกี่ยวข้อง"
	}
	endpoint := "ไม่พบ URL ที่ยืนยันได้"
	if record.ConnectionReference != nil {
		reference := sanitizeConnectionReference(*record.ConnectionReference)
		endpoint = reference.EndpointURLAtFailure
		if endpoint == "" {
			endpoint = reference.CurrentEndpointURL
		}
		if endpoint == "" {
			endpoint = "ไม่พบ URL ที่ยืนยันได้"
		}
	}
	timeTH := formatDiagnosisThaiTime(evidence.OccurredAt)
	requestText := fmt.Sprintf("ส่งคำขอ %d ครั้ง", protocol.RequestCount)
	if protocol.RetryCount == 0 {
		requestText += " และไม่มี Retry"
	} else {
		requestText += fmt.Sprintf(" โดย Retry %d ครั้ง", protocol.RetryCount)
	}
	lines := []string{
		"ผลตรวจสอบเหตุรายงานของ " + name,
		"เวลาเกิดเหตุ: " + timeTH,
		"Java Web Service Base URL: " + endpoint,
		"ข้อเท็จจริง: " + assessment.SummaryTH,
		"การส่งคำขอ: " + requestText,
		"การประเมินภาระ: " + assessment.LoadSignalTH,
		"ผู้ที่ควรตรวจสอบ: " + assessment.OwnerTH,
	}
	if protocol.RequestCount > 0 && protocol.RequestRef != "" {
		lines = append(lines[:3], append([]string{"Request Ref: " + protocol.RequestRef}, lines[3:]...)...)
	}
	return strings.Join(lines, "\n")
}

func sanitizeCustomerName(value string) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 120 {
		value = string(runes[:120])
	}
	return value
}

func formatDiagnosisThaiTime(value time.Time) string {
	bangkok := value.In(time.FixedZone("Asia/Bangkok", 7*60*60))
	return fmt.Sprintf("%02d/%02d/%d %02d:%02d น. เวลาไทย", bangkok.Day(), bangkok.Month(), bangkok.Year()+543, bangkok.Hour(), bangkok.Minute())
}
