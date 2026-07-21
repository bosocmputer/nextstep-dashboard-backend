package failure

import (
	"strings"
	"testing"
	"time"
)

func TestAssessFailureAttributesConfirmedInvalidZIPToCustomerJavaWS(t *testing.T) {
	status := 200
	soapValid, base64Valid, zipValid := true, true, false
	duration := int64(64)
	evidence := Complete(Evidence{
		Version: 2, Level: LevelConfirmed, Category: CategoryJavaWSResponse, Stage: StageDecodePayload,
		OccurredAt: time.Date(2026, 7, 21, 1, 0, 0, 0, time.UTC), DurationMS: &duration,
		SafeErrorCode: "SML_ZIP_FORMAT_INVALID",
		ProtocolEvidence: &JavaWSProtocolEvidence{
			RequestRef: "NXR-TEST1234567890", RequestCount: 1, RetryCount: 0, HTTPStatus: &status,
			SOAPValid: &soapValid, Base64Valid: &base64Valid, ZIPSignatureValid: &zipValid,
			TenantConcurrentQueries: 1, HostConcurrentQueries: 1,
		},
	})
	assessment := Assess(evidence, Baseline{P50MS: 80, P90MS: 96, SampleCount: 8})
	if assessment.ProblemArea != ProblemCustomerJavaWS || assessment.InvestigationOwner != OwnerCustomerIT || assessment.LoadSignal != LoadNoNextstepSignal {
		t.Fatalf("assessment = %+v", assessment)
	}
	if !strings.Contains(assessment.SummaryTH, "เชื่อมต่อ Java Web Service สำเร็จ") || !strings.Contains(assessment.SummaryTH, "ไม่ใช่ ZIP") {
		t.Fatalf("summary = %q", assessment.SummaryTH)
	}
}

func TestAssessFailureDoesNotClaimNormalLoadWithoutEnoughEvidence(t *testing.T) {
	duration := int64(64)
	evidence := Complete(Evidence{Version: 1, Level: LevelLegacyPartial, Category: CategoryJavaWSResponse, Stage: StageDecodePayload, OccurredAt: time.Now(), DurationMS: &duration, SafeErrorCode: "SML_ZIP_FORMAT_INVALID"})
	assessment := Assess(evidence, Baseline{P90MS: 96, SampleCount: 4})
	if assessment.LoadSignal != LoadInsufficientEvidence || assessment.InvestigationOwner != OwnerJointInvestigation {
		t.Fatalf("assessment = %+v", assessment)
	}
}

func TestAssessConfirmedV1KeepsKnownStageOwnerButDoesNotClaimLoad(t *testing.T) {
	evidence := Complete(Evidence{Version: 1, Level: LevelConfirmed, Category: CategoryReportProcessing, Stage: StageBuildReport, OccurredAt: time.Now(), SafeErrorCode: "REPORT_OUTPUT_INVALID"})
	assessment := Assess(evidence, Baseline{})
	if assessment.InvestigationOwner != OwnerNextstepTeam || assessment.ProblemArea != ProblemNextstepReportBuild || assessment.LoadSignal != LoadInsufficientEvidence {
		t.Fatalf("confirmed v1 assessment = %+v", assessment)
	}
}

func TestAssessFailureMapsNextstepStagesToConcreteThaiAreas(t *testing.T) {
	tests := []struct {
		stage Stage
		area  ProblemArea
		text  string
	}{
		{StageBuildReport, ProblemNextstepReportBuild, "สร้างตัวเลขและตารางรายงานไม่สำเร็จ"},
		{StageSaveReport, ProblemNextstepReportStorage, "บันทึกผลรายงานลง Dashboard ไม่สำเร็จ"},
		{StageQueueExecution, ProblemNextstepJobProcessing, "ระบบประมวลผลงานหยุดระหว่างทำงาน"},
		{StagePrepareNotification, ProblemNextstepNotification, "เตรียมชุดรายงานสำหรับ LINE ไม่สำเร็จ"},
		{StageSendLINE, ProblemLineProvider, "ส่งข้อความไปยัง LINE ไม่สำเร็จ"},
	}
	for _, test := range tests {
		evidence := Complete(Evidence{Version: 2, Level: LevelConfirmed, Stage: test.stage, OccurredAt: time.Now(), SafeErrorCode: "REPORT_CONTRACT_INVALID"})
		assessment := Assess(evidence, Baseline{})
		if assessment.ProblemArea != test.area || assessment.SummaryTH != test.text || strings.Contains(assessment.SummaryTH, "ประมวลผลภายใน") {
			t.Fatalf("stage %s assessment = %+v", test.stage, assessment)
		}
	}
}
