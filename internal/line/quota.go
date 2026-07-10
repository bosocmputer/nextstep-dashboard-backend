package line

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

const (
	DefaultQuotaEndpoint            = "https://api.line.me/v2/bot/message/quota"
	DefaultQuotaConsumptionEndpoint = "https://api.line.me/v2/bot/message/quota/consumption"
)

var ErrQuotaUnavailable = errors.New("LINE quota is unavailable")

type QuotaUsage struct {
	Limit    *int
	Consumed int
}

type QuotaClient struct {
	accessToken         string
	quotaEndpoint       string
	consumptionEndpoint string
	client              *http.Client
}

func NewQuotaClient(accessToken, quotaEndpoint, consumptionEndpoint string, timeout time.Duration) *QuotaClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return &QuotaClient{
		accessToken: accessToken, quotaEndpoint: quotaEndpoint, consumptionEndpoint: consumptionEndpoint,
		client: &http.Client{
			Transport: transport, Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("LINE quota redirects are not allowed") },
		},
	}
}

func (client *QuotaClient) Fetch(ctx context.Context) (QuotaUsage, error) {
	if len(client.accessToken) < 32 {
		return QuotaUsage{}, ErrQuotaUnavailable
	}
	var limitResponse struct {
		Type  string `json:"type"`
		Value *int   `json:"value"`
	}
	if err := client.getJSON(ctx, client.quotaEndpoint, &limitResponse); err != nil {
		return QuotaUsage{}, err
	}
	var limit *int
	switch limitResponse.Type {
	case "none":
	case "limited":
		if limitResponse.Value == nil || *limitResponse.Value < 0 {
			return QuotaUsage{}, ErrQuotaUnavailable
		}
		value := *limitResponse.Value
		limit = &value
	default:
		return QuotaUsage{}, ErrQuotaUnavailable
	}
	var consumption struct {
		TotalUsage int `json:"totalUsage"`
	}
	if err := client.getJSON(ctx, client.consumptionEndpoint, &consumption); err != nil || consumption.TotalUsage < 0 {
		return QuotaUsage{}, ErrQuotaUnavailable
	}
	return QuotaUsage{Limit: limit, Consumed: consumption.TotalUsage}, nil
}

func (client *QuotaClient) getJSON(ctx context.Context, endpoint string, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ErrQuotaUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+client.accessToken)
	request.Header.Set("Accept", "application/json")
	response, err := client.client.Do(request)
	if err != nil {
		return ErrQuotaUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return ErrQuotaUnavailable
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(destination); err != nil {
		return ErrQuotaUnavailable
	}
	return nil
}
