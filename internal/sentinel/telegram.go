package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	telegramTokenPattern = regexp.MustCompile(`^[0-9]{5,16}:[A-Za-z0-9_-]{24,240}$`)
	telegramChatPattern  = regexp.MustCompile(`^-?[0-9]{1,20}$`)
)

type TelegramClient struct {
	token   string
	chatID  string
	baseURL string
	http    *http.Client
}

func NewTelegramClient(token, chatID, baseURL string, client *http.Client) (*TelegramClient, error) {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if !telegramTokenPattern.MatchString(token) {
		return nil, errors.New("Telegram token is invalid")
	}
	if !telegramChatPattern.MatchString(chatID) {
		return nil, errors.New("Telegram chat identifier is invalid")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		return nil, errors.New("Telegram API base URL is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TelegramClient{token: token, chatID: chatID, baseURL: baseURL, http: client}, nil
}

func (client *TelegramClient) Send(ctx context.Context, alert Alert, adminIncidentURL string) (string, error) {
	requestBody := map[string]any{
		"chat_id":                  client.chatID,
		"text":                     TelegramMessage(alert, adminIncidentURL),
		"disable_web_page_preview": true,
	}
	var response struct {
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
		Result    struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := client.call(ctx, "sendMessage", requestBody, &response); err != nil {
		return "", err
	}
	if !response.OK || response.Result.MessageID == 0 {
		return "", telegramAPIError(response.ErrorCode)
	}
	return strconv.FormatInt(response.Result.MessageID, 10), nil
}

func (client *TelegramClient) Preflight(ctx context.Context) error {
	var me struct {
		OK bool `json:"ok"`
	}
	if err := client.call(ctx, "getMe", struct{}{}, &me); err != nil || !me.OK {
		if err != nil {
			return err
		}
		return &SendError{Code: "TELEGRAM_GET_ME_REJECTED", Permanent: true}
	}
	var chat struct {
		OK bool `json:"ok"`
	}
	if err := client.call(ctx, "getChat", map[string]string{"chat_id": client.chatID}, &chat); err != nil || !chat.OK {
		if err != nil {
			return err
		}
		return &SendError{Code: "TELEGRAM_GET_CHAT_REJECTED", Permanent: true}
	}
	return nil
}

// SendPreflightMessage sends one fixed, non-customer test message. Callers must
// require an explicit operator flag so a routine preflight cannot create noise.
func (client *TelegramClient) SendPreflightMessage(ctx context.Context) error {
	requestBody := map[string]any{
		"chat_id": client.chatID, "text": "Nextstep Sentinel preflight ผ่าน · ยังไม่ได้เปิดการแจ้งเตือน Production",
		"disable_web_page_preview": true,
	}
	var response struct {
		OK        bool `json:"ok"`
		ErrorCode int  `json:"error_code"`
	}
	if err := client.call(ctx, "sendMessage", requestBody, &response); err != nil {
		return err
	}
	if !response.OK {
		return telegramAPIError(response.ErrorCode)
	}
	return nil
}

func (client *TelegramClient) call(ctx context.Context, method string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return &SendError{Code: "TELEGRAM_REQUEST_INVALID", Permanent: true}
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * 250 * time.Millisecond
			select {
			case <-ctx.Done():
				return &SendError{Code: "TELEGRAM_NETWORK_ERROR", Permanent: false}
			case <-time.After(delay):
			}
		}
		lastErr = client.callOnce(ctx, method, body, output)
		if lastErr == nil || IsPermanentSendError(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func (client *TelegramClient) callOnce(ctx context.Context, method string, body []byte, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/bot"+client.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return &SendError{Code: "TELEGRAM_REQUEST_INVALID", Permanent: true}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.http.Do(request)
	if err != nil {
		return &SendError{Code: "TELEGRAM_NETWORK_ERROR", Permanent: false}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return telegramHTTPError(response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 64*1024))
	if err := decoder.Decode(output); err != nil {
		return &SendError{Code: "TELEGRAM_RESPONSE_INVALID", Permanent: false}
	}
	return nil
}

func telegramHTTPError(status int) error {
	switch {
	case status == http.StatusRequestTimeout, status == http.StatusTooManyRequests, status >= 500:
		return &SendError{Code: fmt.Sprintf("TELEGRAM_HTTP_%d", status), Permanent: false}
	default:
		return &SendError{Code: fmt.Sprintf("TELEGRAM_HTTP_%d", status), Permanent: status >= 400 && status < 500}
	}
}

func telegramAPIError(code int) error {
	if code == 0 {
		return &SendError{Code: "TELEGRAM_API_REJECTED", Permanent: false}
	}
	return telegramHTTPError(code)
}
