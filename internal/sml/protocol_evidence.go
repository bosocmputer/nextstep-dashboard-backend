package sml

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
	"sync"
	"time"
)

const nextstepRequestRefHeader = "X-Nextstep-Request-Ref"

type ProtocolEvidence struct {
	RequestRef              string     `json:"requestRef"`
	RequestCount            int        `json:"requestCount"`
	RetryCount              int        `json:"retryCount"`
	RequestSentAt           *time.Time `json:"requestSentAt,omitempty"`
	FirstResponseByteAt     *time.Time `json:"firstResponseByteAt,omitempty"`
	ResponseCompletedAt     *time.Time `json:"responseCompletedAt,omitempty"`
	HTTPStatus              *int       `json:"httpStatus,omitempty"`
	ResponseContentType     string     `json:"responseContentType,omitempty"`
	ResponseBodyBytes       *int64     `json:"responseBodyBytes,omitempty"`
	SOAPValid               *bool      `json:"soapValid,omitempty"`
	SOAPReturnCharacters    *int       `json:"soapReturnCharacters,omitempty"`
	Base64Valid             *bool      `json:"base64Valid,omitempty"`
	DecodedPayloadBytes     *int64     `json:"decodedPayloadBytes,omitempty"`
	ZIPSignatureValid       *bool      `json:"zipSignatureValid,omitempty"`
	ResponseSHA256          string     `json:"responseSha256,omitempty"`
	TenantConcurrentQueries int        `json:"tenantConcurrentQueries"`
	HostConcurrentQueries   int        `json:"hostConcurrentQueries"`
}

type ProtocolRecorder struct {
	mu       sync.Mutex
	evidence ProtocolEvidence
}

type protocolRecorderContextKey struct{}

func NewProtocolRecorder(ctx context.Context) (*ProtocolRecorder, context.Context, error) {
	if ctx == nil {
		return nil, nil, errors.New("protocol evidence context is required")
	}
	ref, err := newRequestReference()
	if err != nil {
		return nil, nil, err
	}
	recorder := &ProtocolRecorder{evidence: ProtocolEvidence{RequestRef: ref}}
	return recorder, context.WithValue(ctx, protocolRecorderContextKey{}, recorder), nil
}

func newRequestReference() (string, error) {
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("generate JavaWS request reference")
	}
	return "NXR-" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

func protocolRecorder(ctx context.Context) *ProtocolRecorder {
	recorder, _ := ctx.Value(protocolRecorderContextKey{}).(*ProtocolRecorder)
	return recorder
}

func (recorder *ProtocolRecorder) requestSent(at time.Time) {
	recorder.mutate(func(evidence *ProtocolEvidence) {
		evidence.RequestCount++
		at = at.UTC()
		evidence.RequestSentAt = &at
	})
}

func (recorder *ProtocolRecorder) firstResponseByte(at time.Time) {
	recorder.mutate(func(evidence *ProtocolEvidence) {
		at = at.UTC()
		evidence.FirstResponseByteAt = &at
	})
}

func (recorder *ProtocolRecorder) mutate(update func(*ProtocolEvidence)) {
	if recorder == nil {
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	update(&recorder.evidence)
}

func (recorder *ProtocolRecorder) SetConcurrency(tenant, host int) {
	recorder.mutate(func(evidence *ProtocolEvidence) {
		evidence.TenantConcurrentQueries = max(0, tenant)
		evidence.HostConcurrentQueries = max(0, host)
	})
}

func (recorder *ProtocolRecorder) Snapshot() ProtocolEvidence {
	if recorder == nil {
		return ProtocolEvidence{}
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.evidence
}

func normalizeContentType(value string) string {
	value = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
	if len(value) > 160 {
		return ""
	}
	return value
}

func hasZIPSignature(payload []byte) bool {
	return len(payload) >= 4 && payload[0] == 'P' && payload[1] == 'K' &&
		((payload[2] == 3 && payload[3] == 4) || (payload[2] == 5 && payload[3] == 6) || (payload[2] == 7 && payload[3] == 8))
}
