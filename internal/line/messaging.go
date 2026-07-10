package line

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const DefaultPushEndpoint = "https://api.line.me/v2/bot/message/push"

type PushOutcome string

const (
	PushAccepted  PushOutcome = "ACCEPTED"
	PushRetryable PushOutcome = "RETRYABLE"
	PushPermanent PushOutcome = "PERMANENT"
)

type PushResult struct {
	Outcome           PushOutcome
	SafeCode          string
	ProviderRequestID string
	RetryAfter        time.Duration
	Uncertain         bool
}

type MessagingClient struct {
	accessToken string
	endpoint    string
	client      *http.Client
}

func NewMessagingClient(accessToken, endpoint string, timeout time.Duration) *MessagingClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &MessagingClient{
		accessToken: accessToken,
		endpoint:    endpoint,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("LINE push redirects are not allowed")
			},
		},
	}
}

func (client *MessagingClient) Push(ctx context.Context, lineUserID string, retryKey uuid.UUID, message json.RawMessage) PushResult {
	if len(client.accessToken) < 32 || len(lineUserID) < 2 || len(lineUserID) > 128 || retryKey == uuid.Nil || len(message) == 0 || len(message) > maximumFlexPayloadBytes || !json.Valid(message) {
		return PushResult{Outcome: PushPermanent, SafeCode: "LINE_PUSH_INPUT_INVALID"}
	}
	body, err := json.Marshal(struct {
		To       string            `json:"to"`
		Messages []json.RawMessage `json:"messages"`
	}{To: lineUserID, Messages: []json.RawMessage{message}})
	if err != nil {
		return PushResult{Outcome: PushPermanent, SafeCode: "LINE_PUSH_INPUT_INVALID"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return PushResult{Outcome: PushPermanent, SafeCode: "LINE_PUSH_REQUEST_INVALID"}
	}
	request.Header.Set("Authorization", "Bearer "+client.accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Line-Retry-Key", retryKey.String())
	response, err := client.client.Do(request)
	if err != nil {
		return PushResult{Outcome: PushRetryable, SafeCode: "LINE_PUSH_UNCERTAIN", RetryAfter: 30 * time.Second, Uncertain: true}
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
	requestID := strings.TrimSpace(response.Header.Get("X-Line-Request-Id"))
	if len(requestID) > 128 {
		requestID = requestID[:128]
	}
	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
		return PushResult{Outcome: PushAccepted, SafeCode: "LINE_PUSH_ACCEPTED", ProviderRequestID: requestID}
	case response.StatusCode == http.StatusConflict:
		return PushResult{Outcome: PushAccepted, SafeCode: "LINE_PUSH_ALREADY_ACCEPTED", ProviderRequestID: requestID}
	case response.StatusCode == http.StatusTooManyRequests:
		return PushResult{Outcome: PushRetryable, SafeCode: "LINE_PUSH_RATE_LIMITED", ProviderRequestID: requestID, RetryAfter: retryAfter(response.Header.Get("Retry-After"))}
	case response.StatusCode >= 500:
		return PushResult{Outcome: PushRetryable, SafeCode: "LINE_PUSH_UNAVAILABLE", ProviderRequestID: requestID, RetryAfter: 30 * time.Second}
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return PushResult{Outcome: PushPermanent, SafeCode: "LINE_PUSH_AUTH_REJECTED", ProviderRequestID: requestID}
	default:
		return PushResult{Outcome: PushPermanent, SafeCode: "LINE_PUSH_REJECTED", ProviderRequestID: requestID}
	}
}

func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 {
		return 30 * time.Second
	}
	if seconds > 3600 {
		seconds = 3600
	}
	return time.Duration(seconds) * time.Second
}
