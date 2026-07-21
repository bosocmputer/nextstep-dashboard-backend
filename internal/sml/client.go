package sml

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
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
	Code             string
	Retryable        bool
	Phase            TransportPhase
	ProtocolEvidence *ProtocolEvidence
}

func (err *SafeError) Error() string {
	return err.Code
}

func protocolSafeError(recorder *ProtocolRecorder, code string, retryable bool, phase TransportPhase) *SafeError {
	error := &SafeError{Code: code, Retryable: retryable, Phase: phase}
	if recorder != nil {
		evidence := recorder.Snapshot()
		if evidence.RequestRef != "" {
			error.ProtocolEvidence = &evidence
		}
	}
	return error
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
	recorder := protocolRecorder(ctx)
	if recorder == nil {
		var err error
		recorder, ctx, err = NewProtocolRecorder(ctx)
		if err != nil {
			return nil, &SafeError{Code: "SML_REQUEST_INVALID", Retryable: false}
		}
	}
	if connection.ConfigFileName == "" || connection.DatabaseName == "" || sql == "" {
		return nil, protocolSafeError(recorder, "SML_CONFIGURATION_INVALID", false, "")
	}
	resolved, err := client.policy.Resolve(ctx, connection.EndpointURL)
	if err != nil {
		return nil, protocolSafeError(recorder, "SML_ENDPOINT_DENIED", false, "")
	}
	compressed, err := CompressPayload([]byte(sql))
	if err != nil {
		return nil, protocolSafeError(recorder, "SML_QUERY_ENCODING_FAILED", false, "")
	}
	envelope := BuildQueryEnvelope("NEXTSTEP", connection.ConfigFileName, connection.DatabaseName, base64.StdEncoding.EncodeToString(compressed))

	requestCtx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, resolved.URL.String(), bytes.NewBufferString(envelope))
	if err != nil {
		return nil, protocolSafeError(recorder, "SML_REQUEST_INVALID", false, "")
	}
	request.Header.Set("Content-Type", "text/xml; charset=utf-8")
	request.Header.Set("SOAPAction", "")
	request.Header.Set(nextstepRequestRefHeader, recorder.Snapshot().RequestRef)
	if connection.Username != "" || connection.Password != "" {
		request.SetBasicAuth(connection.Username, connection.Password)
	}
	wroteRequest, responseStarted := false, false
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) {
			if !wroteRequest {
				wroteRequest = true
				recorder.requestSent(time.Now())
			}
		},
		GotFirstResponseByte: func() {
			responseStarted = true
			recorder.firstResponseByte(time.Now())
		},
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
			return nil, protocolSafeError(recorder, "SML_TIMEOUT", phase == BeforeRequestSent, phase)
		}
		return nil, protocolSafeError(recorder, "SML_UNREACHABLE", phase == BeforeRequestSent, phase)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, client.maximumResponseBytes+1))
	if err != nil {
		return nil, protocolSafeError(recorder, "SML_RESPONSE_READ_FAILED", false, ResponseStarted)
	}
	completedAt := time.Now().UTC()
	responseBytes := int64(len(body))
	status := response.StatusCode
	digest := sha256.Sum256(body)
	recorder.mutate(func(evidence *ProtocolEvidence) {
		evidence.ResponseCompletedAt = &completedAt
		evidence.HTTPStatus = &status
		evidence.ResponseContentType = normalizeContentType(response.Header.Get("Content-Type"))
		evidence.ResponseBodyBytes = &responseBytes
		evidence.ResponseSHA256 = hex.EncodeToString(digest[:])
	})
	if int64(len(body)) > client.maximumResponseBytes {
		return nil, protocolSafeError(recorder, "SML_RESPONSE_TOO_LARGE", false, ResponseStarted)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return nil, protocolSafeError(recorder, "SML_HTTP_"+strconv.Itoa(response.StatusCode), retryable, ResponseStarted)
	}
	encodedReturn, err := ExtractSOAPReturn(body)
	if err != nil {
		valid := false
		recorder.mutate(func(evidence *ProtocolEvidence) { evidence.SOAPValid = &valid })
		return nil, protocolSafeError(recorder, "SML_SOAP_INVALID", false, ResponseStarted)
	}
	soapValid, soapCharacters := true, len(encodedReturn)
	recorder.mutate(func(evidence *ProtocolEvidence) {
		evidence.SOAPValid = &soapValid
		evidence.SOAPReturnCharacters = &soapCharacters
	})
	zippedResult, err := base64.StdEncoding.DecodeString(encodedReturn)
	if err != nil {
		valid := false
		recorder.mutate(func(evidence *ProtocolEvidence) { evidence.Base64Valid = &valid })
		return nil, protocolSafeError(recorder, "SML_RETURN_NOT_BASE64", false, ResponseStarted)
	}
	base64Valid, zipValid, decodedBytes := true, hasZIPSignature(zippedResult), int64(len(zippedResult))
	recorder.mutate(func(evidence *ProtocolEvidence) {
		evidence.Base64Valid = &base64Valid
		evidence.DecodedPayloadBytes = &decodedBytes
		evidence.ZIPSignatureValid = &zipValid
	})
	xmlPayload, err := DecompressPayload(zippedResult, client.maximumResponseBytes*4)
	if err != nil {
		// The HTTP body is already complete. A ZIP decoding failure does not
		// mean the remote query may still be running, so it must not open the
		// tenant uncertainty circuit used for transport timeouts.
		return nil, protocolSafeError(recorder, zipSafeErrorCode(err), false, ResponseStarted)
	}
	rows, err := ParseRows(xmlPayload, client.maximumRows)
	if err != nil {
		return nil, protocolSafeError(recorder, "SML_RESULT_INVALID", false, ResponseStarted)
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
