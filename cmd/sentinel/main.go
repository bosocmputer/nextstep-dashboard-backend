package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/database"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	config, err := sentinel.LoadRuntimeConfig(os.LookupEnv)
	if err != nil {
		logger.Error("invalid Sentinel configuration", "safeErrorCode", "SENTINEL_CONFIG_INVALID")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := database.OpenSentinelPool(ctx, config.DatabaseURL)
	if err != nil {
		logger.Error("Sentinel database configuration rejected", "safeErrorCode", "DATABASE_CONFIG_INVALID")
		os.Exit(1)
	}
	defer pool.Close()
	store := database.NewSentinelStore(pool)
	workerID := fmt.Sprintf("sentinel-%d", os.Getpid())
	adminURL := config.PublicBaseURL.String() + "/admin/operational-incidents"
	var sender sentinel.Sender
	var emergency *sentinel.EmergencyLane
	if config.Mode == sentinel.ModeSend {
		telegram, telegramErr := sentinel.NewTelegramClient(config.TelegramToken, config.TelegramChatID, config.TelegramAPIBase, &http.Client{Timeout: 10 * time.Second})
		if telegramErr != nil {
			logger.Error("Sentinel Telegram configuration rejected", "safeErrorCode", "TELEGRAM_CONFIG_INVALID")
			os.Exit(1)
		}
		sender = telegram
		emergency = sentinel.NewEmergencyLane(sentinel.NewEmergencyStateStore(config.StatePath), telegram, adminURL)
	}
	monitor := sentinel.NewMonitor(store, sender, config.Mode, workerID, adminURL, time.Now).
		ConfigureObservationSource(sentinel.NewRuntimeProbeSource(config.RuntimeDirectory).ConfigureBackupPolicy(config.BackupPolicy))
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()
	databaseFailures, databaseSuccesses := 0, 0
	for {
		evaluationStarted := time.Now()
		evaluationSucceeded, databaseReachable := evaluate(ctx, monitor, pool)
		evaluationDuration := time.Since(evaluationStarted)
		logger.Info("Sentinel evaluation completed", "result", evaluationSucceeded, "databaseReachable", databaseReachable, "durationMs", evaluationDuration.Milliseconds())
		if databaseReachable {
			databaseFailures = 0
			databaseSuccesses++
			if databaseSuccesses >= 2 && emergency != nil {
				recoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
				recoveryErr := emergency.DatabaseRecovered(recoveryCtx, store, time.Now().UTC())
				cancel()
				if recoveryErr != nil && !errors.Is(recoveryErr, context.Canceled) {
					logger.Warn("Sentinel recovery reconciliation deferred", "safeErrorCode", sentinel.SafeSendErrorCode(recoveryErr))
				}
			}
		} else {
			databaseSuccesses = 0
			databaseFailures++
			if databaseFailures >= 2 && emergency != nil {
				emergencyCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
				emergencyErr := emergency.DatabaseUnavailable(emergencyCtx, time.Now().UTC())
				cancel()
				if emergencyErr != nil && !errors.Is(emergencyErr, context.Canceled) {
					logger.Error("Sentinel emergency lane failed", "safeErrorCode", sentinel.SafeSendErrorCode(emergencyErr))
				}
			}
		}
		if !evaluationSucceeded {
			logger.Warn("Sentinel evaluation incomplete", "safeErrorCode", "SENTINEL_EVALUATION_FAILED", "databaseReachable", databaseReachable)
		}
		if err := sentinel.WriteMonitorHeartbeat(config.RuntimeDirectory, sentinel.MonitorHeartbeat{
			Version: 1, CheckedAt: time.Now().UTC(), Mode: config.Mode, DatabaseReachable: databaseReachable,
			LastEvaluationSucceeded: evaluationSucceeded, EvaluationDurationMs: evaluationDuration.Milliseconds(),
		}); err != nil {
			logger.Warn("Sentinel heartbeat write failed", "safeErrorCode", "SENTINEL_HEARTBEAT_WRITE_FAILED")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type pinger interface{ Ping(context.Context) error }

func evaluate(ctx context.Context, monitor *sentinel.Monitor, database pinger) (bool, bool) {
	evaluationCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	err := monitor.Process(evaluationCtx)
	cancel()
	if err == nil {
		return true, true
	}
	pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
	pingErr := database.Ping(pingCtx)
	pingCancel()
	return false, pingErr == nil
}
