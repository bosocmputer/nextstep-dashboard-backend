package sentinel

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type LookupFunc func(string) (string, bool)

type RuntimeConfig struct {
	DatabaseURL      string
	PublicBaseURL    *url.URL
	Mode             Mode
	Interval         time.Duration
	StatePath        string
	RuntimeDirectory string
	TelegramToken    string
	TelegramChatID   string
	TelegramAPIBase  string
	BackupPolicy     BackupPolicy
}

func LoadRuntimeConfig(lookup LookupFunc) (RuntimeConfig, error) {
	databaseURL := strings.TrimSpace(value(lookup, "DATABASE_URL", ""))
	if databaseURL == "" {
		return RuntimeConfig{}, errors.New("DATABASE_URL is required for Sentinel")
	}
	publicBaseURL, err := url.Parse(strings.TrimSpace(value(lookup, "PUBLIC_BASE_URL", "")))
	if err != nil || publicBaseURL.Host == "" || publicBaseURL.Scheme != "https" {
		return RuntimeConfig{}, errors.New("PUBLIC_BASE_URL must be an absolute HTTPS URL for Sentinel")
	}
	mode, err := ParseMode(value(lookup, "OPERATIONAL_ALERTS_MODE", "off"))
	if err != nil {
		return RuntimeConfig{}, err
	}
	backupPolicy, err := ParseBackupPolicy(value(lookup, "BACKUP_POLICY", string(BackupPolicyPreMigrationOnly)))
	if err != nil {
		return RuntimeConfig{}, err
	}
	intervalSeconds, err := strconv.Atoi(value(lookup, "SENTINEL_INTERVAL_SECONDS", "30"))
	if err != nil || intervalSeconds < 15 || intervalSeconds > 300 {
		return RuntimeConfig{}, errors.New("SENTINEL_INTERVAL_SECONDS must be between 15 and 300")
	}
	statePath := strings.TrimSpace(value(lookup, "SENTINEL_STATE_PATH", "/var/lib/nextstep-sentinel/state.json"))
	runtimeDirectory := strings.TrimSpace(value(lookup, "SENTINEL_RUNTIME_DIR", "/run/nextstep-dashboard"))
	if !filepath.IsAbs(statePath) || !filepath.IsAbs(runtimeDirectory) {
		return RuntimeConfig{}, errors.New("Sentinel state and runtime paths must be absolute")
	}
	config := RuntimeConfig{
		DatabaseURL: databaseURL, PublicBaseURL: publicBaseURL, Mode: mode, Interval: time.Duration(intervalSeconds) * time.Second,
		StatePath: statePath, RuntimeDirectory: runtimeDirectory, BackupPolicy: backupPolicy,
		TelegramAPIBase: strings.TrimRight(value(lookup, "TELEGRAM_API_BASE_URL", "https://api.telegram.org"), "/"),
	}
	if mode == ModeSend {
		tokenPath := strings.TrimSpace(value(lookup, "TELEGRAM_BOT_TOKEN_FILE", "/run/secrets/telegram-bot-token"))
		chatPath := strings.TrimSpace(value(lookup, "TELEGRAM_CHAT_ID_FILE", "/run/secrets/telegram-chat-id"))
		config.TelegramToken, err = readProtectedSecret(tokenPath)
		if err != nil {
			return RuntimeConfig{}, fmt.Errorf("read Telegram token file: %w", err)
		}
		config.TelegramChatID, err = readProtectedSecret(chatPath)
		if err != nil {
			return RuntimeConfig{}, fmt.Errorf("read Telegram chat file: %w", err)
		}
		if _, err := NewTelegramClient(config.TelegramToken, config.TelegramChatID, config.TelegramAPIBase, nil); err != nil {
			return RuntimeConfig{}, err
		}
	}
	return config, nil
}

func value(lookup LookupFunc, name, fallback string) string {
	if raw, ok := lookup(name); ok && strings.TrimSpace(raw) != "" {
		return strings.TrimSpace(raw)
	}
	return fallback
}

func readProtectedSecret(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("secret path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("secret file is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("secret file permissions are unsafe")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != 0 {
		return "", errors.New("secret file must be root-owned")
	}
	body, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(body) == 0 || len(body) > 4096 {
		return "", errors.New("secret file content is invalid")
	}
	secret := strings.TrimSpace(string(body))
	if secret == "" || strings.ContainsAny(secret, "\r\n\t ") {
		return "", errors.New("secret file must contain one compact value")
	}
	return secret, nil
}
