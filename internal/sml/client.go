package sml

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"time"
)

type Connection struct {
	EndpointURL    string
	ConfigFileName string
	DatabaseName   string
	Username       string
	Password       string
}

type SafeError struct {
	Code      string
	Retryable bool
	Phase     TransportPhase
}

func (err *SafeError) Error() string {
	return err.Code
}

type TransportPhase string

const (
	BeforeRequestSent        TransportPhase = "BEFORE_REQUEST_SENT"
	RequestSentResultUnknown TransportPhase = "REQUEST_SENT_RESULT_UNKNOWN"
	ResponseStarted          TransportPhase = "RESPONSE_STARTED"
)

type Client struct {
	policy               EndpointPolicy
	timeout              time.Duration
	maximumResponseBytes int64
	maximumRows          int
}

const (
	defaultConnectTimeout      = 10 * time.Second
	defaultTLSHandshakeTimeout = 10 * time.Second
)

func NewClient(policy EndpointPolicy, timeout time.Duration, maximumResponseBytes int64, maximumRows int) *Client {
	return &Client{policy: policy, timeout: timeout, maximumResponseBytes: maximumResponseBytes, maximumRows: maximumRows}
}

func (client *Client) Query(ctx context.Context, connection Connection, sql string) ([]map[string]string, error) {
	if connection.ConfigFileName == "" || connection.DatabaseName == "" || sql == "" {
		return nil, &SafeError{Code: "SML_CONFIGURATION_INVALID", Retryable: false}
	}
	resolved, err := client.policy.Resolve(ctx, connection.EndpointURL)
	if err != nil {
		return nil, &SafeError{Code: "SML_ENDPOINT_DENIED", Retryable: false}
	}
	compressed, err := CompressPayload([]byte(sql))
	if err != nil {
		return nil, &SafeError{Code: "SML_QUERY_ENCODING_FAILED", Retryable: false}
	}
	envelope := BuildQueryEnvelope("NEXTSTEP", connection.ConfigFileName, connection.DatabaseName, base64.StdEncoding.EncodeToString(compressed))

	requestCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, resolved.URL.String(), bytes.NewBufferString(envelope))
	if err != nil {
		return nil, &SafeError{Code: "SML_REQUEST_INVALID", Retryable: false}
	}
	request.Header.Set("Content-Type", "text/xml; charset=utf-8")
	request.Header.Set("SOAPAction", "")
	if connection.Username != "" || connection.Password != "" {
		request.SetBasicAuth(connection.Username, connection.Password)
	}
	wroteRequest, responseStarted := false, false
	trace := &httptrace.ClientTrace{
		WroteRequest:         func(httptrace.WroteRequestInfo) { wroteRequest = true },
		GotFirstResponseByte: func() { responseStarted = true },
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))

	httpClient, transport := pinnedHTTPClient(resolved, client.timeout)
	defer transport.CloseIdleConnections()
	response, err := httpClient.Do(request)
	if err != nil {
		phase := BeforeRequestSent
		if wroteRequest {
			phase = RequestSentResultUnknown
		}
		if responseStarted {
			phase = ResponseStarted
		}
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return nil, &SafeError{Code: "SML_TIMEOUT", Retryable: phase == BeforeRequestSent, Phase: phase}
		}
		return nil, &SafeError{Code: "SML_UNREACHABLE", Retryable: phase == BeforeRequestSent, Phase: phase}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, client.maximumResponseBytes+1))
	if err != nil {
		return nil, &SafeError{Code: "SML_RESPONSE_READ_FAILED", Retryable: false, Phase: ResponseStarted}
	}
	if int64(len(body)) > client.maximumResponseBytes {
		return nil, &SafeError{Code: "SML_RESPONSE_TOO_LARGE", Retryable: false}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return nil, &SafeError{Code: "SML_HTTP_" + strconv.Itoa(response.StatusCode), Retryable: retryable}
	}
	encodedReturn, err := ExtractSOAPReturn(body)
	if err != nil {
		return nil, &SafeError{Code: "SML_SOAP_INVALID", Retryable: false}
	}
	zippedResult, err := base64.StdEncoding.DecodeString(encodedReturn)
	if err != nil {
		return nil, &SafeError{Code: "SML_RETURN_NOT_BASE64", Retryable: false}
	}
	xmlPayload, err := DecompressPayload(zippedResult, client.maximumResponseBytes*4)
	if err != nil {
		// The HTTP body is already complete. A ZIP decoding failure does not
		// mean the remote query may still be running, so it must not open the
		// tenant uncertainty circuit used for transport timeouts.
		return nil, &SafeError{Code: zipSafeErrorCode(err), Retryable: false}
	}
	rows, err := ParseRows(xmlPayload, client.maximumRows)
	if err != nil {
		return nil, &SafeError{Code: "SML_RESULT_INVALID", Retryable: false}
	}
	return rows, nil
}

func zipSafeErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrZIPFormatInvalid):
		return "SML_ZIP_FORMAT_INVALID"
	case errors.Is(err, ErrZIPEmpty):
		return "SML_ZIP_EMPTY"
	case errors.Is(err, ErrZIPTooLarge):
		return "SML_ZIP_TOO_LARGE"
	case errors.Is(err, ErrZIPReadFailed):
		return "SML_ZIP_READ_FAILED"
	default:
		return "SML_ZIP_INVALID"
	}
}

func pinnedHTTPClient(endpoint ResolvedEndpoint, timeout time.Duration) (*http.Client, *http.Transport) {
	port := endpoint.URL.Port()
	if port == "" {
		if endpoint.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	dialAddress := net.JoinHostPort(endpoint.IP.String(), port)
	// Connection establishment has a short, independent budget. The request
	// deadline may be five minutes for heavy reports, but an unreachable host
	// must not consume that entire budget before any bytes are sent.
	dialer := &net.Dialer{Timeout: defaultConnectTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, dialAddress)
		},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   defaultTLSHandshakeTimeout,
		ResponseHeaderTimeout: timeout,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: endpoint.URL.Hostname(),
		},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("SML redirects are not allowed")
		},
	}
	return httpClient, transport
}
