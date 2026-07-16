package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := sentinel.LoadRuntimeConfig(os.LookupEnv)
	if err != nil || config.Mode != sentinel.ModeSend {
		logger.Error("Sentinel preflight configuration rejected", "safeErrorCode", "SENTINEL_CONFIG_INVALID")
		os.Exit(1)
	}
	client, err := sentinel.NewTelegramClient(config.TelegramToken, config.TelegramChatID, config.TelegramAPIBase, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		logger.Error("Sentinel Telegram configuration rejected", "safeErrorCode", "TELEGRAM_CONFIG_INVALID")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Preflight(ctx); err != nil {
		logger.Error("Sentinel Telegram preflight failed", "safeErrorCode", sentinel.SafeSendErrorCode(err))
		os.Exit(1)
	}
	if os.Getenv("SENTINEL_PREFLIGHT_SEND_TEST") == "true" {
		if err := client.SendPreflightMessage(ctx); err != nil {
			logger.Error("Sentinel Telegram test message failed", "safeErrorCode", sentinel.SafeSendErrorCode(err))
			os.Exit(1)
		}
		logger.Info("Sentinel Telegram preflight passed and one test message was sent")
		return
	}
	logger.Info("Sentinel Telegram preflight passed")
}
