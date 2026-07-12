package line

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/google/uuid"
)

type previewTenantReader struct {
	item tenant.Tenant
	err  error
}

func (reader previewTenantReader) Get(context.Context, uuid.UUID) (tenant.Tenant, error) {
	return reader.item, reader.err
}

func TestFlexPreviewUsesTenantTimezoneAndExactRenderedMessage(t *testing.T) {
	tenantID := uuid.New()
	baseURL, _ := url.Parse("https://dashboard.nextstep-soft.com")
	now := time.Date(2026, 7, 10, 18, 30, 0, 0, time.UTC)
	service := NewFlexPreviewService(previewTenantReader{item: tenant.Tenant{
		ID: tenantID, Name: "ร้านตัวอย่าง", Timezone: "Asia/Bangkok",
	}}, baseURL, func() time.Time { return now })

	preview, err := service.Preview(context.Background(), tenantID, FlexPreviewInput{
		PeriodPreset: report.Yesterday,
		ReportKeys:   []report.Key{report.SalesGoodsServices, report.StockBalance},
	})
	if err != nil {
		t.Fatalf("Preview() error = %v", err)
	}
	if preview.TenantName != "ร้านตัวอย่าง" || preview.Period.DateFrom != "2026-07-10" || preview.Period.DateTo != "2026-07-10" {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.PayloadBytes != len(preview.Message) || preview.PayloadBytes == 0 || len(preview.Reports) != 2 {
		t.Fatalf("payloadBytes=%d message=%d reports=%d", preview.PayloadBytes, len(preview.Message), len(preview.Reports))
	}
	if preview.PresentationVersion != FlexPresentationVersion {
		t.Fatalf("presentationVersion=%q", preview.PresentationVersion)
	}
	for _, item := range preview.Reports {
		if len(item.Metrics) != 2 {
			t.Fatalf("report %s metrics=%d", item.Key, len(item.Metrics))
		}
		if item.Primary.Label == "" || item.Primary.Value == "" || item.CategoryLabel == "" || item.ActionURL == "" {
			t.Fatalf("executive preview fields missing = %+v", item)
		}
		if !strings.Contains(string(preview.Message), item.Primary.Label) || !strings.Contains(string(preview.Message), item.Primary.Value) {
			t.Fatalf("rendered message does not contain executive metric %+v", item.Primary)
		}
	}
	if !strings.Contains(string(preview.Message), "เวลาไทย") || strings.Contains(string(preview.Message), "UTC") || !strings.Contains(preview.ActionURL, "/app/tenant/"+tenantID.String()) {
		t.Fatalf("preview timezone/action mismatch: %+v message=%s", preview, preview.Message)
	}
	var message struct {
		Type    string `json:"type"`
		AltText string `json:"altText"`
	}
	if err := json.Unmarshal(preview.Message, &message); err != nil || message.Type != "flex" || message.AltText != preview.AltText {
		t.Fatalf("message=%s err=%v previewAlt=%q", preview.Message, err, preview.AltText)
	}
}

func TestFlexPreviewRejectsInvalidReportsBeforeTenantLookup(t *testing.T) {
	baseURL, _ := url.Parse("https://dashboard.nextstep-soft.com")
	service := NewFlexPreviewService(previewTenantReader{err: errors.New("must not be called")}, baseURL, time.Now)

	_, err := service.Preview(context.Background(), uuid.New(), FlexPreviewInput{
		PeriodPreset: report.Yesterday,
		ReportKeys:   []report.Key{report.SalesGoodsServices, report.SalesGoodsServices},
	})
	var validationError *FlexPreviewValidationError
	if !errors.As(err, &validationError) || validationError.Field != "reportKeys" || validationError.Code != "DUPLICATE_REPORT" {
		t.Fatalf("Preview() error = %v", err)
	}
}

func TestFlexPreviewPreservesTenantNotFound(t *testing.T) {
	baseURL, _ := url.Parse("https://dashboard.nextstep-soft.com")
	service := NewFlexPreviewService(previewTenantReader{err: tenant.ErrNotFound}, baseURL, time.Now)

	_, err := service.Preview(context.Background(), uuid.New(), FlexPreviewInput{
		PeriodPreset: report.Yesterday,
		ReportKeys:   []report.Key{report.SalesGoodsServices},
	})
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("Preview() error = %v", err)
	}
}
