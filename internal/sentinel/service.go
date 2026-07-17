package sentinel

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/failure"
	"github.com/google/uuid"
)

var unsafeOperatorReason = regexp.MustCompile(`(?i)(https?://|\b[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\b|\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b|\b[0-9]{8,}\b|\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b|\b[0-9]{5,16}:[A-Za-z0-9_-]{24,}\b|(?:deliveryRef|[?&]ref)=)`)

var (
	ErrNotFound        = errors.New("operational incident not found")
	ErrVersionConflict = errors.New("operational incident version conflict")
	ErrInvalidInput    = errors.New("operational incident input is invalid")
	ErrNoAlertReady    = errors.New("operational alert is not ready")
	ErrAlertLeaseLost  = errors.New("operational alert lease was lost")
)

type IncidentFilter struct {
	Status   *Status
	Severity *Severity
	Cursor   string
	PageSize int
}

type IncidentPage struct {
	Data       []Incident `json:"data"`
	NextCursor string     `json:"nextCursor,omitempty"`
	HasMore    bool       `json:"hasMore"`
}

type IncidentEvent struct {
	ID                            uuid.UUID         `json:"id"`
	EventKind                     string            `json:"eventKind"`
	SourceKind                    SourceKind        `json:"sourceKind,omitempty"`
	SafeErrorCode                 string            `json:"safeErrorCode,omitempty"`
	TenantName                    string            `json:"tenantName,omitempty"`
	ObservedAt                    time.Time         `json:"observedAt"`
	FailureEvidence               *failure.Evidence `json:"failureEvidence,omitempty"`
	ReportKey                     string            `json:"reportKey,omitempty"`
	TriggerKind                   TriggerKind       `json:"triggerKind,omitempty"`
	Impact                        *failure.Impact   `json:"impact,omitempty"`
	IsDownstream                  bool              `json:"isDownstream"`
	CausedByAlertRef              string            `json:"causedByAlertRef,omitempty"`
	ConnectionChangedSinceFailure bool              `json:"connectionChangedSinceFailure"`
}

type IncidentDetail struct {
	Incident
	Events []IncidentEvent `json:"events"`
}

type Alert struct {
	ID       uuid.UUID
	Kind     string
	Incident Incident
}

type AdminStore interface {
	ListIncidents(context.Context, IncidentFilter) (IncidentPage, error)
	GetIncident(context.Context, uuid.UUID) (IncidentDetail, error)
	AcknowledgeIncident(context.Context, uuid.UUID, int, time.Time) (Incident, error)
	AcceptIncidentRisk(context.Context, uuid.UUID, int, string, time.Time) (Incident, error)
}

type AdminService struct {
	store AdminStore
	now   func() time.Time
}

func NewAdminService(store AdminStore, now func() time.Time) *AdminService {
	return &AdminService{store: store, now: now}
}

func (service *AdminService) List(ctx context.Context, filter IncidentFilter) (IncidentPage, error) {
	if filter.PageSize == 0 {
		filter.PageSize = 25
	}
	if filter.PageSize < 1 || filter.PageSize > 100 {
		return IncidentPage{}, ErrInvalidInput
	}
	page, err := service.store.ListIncidents(ctx, filter)
	if err != nil {
		return IncidentPage{}, err
	}
	for index := range page.Data {
		page.Data[index].Presentation = incidentPresentation(page.Data[index])
	}
	return page, nil
}

func (service *AdminService) Get(ctx context.Context, id uuid.UUID) (IncidentDetail, error) {
	if id == uuid.Nil {
		return IncidentDetail{}, ErrInvalidInput
	}
	detail, err := service.store.GetIncident(ctx, id)
	if err != nil {
		return IncidentDetail{}, err
	}
	detail.Presentation = incidentPresentation(detail.Incident)
	for index := range detail.Events {
		if detail.Events[index].FailureEvidence != nil {
			evidence := failure.Complete(*detail.Events[index].FailureEvidence)
			detail.Events[index].FailureEvidence = &evidence
		} else if detail.Events[index].SafeErrorCode != "" {
			evidence := failure.EvidenceForCode(detail.Events[index].SafeErrorCode)
			evidence.Version = 0
			evidence.Level = failure.LevelLegacyPartial
			evidence.OccurredAt = detail.Events[index].ObservedAt
			evidence = failure.Complete(evidence)
			detail.Events[index].FailureEvidence = &evidence
		}
	}
	return detail, nil
}

func (service *AdminService) Acknowledge(ctx context.Context, id uuid.UUID, version int) (Incident, error) {
	if id == uuid.Nil || version < 1 {
		return Incident{}, ErrInvalidInput
	}
	incident, err := service.store.AcknowledgeIncident(ctx, id, version, service.now().UTC())
	if err == nil {
		incident.Presentation = incidentPresentation(incident)
	}
	return incident, err
}

func (service *AdminService) AcceptRisk(ctx context.Context, id uuid.UUID, version int, reason string) (Incident, error) {
	reason = strings.TrimSpace(reason)
	if id == uuid.Nil || version < 1 || !validOperatorReason(reason) {
		return Incident{}, ErrInvalidInput
	}
	incident, err := service.store.AcceptIncidentRisk(ctx, id, version, reason, service.now().UTC())
	if err == nil {
		incident.Presentation = incidentPresentation(incident)
	}
	return incident, err
}

func validOperatorReason(reason string) bool {
	length := len([]rune(reason))
	return length >= 12 && length <= 500 && !unsafeOperatorReason.MatchString(reason)
}

type MonitorStore interface {
	ScanObservations(context.Context, time.Time, int, time.Duration) ([]Observation, error)
	RecordObservations(context.Context, []Observation, time.Time, time.Duration, bool) error
	AdvanceObservationCursors(context.Context, time.Time) error
	AdvanceLifecycle(context.Context, []string, time.Time, bool) error
	MaintenanceActive(context.Context, time.Time) (bool, error)
	ClaimAlert(context.Context, string, time.Duration, time.Time) (Alert, error)
	CompleteAlert(context.Context, uuid.UUID, string, time.Time) error
	RetryAlert(context.Context, uuid.UUID, string, string, time.Time, time.Time, bool) error
}

type Sender interface {
	Send(context.Context, Alert, string) (string, error)
}

type Monitor struct {
	store             MonitorStore
	sender            Sender
	mode              Mode
	workerID          string
	adminIncidentURL  string
	now               func() time.Time
	aggregationWindow time.Duration
	observationSource ObservationSource
}

type ObservationSource interface {
	Observations(time.Time) []Observation
}

func NewMonitor(store MonitorStore, sender Sender, mode Mode, workerID, adminIncidentURL string, now func() time.Time) *Monitor {
	return &Monitor{
		store: store, sender: sender, mode: mode, workerID: workerID, adminIncidentURL: strings.TrimRight(adminIncidentURL, "/"),
		now: now, aggregationWindow: 30 * time.Second,
	}
}

func (monitor *Monitor) ConfigureObservationSource(source ObservationSource) *Monitor {
	monitor.observationSource = source
	return monitor
}

func (monitor *Monitor) Process(ctx context.Context) error {
	if monitor.mode == ModeOff {
		return nil
	}
	now := monitor.now().UTC()
	observations, err := monitor.store.ScanObservations(ctx, now, 500, 5*time.Minute)
	if err != nil {
		return err
	}
	if monitor.observationSource != nil && len(observations) < 500 {
		additional := monitor.observationSource.Observations(now)
		available := 500 - len(observations)
		if len(additional) > available {
			additional = additional[:available]
		}
		observations = append(observations, additional...)
	}
	maintenance, err := monitor.store.MaintenanceActive(ctx, now)
	if err != nil {
		return err
	}
	// Persist outbox intent even during maintenance, but do not contact the
	// provider until the window closes. A transient issue that resolves inside
	// the window becomes unclaimable; an issue that persists is sent once after
	// maintenance instead of being silently lost.
	enqueue := monitor.mode == ModeSend
	allowSend := enqueue && !maintenance
	if err := monitor.store.RecordObservations(ctx, observations, now, monitor.aggregationWindow, enqueue); err != nil {
		return err
	}
	// Commit the scan boundary only after observations are durably recorded.
	// The next scan still overlaps this boundary by five minutes so a slowly
	// committing business transaction cannot be missed.
	if err := monitor.store.AdvanceObservationCursors(ctx, now); err != nil {
		return err
	}
	activeFingerprints := make([]string, 0, len(observations))
	for _, observation := range observations {
		activeFingerprints = append(activeFingerprints, observation.Fingerprint())
	}
	if err := monitor.store.AdvanceLifecycle(ctx, activeFingerprints, now, enqueue); err != nil {
		return err
	}
	if !allowSend || monitor.sender == nil {
		return nil
	}
	for processed := 0; processed < 10; processed++ {
		alert, err := monitor.store.ClaimAlert(ctx, monitor.workerID, time.Minute, now)
		if errors.Is(err, ErrNoAlertReady) {
			return nil
		}
		if err != nil {
			return err
		}
		remoteID, sendErr := monitor.sender.Send(ctx, alert, monitor.adminIncidentURL)
		if sendErr == nil {
			if err := monitor.store.CompleteAlert(ctx, alert.ID, monitor.workerID, now); err != nil {
				return err
			}
			_ = remoteID // Remote identifiers are deliberately not persisted or logged.
			continue
		}
		permanent := IsPermanentSendError(sendErr)
		if err := monitor.store.RetryAlert(ctx, alert.ID, monitor.workerID, SafeSendErrorCode(sendErr), now.Add(time.Minute), now, permanent); err != nil {
			return err
		}
	}
	return nil
}

type SendError struct {
	Code      string
	Permanent bool
}

func (err *SendError) Error() string { return err.Code }
func IsPermanentSendError(err error) bool {
	var sendError *SendError
	return errors.As(err, &sendError) && sendError.Permanent
}
func SafeSendErrorCode(err error) string {
	var sendError *SendError
	if errors.As(err, &sendError) && sendError.Code != "" {
		return sendError.Code
	}
	return "TELEGRAM_NETWORK_ERROR"
}
