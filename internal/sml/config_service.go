package sml

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/google/uuid"
)

type Readiness string

const (
	ReadinessUntested Readiness = "UNTESTED"
	ReadinessReady    Readiness = "READY"
	ReadinessFailed   Readiness = "FAILED"
)

var (
	ErrConnectionNotConfigured   = errors.New("SML connection is not configured")
	ErrConnectionVersionConflict = errors.New("SML connection version conflict")
	configFilenamePattern        = regexp.MustCompile(`^[A-Za-z0-9_.-]+\.xml$`)
)

type ConnectionInput struct {
	EndpointURL     string
	ConfigFileName  string
	DatabaseName    string
	Username        string
	Password        string
	ExpectedVersion int
}

type StoredConnection struct {
	TenantID          uuid.UUID
	EndpointURL       string
	ConfigFileName    string
	DatabaseName      string
	Username          secret.Sealed
	Password          secret.Sealed
	Version           int
	Readiness         Readiness
	LastTestedAt      *time.Time
	LastSafeErrorCode string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ConnectionStatus struct {
	IsConfigured      bool       `json:"isConfigured"`
	EndpointHost      string     `json:"endpointHost"`
	DatabaseName      string     `json:"databaseName"`
	ConfigFileName    string     `json:"configFileName"`
	Readiness         Readiness  `json:"readinessStatus"`
	LastTestedAt      *time.Time `json:"lastTestedAt"`
	LastSafeErrorCode string     `json:"lastSafeErrorCode,omitempty"`
	Version           int        `json:"version"`
}

type ConnectionTestResult struct {
	Status    Readiness `json:"status"`
	TestedAt  time.Time `json:"testedAt"`
	LatencyMS int64     `json:"latencyMs"`
}

type ConnectionTestError struct {
	SafeCode  string
	Retryable bool
}

func (err *ConnectionTestError) Error() string { return err.SafeCode }

type ConnectionStore interface {
	Get(context.Context, uuid.UUID) (StoredConnection, error)
	Put(context.Context, []byte, string, StoredConnection, int, time.Time) (StoredConnection, error)
	MarkTested(context.Context, []byte, string, uuid.UUID, Readiness, string, time.Time) error
}

type QueryClient interface {
	Query(context.Context, Connection, string) ([]map[string]string, error)
}

type ConnectionService struct {
	store  ConnectionStore
	box    *secret.Box
	policy EndpointPolicy
	client QueryClient
	now    func() time.Time
}

func NewConnectionService(store ConnectionStore, box *secret.Box, policy EndpointPolicy, client QueryClient, now func() time.Time) *ConnectionService {
	return &ConnectionService{store: store, box: box, policy: policy, client: client, now: now}
}

func (service *ConnectionService) Replace(ctx context.Context, actorHash []byte, requestID string, tenantID uuid.UUID, input ConnectionInput) (ConnectionStatus, error) {
	normalized, err := service.normalizeInput(ctx, input)
	if err != nil {
		return ConnectionStatus{}, err
	}
	username, err := service.box.Encrypt([]byte(normalized.Username), connectionAAD(tenantID, "username"))
	if err != nil {
		return ConnectionStatus{}, err
	}
	password, err := service.box.Encrypt([]byte(normalized.Password), connectionAAD(tenantID, "password"))
	if err != nil {
		return ConnectionStatus{}, err
	}
	stored, err := service.store.Put(ctx, actorHash, requestID, StoredConnection{
		TenantID: tenantID, EndpointURL: normalized.EndpointURL, ConfigFileName: normalized.ConfigFileName,
		DatabaseName: normalized.DatabaseName, Username: username, Password: password,
	}, normalized.ExpectedVersion, service.now().UTC())
	if err != nil {
		return ConnectionStatus{}, err
	}
	return redactedStatus(stored), nil
}

func (service *ConnectionService) Get(ctx context.Context, tenantID uuid.UUID) (ConnectionStatus, error) {
	stored, err := service.store.Get(ctx, tenantID)
	if err != nil {
		return ConnectionStatus{}, err
	}
	return redactedStatus(stored), nil
}

func (service *ConnectionService) Test(ctx context.Context, actorHash []byte, requestID string, tenantID uuid.UUID) (ConnectionTestResult, error) {
	connection, err := service.Open(ctx, tenantID)
	if err != nil {
		return ConnectionTestResult{}, err
	}
	startedAt := service.now().UTC()
	rows, queryErr := service.client.Query(ctx, connection, "select 1 as ok")
	testedAt := service.now().UTC()
	if queryErr != nil {
		safeCode, retryable := "SML_TEST_FAILED", false
		var safeError *SafeError
		if errors.As(queryErr, &safeError) {
			safeCode, retryable = safeError.Code, safeError.Retryable
		}
		if err := service.store.MarkTested(ctx, actorHash, requestID, tenantID, ReadinessFailed, safeCode, testedAt); err != nil {
			return ConnectionTestResult{}, err
		}
		return ConnectionTestResult{}, &ConnectionTestError{SafeCode: safeCode, Retryable: retryable}
	}
	if len(rows) == 0 || (rows[0]["ok"] != "1" && !strings.EqualFold(rows[0]["ok"], "true")) {
		const safeCode = "SML_TEST_RESULT_INVALID"
		if err := service.store.MarkTested(ctx, actorHash, requestID, tenantID, ReadinessFailed, safeCode, testedAt); err != nil {
			return ConnectionTestResult{}, err
		}
		return ConnectionTestResult{}, &ConnectionTestError{SafeCode: safeCode, Retryable: false}
	}
	if err := service.store.MarkTested(ctx, actorHash, requestID, tenantID, ReadinessReady, "", testedAt); err != nil {
		return ConnectionTestResult{}, err
	}
	return ConnectionTestResult{Status: ReadinessReady, TestedAt: testedAt, LatencyMS: testedAt.Sub(startedAt).Milliseconds()}, nil
}

func (service *ConnectionService) Open(ctx context.Context, tenantID uuid.UUID) (Connection, error) {
	stored, err := service.store.Get(ctx, tenantID)
	if err != nil {
		return Connection{}, err
	}
	username, err := service.box.Decrypt(stored.Username, connectionAAD(tenantID, "username"))
	if err != nil {
		return Connection{}, &ConnectionTestError{SafeCode: "SML_CREDENTIAL_DECRYPT_FAILED", Retryable: false}
	}
	password, err := service.box.Decrypt(stored.Password, connectionAAD(tenantID, "password"))
	if err != nil {
		return Connection{}, &ConnectionTestError{SafeCode: "SML_CREDENTIAL_DECRYPT_FAILED", Retryable: false}
	}
	return Connection{
		EndpointURL: stored.EndpointURL, ConfigFileName: stored.ConfigFileName, DatabaseName: stored.DatabaseName,
		Username: string(username), Password: string(password),
	}, nil
}

func (service *ConnectionService) normalizeInput(ctx context.Context, input ConnectionInput) (ConnectionInput, error) {
	input.EndpointURL = strings.TrimSpace(input.EndpointURL)
	input.ConfigFileName = strings.TrimSpace(input.ConfigFileName)
	input.DatabaseName = strings.TrimSpace(input.DatabaseName)
	input.Username = strings.TrimSpace(input.Username)
	if input.ExpectedVersion < 0 {
		return ConnectionInput{}, validationError("version", "INVALID_VERSION")
	}
	if len(input.EndpointURL) < 1 || len(input.EndpointURL) > 2048 {
		return ConnectionInput{}, validationError("endpointUrl", "INVALID_ENDPOINT")
	}
	resolved, err := service.policy.Resolve(ctx, input.EndpointURL)
	if err != nil {
		return ConnectionInput{}, validationError("endpointUrl", "ENDPOINT_NOT_ALLOWED")
	}
	input.EndpointURL = resolved.URL.String()
	if len(input.ConfigFileName) < 1 || len(input.ConfigFileName) > 128 || !configFilenamePattern.MatchString(input.ConfigFileName) {
		return ConnectionInput{}, validationError("configFileName", "INVALID_CONFIG_FILE")
	}
	if len(input.DatabaseName) < 1 || len(input.DatabaseName) > 160 {
		return ConnectionInput{}, validationError("databaseName", "INVALID_DATABASE")
	}
	if len(input.Username) > 256 {
		return ConnectionInput{}, validationError("username", "INVALID_USERNAME")
	}
	if len(input.Password) > 1024 {
		return ConnectionInput{}, validationError("password", "INVALID_PASSWORD")
	}
	if (input.Username == "") != (input.Password == "") {
		return ConnectionInput{}, validationError("authentication", "INCOMPLETE_BASIC_AUTH")
	}
	if input.Username != "" && resolved.URL.Scheme != "https" && !resolved.IP.IsPrivate() && !resolved.IP.IsLoopback() {
		return ConnectionInput{}, validationError("authentication", "BASIC_AUTH_REQUIRES_HTTPS")
	}
	return input, nil
}

type ValidationError struct {
	Field string
	Code  string
}

func (err *ValidationError) Error() string { return fmt.Sprintf("%s: %s", err.Field, err.Code) }

func validationError(field, code string) *ValidationError {
	return &ValidationError{Field: field, Code: code}
}

func redactedStatus(stored StoredConnection) ConnectionStatus {
	host := ""
	if endpoint, err := url.Parse(stored.EndpointURL); err == nil {
		host = endpoint.Host
	}
	return ConnectionStatus{
		IsConfigured: true, EndpointHost: host, DatabaseName: stored.DatabaseName, ConfigFileName: stored.ConfigFileName,
		Readiness: stored.Readiness, LastTestedAt: stored.LastTestedAt, LastSafeErrorCode: stored.LastSafeErrorCode, Version: stored.Version,
	}
}

func connectionAAD(tenantID uuid.UUID, field string) []byte {
	return []byte(tenantID.String() + ":" + field)
}
