package sentinel

import (
	"errors"
	"strings"
	"unicode"
)

type TelegramTenantContextMode string
type TelegramTenantContextStatus string
type TelegramURLStatus string
type TelegramContextResult string

const (
	TelegramTenantContextOff         TelegramTenantContextMode = "off"
	TelegramTenantContextPrivateChat TelegramTenantContextMode = "private_chat"

	TelegramTenantContextDisabled                   TelegramTenantContextStatus = "off"
	TelegramTenantContextPendingVerification        TelegramTenantContextStatus = "pending_verification"
	TelegramTenantContextPrivateVerified            TelegramTenantContextStatus = "private_verified"
	TelegramTenantContextRedactedChatNotPrivate     TelegramTenantContextStatus = "redacted_chat_not_private"
	TelegramTenantContextRedactedVerificationFailed TelegramTenantContextStatus = "redacted_verification_failed"

	TelegramURLAtFailure           TelegramURLStatus = "AT_FAILURE"
	TelegramURLChangedSinceFailure TelegramURLStatus = "CHANGED_SINCE_FAILURE"
	TelegramURLCurrentOnly         TelegramURLStatus = "CURRENT_ONLY"
	TelegramURLUnavailable         TelegramURLStatus = "UNAVAILABLE"

	TelegramContextIncluded              TelegramContextResult = "included"
	TelegramContextNotTenantScoped       TelegramContextResult = "not_tenant_scoped"
	TelegramContextURLUnavailable        TelegramContextResult = "url_unavailable"
	TelegramContextChatNotPrivate        TelegramContextResult = "chat_not_private"
	TelegramContextQueryFailed           TelegramContextResult = "query_failed"
	TelegramContextMessageBudgetExceeded TelegramContextResult = "message_budget_exceeded"
)

var telegramContextMetricResults = [...]TelegramContextResult{
	TelegramContextIncluded,
	TelegramContextNotTenantScoped,
	TelegramContextURLUnavailable,
	TelegramContextChatNotPrivate,
	TelegramContextQueryFailed,
	TelegramContextMessageBudgetExceeded,
}

type TelegramTenantContext struct {
	TenantName  string
	EndpointURL string
	URLStatus   TelegramURLStatus
}

func ParseTelegramTenantContextMode(raw string) (TelegramTenantContextMode, error) {
	switch TelegramTenantContextMode(strings.ToLower(strings.TrimSpace(raw))) {
	case TelegramTenantContextOff:
		return TelegramTenantContextOff, nil
	case TelegramTenantContextPrivateChat:
		return TelegramTenantContextPrivateChat, nil
	default:
		return "", errors.New("TELEGRAM_TENANT_CONTEXT_MODE must be off or private_chat")
	}
}

func sanitizeTelegramTenantName(raw string) string {
	cleaned := strings.Map(func(value rune) rune {
		if unicode.IsControl(value) {
			return ' '
		}
		return value
	}, raw)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	runes := []rune(cleaned)
	if len(runes) <= 120 {
		return cleaned
	}
	return string(runes[:119]) + "…"
}
