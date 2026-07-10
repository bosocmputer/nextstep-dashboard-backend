package line

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultIDTokenVerifyEndpoint = "https://api.line.me/oauth2/v2.1/verify"

type SafeError struct {
	Code      string
	Retryable bool
}

func (err *SafeError) Error() string { return err.Code }

type Identity struct {
	Subject     string
	DisplayName string
	ExpiresAt   time.Time
}

type IDTokenVerifier struct {
	channelID string
	endpoint  string
	client    *http.Client
	now       func() time.Time
}

func NewIDTokenVerifier(channelID, endpoint string, timeout time.Duration, now func() time.Time) *IDTokenVerifier {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &IDTokenVerifier{
		channelID: channelID,
		endpoint:  endpoint,
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("LINE verification redirects are not allowed")
			},
		},
		now: now,
	}
}

func (verifier *IDTokenVerifier) Verify(ctx context.Context, idToken string) (Identity, error) {
	if verifier.channelID == "" {
		return Identity{}, &SafeError{Code: "LINE_LOGIN_NOT_CONFIGURED", Retryable: false}
	}
	if len(idToken) < 16 || len(idToken) > 8192 {
		return Identity{}, &SafeError{Code: "LINE_ID_TOKEN_INVALID", Retryable: false}
	}
	form := url.Values{"id_token": {idToken}, "client_id": {verifier.channelID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, verifier.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, &SafeError{Code: "LINE_VERIFY_REQUEST_INVALID", Retryable: false}
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := verifier.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return Identity{}, ctx.Err()
		}
		return Identity{}, &SafeError{Code: "LINE_VERIFY_UNAVAILABLE", Retryable: true}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil {
		return Identity{}, &SafeError{Code: "LINE_VERIFY_RESPONSE_INVALID", Retryable: true}
	}
	if len(body) > 64*1024 {
		return Identity{}, &SafeError{Code: "LINE_VERIFY_RESPONSE_TOO_LARGE", Retryable: false}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		code := "LINE_ID_TOKEN_INVALID"
		if retryable {
			code = "LINE_VERIFY_UNAVAILABLE"
		}
		return Identity{}, &SafeError{Code: code, Retryable: retryable}
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Subject  string `json:"sub"`
		Audience string `json:"aud"`
		Expires  int64  `json:"exp"`
		IssuedAt int64  `json:"iat"`
		Name     string `json:"name"`
	}
	if err := json.Unmarshal(body, &claims); err != nil {
		return Identity{}, &SafeError{Code: "LINE_VERIFY_RESPONSE_INVALID", Retryable: false}
	}
	now := verifier.now().UTC()
	if claims.Issuer != "https://access.line.me" || claims.Audience != verifier.channelID || len(claims.Subject) < 2 || len(claims.Subject) > 128 || claims.Expires <= now.Unix() || claims.IssuedAt > now.Add(5*time.Minute).Unix() {
		return Identity{}, &SafeError{Code: "LINE_ID_TOKEN_INVALID", Retryable: false}
	}
	displayName := strings.TrimSpace(claims.Name)
	if len(displayName) > 255 {
		displayName = displayName[:255]
	}
	return Identity{Subject: claims.Subject, DisplayName: displayName, ExpiresAt: time.Unix(claims.Expires, 0).UTC()}, nil
}
