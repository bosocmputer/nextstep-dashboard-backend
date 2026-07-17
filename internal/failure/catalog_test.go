package failure

import (
	"strings"
	"testing"
)

func TestPresentationExplainsJavaWSTransportPhaseInThai(t *testing.T) {
	tests := []struct {
		phase TransportPhase
		want  string
	}{
		{PhaseBeforeRequestSent, "ไม่สามารถเริ่มส่งคำขอ"},
		{PhaseRequestSentResultUnknown, "ส่งคำขอแล้ว แต่ไม่ได้รับผลยืนยัน"},
		{PhaseResponseStarted, "เริ่มได้รับคำตอบ"},
	}
	for _, test := range tests {
		presentation := PresentationFor(Evidence{
			Version: 1, Level: LevelConfirmed, Category: CategoryJavaWSConnectivity,
			Stage: StageConnectJavaWS, TransportPhase: test.phase, SafeErrorCode: CodeSMLUnreachable,
		})
		if presentation.TitleTH != "ติดต่อ Java Web Service ของร้านไม่สำเร็จ" || !strings.Contains(presentation.SummaryTH, test.want) {
			t.Fatalf("phase=%s presentation=%+v", test.phase, presentation)
		}
	}
}

func TestLegacyPresentationDoesNotInventTransportEvidence(t *testing.T) {
	presentation := PresentationFor(Evidence{
		Level: LevelLegacyPartial, Category: CategoryJavaWSConnectivity,
		SafeErrorCode: CodeSMLUnreachable,
	})
	if !strings.Contains(presentation.EvidenceNoteTH, "ระบบรุ่นเดิมไม่ได้บันทึก") || presentation.StageTH != "ระบบรุ่นเดิมไม่ได้บันทึกขั้นตอนที่ล้ม" || strings.Contains(presentation.SummaryTH, "ก่อนส่ง") {
		t.Fatalf("legacy presentation invented evidence: %+v", presentation)
	}
}

func TestEveryOperationalCodeHasThaiPresentation(t *testing.T) {
	for _, code := range KnownCodes() {
		evidence := EvidenceForCode(code)
		presentation := PresentationFor(evidence)
		if presentation.TitleTH == "" || presentation.SummaryTH == "" || presentation.StageTH == "" || len(presentation.NextActionsTH) == 0 {
			t.Fatalf("code=%s incomplete presentation=%+v", code, presentation)
		}
		for _, text := range append([]string{presentation.TitleTH, presentation.SummaryTH, presentation.StageTH}, presentation.NextActionsTH...) {
			if strings.Contains(text, "failed safely") || strings.Contains(text, "UNKNOWN") {
				t.Fatalf("code=%s leaked English fallback in %q", code, text)
			}
		}
	}
}

func TestUnknownCodeUsesSafeThaiFallback(t *testing.T) {
	presentation := PresentationFor(EvidenceForCode("SOME_NEW_FAILURE"))
	if presentation.TitleTH != "ระบบไม่สามารถดำเนินงานนี้ได้" || strings.Contains(presentation.SummaryTH, "SOME_NEW_FAILURE") {
		t.Fatalf("unknown presentation=%+v", presentation)
	}
}
