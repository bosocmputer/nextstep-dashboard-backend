package sentinel

import (
	"context"
	"errors"
	"net/url"
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
	Status     *Status
	Severity   *Severity
	ActiveOnly bool
	Cursor     string
	PageSize   int
}

type ConnectionReferenceStatus string
type SchemeSecurity string

const (
	ConnectionExactVersion ConnectionReferenceStatus = "EXACT_VERSION"
	ConnectionChanged      ConnectionReferenceStatus = "CHANGED_SINCE_FAILURE"
	ConnectionCurrentOnly  ConnectionReferenceStatus = "CURRENT_ONLY"
	ConnectionUnavailable  ConnectionReferenceStatus = "UNAVAILABLE"
	SchemeHTTP             SchemeSecurity            = "HTTP"
	SchemeHTTPS            SchemeSecurity            = "HTTPS"
)

type SMLConnectionReference struct {
	EndpointURLAtFailure string                    `json:"endpointUrlAtFailure,omitempty"`
	CurrentEndpointURL   string                    `json:"currentEndpointUrl,omitempty"`
	EndpointHost         string                    `json:"endpointHost,omitempty"`
	VersionAtFailure     *int                      `json:"versionAtFailure,omitempty"`
	CurrentVersion       *int                      `json:"currentVersion,omitempty"`
	Status               ConnectionReferenceStatus `json:"status"`
	SchemeSecurity       SchemeSecurity            `json:"schemeSecurity,omitempty"`
	TestAvailableAt      *time.Time                `json:"testAvailableAt,omitempty"`
	TestBlockedReason    string                    `json:"testBlockedReason,omitempty"`
}

type IncidentOccurrence struct {
	ID                  uuid.UUID               `json:"id"`
	TenantID            uuid.UUID               `json:"tenantId,omitempty"`
	TenantName          string                  `json:"tenantName,omitempty"`
	ReportKey           string                  `json:"reportKey,omitempty"`
	SourceKind          SourceKind              `json:"sourceKind"`
	SafeErrorCode       string                  `json:"safeErrorCode"`
	ObservedAt          time.Time               `json:"observedAt"`
	FailureEvidence     *failure.Evidence       `json:"failureEvidence,omitempty"`
	Impact              *failure.Impact         `json:"impact,omitempty"`
	ConnectionReference *SMLConnectionReference `json:"smlConnectionReference,omitempty"`
}

type OccurrenceFilter struct {
	Cursor   string
	PageSize int
}
type OccurrencePage struct {
	Data       []IncidentOccurrence `json:"data"`
	NextCursor string               `json:"nextCursor,omitempty"`
	HasMore    bool                 `json:"hasMore"`
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
	ID                    uuid.UUID
	Kind                  string
	Incident              Incident
	TenantContexts        []TelegramTenantContext
	AdditionalTenantCount int
	TenantContextResult   TelegramContextResult
}

type AdminStore interface {
	ListIncidents(context.Context, IncidentFilter) (IncidentPage, error)
	GetIncident(context.Context, uuid.UUID) (IncidentDetail, error)
	ListIncidentOccurrences(context.Context, uuid.UUID, OccurrenceFilter) (OccurrencePage, error)
	AcknowledgeIncident(context.Context, uuid.UUID, int, time.Time) (Incident, error)
	AcceptIncidentRisk(context.Context, uuid.UUID, int, string, time.Time) (Incident, error)
}

func (service *AdminService) Occurrences(ctx context.Context, id uuid.UUID, filter OccurrenceFilter) (OccurrencePage, error) {
	if id == uuid.Nil {
		return OccurrencePage{}, ErrInvalidInput
	}
	if filter.PageSize == 0 {
		filter.PageSize = 50
	}
	if filter.PageSize < 1 || filter.PageSize > 100 {
		return OccurrencePage{}, ErrInvalidInput
	}
	page, err := service.store.ListIncidentOccurrences(ctx, id, filter)
	if err != nil {
		return OccurrencePage{}, err
	}
	for index := range page.Data {
		occurrence := &page.Data[index]
		if occurrence.FailureEvidence != nil {
			completed := failure.Complete(*occurrence.FailureEvidence)
			occurrence.FailureEvidence = &completed
		}
		if occurrence.ConnectionReference != nil {
			occurrence.ConnectionReference = sanitizeConnectionReference(*occurrence.ConnectionReference)
		}
	}
	return page, nil
}

func sanitizeConnectionReference(reference SMLConnectionReference) *SMLConnectionReference {
	reference.EndpointURLAtFailure = sanitizeEndpointURL(reference.EndpointURLAtFailure)
	reference.CurrentEndpointURL = sanitizeEndpointURL(reference.CurrentEndpointURL)
	if reference.EndpointURLAtFailure == "" && reference.CurrentEndpointURL != "" {
		reference.Status = ConnectionCurrentOnly
	}
	if reference.EndpointURLAtFailure == "" && reference.CurrentEndpointURL == "" {
		reference.Status = ConnectionUnavailable
	}
	visible := reference.EndpointURLAtFailure
	if visible == "" {
		visible = reference.CurrentEndpointURL
	}
	parsed, err := url.Parse(visible)
	if err != nil || parsed.Hostname() == "" {
		reference.Status = ConnectionUnavailable
		reference.EndpointURLAtFailure = ""
		reference.CurrentEndpointURL = ""
		return &reference
	}
	reference.EndpointHost = parsed.Hostname()
	if parsed.Scheme == "https" {
		reference.SchemeSecurity = SchemeHTTPS
	} else {
		reference.SchemeSecurity = SchemeHTTP
	}
	return &reference
}

func sanitizeEndpointURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	clean := parsed.String()
	if len(clean) > 2048 {
		return ""
	}
	return clean
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

// PresentIncident applies the same Thai failure catalog used by the cursor
// endpoint to additive numbered table-query results.
func PresentIncident(item Incident) Incident {
	item.Presentation = incidentPresentation(item)
	return item
}

func SanitizeOccurrenceConnectionReference(reference *SMLConnectionReference) *SMLConnectionReference {
	if reference == nil {
		return nil
	}
	return sanitizeConnectionReference(*reference)
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
	for index := range detail.CauseBreakdown {
		entry := &detail.CauseBreakdown[index]
		entry.InvestigationScope = investigationScope(entry.Category)
		entry.AffectedLabelTH = affectedLabel(entry.SubjectType)
	}
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

func investigationScope(category failure.Category) InvestigationScope {
	switch category {
	case failure.CategoryJavaWSConnectivity, failure.CategoryJavaWSResponse:
		return ScopeCustomerSystem
	case failure.CategorySMLConfiguration:
		return ScopeConfiguration
	case failure.CategoryLineDelivery:
		return ScopeLineProvider
	case failure.CategoryPlatform, failure.CategoryCapacity, failure.CategoryQueueWorker:
		return ScopeNextstepPlatform
	default:
		return ScopeUnknown
	}
}

func affectedLabel(subject SubjectType) string {
	switch subject {
	case SubjectTenant:
		return "ร้านที่ได้รับผล"
	case SubjectDatabase:
		return "ฐานข้อมูลที่ได้รับผล"
	case SubjectContainer:
		return "บริการระบบที่ได้รับผล"
	case SubjectLineProvider:
		return "ผู้ให้บริการ LINE ที่ได้รับผล"
	default:
		return "ทรัพยากร Server ที่ต้องตรวจสอบ"
	}
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
	AdvanceLifecycle(context.Context, []Observation, bool, time.Time, bool) error
	MaintenanceActive(context.Context, time.Time) (bool, error)
	ClaimAlert(context.Context, string, time.Duration, time.Time, bool) (Alert, error)
	CompleteAlert(context.Context, uuid.UUID, string, time.Time) error
	RetryAlert(context.Context, uuid.UUID, string, string, time.Time, time.Time, bool) error
}

type Sender interface {
	Send(context.Context, Alert, string) (string, error)
}

type tenantContextSender interface {
	Sender
	TenantContextAllowed() bool
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
	continuousSnapshotComplete := len(observations) < 500
	if err := monitor.store.AdvanceLifecycle(ctx, observations, continuousSnapshotComplete, now, enqueue); err != nil {
		return err
	}
	if !allowSend || monitor.sender == nil {
		return nil
	}
	for processed := 0; processed < 10; processed++ {
		includeTenantContext := false
		if sender, ok := monitor.sender.(tenantContextSender); ok {
			includeTenantContext = sender.TenantContextAllowed()
		}
		alert, err := monitor.store.ClaimAlert(ctx, monitor.workerID, time.Minute, now, includeTenantContext)
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
