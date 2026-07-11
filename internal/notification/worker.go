package notification

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/google/uuid"
)

var (
	ErrNoExecutionReady   = errors.New("no notification execution is ready")
	ErrExecutionLeaseLost = errors.New("notification execution lease was lost")
)

type ReportResult struct {
	Key        report.Key
	Period     report.Period
	Metrics    map[string]string
	FinishedAt time.Time
}

type Target struct {
	RecipientID uuid.UUID
	ReportKeys  []report.Key
}

type Work struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	ScheduleID uuid.UUID
	TenantName string
	Pending    bool
	Partial    bool
	Reports    []ReportResult
	Targets    []Target
}

type PreparedDelivery struct {
	ID            uuid.UUID
	RecipientID   uuid.UUID
	RetryKey      uuid.UUID
	ReferenceHash []byte
	Payload       json.RawMessage
	ReportKeys    []report.Key
}

type Store interface {
	Claim(context.Context, string, time.Duration, time.Time) (Work, error)
	Defer(context.Context, uuid.UUID, string, time.Time, time.Time) error
	Fail(context.Context, uuid.UUID, string, string, time.Time) error
	Publish(context.Context, uuid.UUID, string, []PreparedDelivery, bool, time.Time) error
}

type Renderer func(line.FlexInput) (json.RawMessage, error)

type Worker struct {
	store         Store
	render        Renderer
	tokens        *auth.SessionManager
	entropy       io.Reader
	publicBaseURL *url.URL
	workerID      string
	now           func() time.Time
}

func NewWorker(store Store, render Renderer, tokens *auth.SessionManager, entropy io.Reader, publicBaseURL *url.URL, workerID string, now func() time.Time) *Worker {
	if entropy == nil {
		entropy = rand.Reader
	}
	return &Worker{store: store, render: render, tokens: tokens, entropy: entropy, publicBaseURL: publicBaseURL, workerID: workerID, now: now}
}

func (worker *Worker) ProcessOne(ctx context.Context) error {
	now := worker.now().UTC()
	work, err := worker.store.Claim(ctx, worker.workerID, time.Minute, now)
	if err != nil {
		return err
	}
	if work.Pending {
		return worker.store.Defer(ctx, work.ID, worker.workerID, now.Add(5*time.Second), now)
	}
	if len(work.Reports) == 0 {
		return worker.store.Fail(ctx, work.ID, worker.workerID, "ALL_REPORTS_FAILED", now)
	}
	if len(work.Targets) == 0 {
		return worker.store.Fail(ctx, work.ID, worker.workerID, "NO_ELIGIBLE_RECIPIENTS", now)
	}
	prepared := make([]PreparedDelivery, 0, len(work.Targets))
	for _, target := range work.Targets {
		reports := permittedReports(work.Reports, target.ReportKeys)
		// Never turn an incomplete result set into a smaller card silently. The
		// database query applies the same invariant; this is the worker boundary.
		if len(reports) == 0 || len(reports) != len(target.ReportKeys) {
			continue
		}
		reference, referenceHash, err := worker.issueDeliveryReference()
		if err != nil {
			return worker.store.Fail(ctx, work.ID, worker.workerID, "DELIVERY_REFERENCE_FAILED", now)
		}
		actionURL := *worker.publicBaseURL
		actionURL.Path = "/app"
		query := actionURL.Query()
		query.Set("deliveryRef", reference)
		actionURL.RawQuery = query.Encode()
		generatedAt := reports[0].FinishedAt
		flexReports := make([]line.FlexReport, 0, len(reports))
		for _, result := range reports {
			if result.FinishedAt.After(generatedAt) {
				generatedAt = result.FinishedAt
			}
			flexReports = append(flexReports, line.FlexReport{Key: result.Key, Metrics: result.Metrics})
		}
		payload, err := worker.render(line.FlexInput{
			TenantName: work.TenantName, Period: reports[0].Period, GeneratedAt: generatedAt,
			ActionURL: actionURL.String(), Reports: flexReports,
		})
		if err != nil {
			return worker.store.Fail(ctx, work.ID, worker.workerID, "FLEX_RENDER_FAILED", now)
		}
		prepared = append(prepared, PreparedDelivery{
			ID: uuid.New(), RecipientID: target.RecipientID, RetryKey: uuid.New(), ReferenceHash: referenceHash, Payload: payload,
			ReportKeys: append([]report.Key(nil), target.ReportKeys...),
		})
	}
	if len(prepared) == 0 {
		return worker.store.Fail(ctx, work.ID, worker.workerID, "NO_ELIGIBLE_RECIPIENTS", now)
	}
	return worker.store.Publish(ctx, work.ID, worker.workerID, prepared, work.Partial, now)
}

func (worker *Worker) issueDeliveryReference() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(worker.entropy, raw); err != nil {
		return "", nil, fmt.Errorf("generate delivery reference: %w", err)
	}
	reference := base64.RawURLEncoding.EncodeToString(raw)
	return reference, worker.tokens.HashToken("delivery-reference:" + reference), nil
}

func permittedReports(results []ReportResult, keys []report.Key) []ReportResult {
	allowed := make(map[report.Key]struct{}, len(keys))
	for _, key := range keys {
		allowed[key] = struct{}{}
	}
	filtered := make([]ReportResult, 0, len(results))
	for _, result := range results {
		if _, ok := allowed[result.Key]; ok {
			filtered = append(filtered, result)
		}
	}
	return filtered
}
