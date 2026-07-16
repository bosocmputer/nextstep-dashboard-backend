package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/config"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/database"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/delivery"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/notification"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/quota"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/retention"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		logger.Error("invalid worker configuration", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := database.OpenPool(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConnections, cfg.DatabaseMinConnections)
	if err != nil {
		logger.Error("create database pool", "error", "database configuration rejected")
		os.Exit(1)
	}
	defer pool.Close()
	box, err := secret.NewBox(cfg.EncryptionMasterKey, cfg.EncryptionKeyID, rand.Reader)
	if err != nil {
		logger.Error("create secret box", "error", "encryption configuration rejected")
		os.Exit(1)
	}
	policy := sml.EndpointPolicy{AllowedPrefixes: cfg.SMLAllowedPrefixes, AllowedHosts: cfg.SMLAllowedHosts, AllowPublicEndpoints: cfg.SMLAllowPublicEndpoints, AllowedPorts: cfg.SMLAllowedPorts}
	connections := sml.NewConnectionService(database.NewSMLConnectionStore(pool), box, policy, nil, time.Now)
	// The HTTP transport is a hard ceiling. Report-level contexts impose the
	// lower 60s/120s/5m total budgets and are shared by current+comparison.
	reportClient := sml.NewClient(policy, 5*time.Minute, 32*1024*1024, 200_000)
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	reportStore := database.NewReportStore(pool).ConfigureGenerationCache(cfg.GenerationCacheEnabled)
	reportWorker := worker.NewReportWorker(reportStore, connections, reportClient, workerID, time.Now).
		ConfigureSummaryQueries(cfg.SummaryQueryEnabled).
		ConfigureHeavyChunks(cfg.HeavyChunkEnabled, cfg.ScheduleChunkEnabled, cfg.HeavyChunkTenantReports)
	schedulerID := workerID + "-scheduler"
	periodObserver := func(preset report.Preset, mode report.ParameterKind, result string) {
		logger.Info("schedule period resolved", "event", "schedule_period_resolution", "preset", preset, "mode", mode, "result", result, "schedulePeriodResolutionTotal", 1)
	}
	dueWorker := schedule.NewDueWorker(database.NewScheduleStore(pool).ConfigureSmartPeriods(cfg.SmartSchedulePeriodsEnabled, cfg.SmartSchedulePeriodTenantIDs, periodObserver), schedulerID, time.Now)
	sessionManager, err := auth.NewSessionManager(cfg.SessionHMACKey, rand.Reader, time.Now)
	if err != nil {
		logger.Error("create worker token manager", "error", "session configuration rejected")
		os.Exit(1)
	}
	notificationID := workerID + "-notification"
	observedFlexRenderer := func(input line.FlexInput) (json.RawMessage, error) {
		result, renderErr := line.RenderFlexWithStats(input)
		if renderErr != nil {
			logger.Warn("flex render failed",
				"event", "flex_rendered", "presentationVersion", line.FlexPresentationVersion, "result", "ERROR",
				"safeErrorCode", "FLEX_RENDER_FAILED", "reportCount", len(input.Reports),
				"flexRenderTotal", 1, "flexRenderDurationMs", float64(result.Duration.Microseconds())/1000,
			)
			return nil, renderErr
		}
		logger.Info("flex render completed",
			"event", "flex_rendered", "presentationVersion", result.PresentationVersion, "result", "SUCCESS",
			"reportCount", result.ReportCount, "flexRenderTotal", 1, "flexPayloadBytes", result.PayloadBytes,
			"flexZeroReportCount", result.ZeroReportCount, "flexRenderDurationMs", float64(result.Duration.Microseconds())/1000,
			"mixedPeriods", result.MixedPeriods,
			"flexMixedPeriodTotal", 1,
		)
		return result.Message, nil
	}
	notificationWorker := notification.NewWorker(
		database.NewNotificationStore(pool), observedFlexRenderer, sessionManager, rand.Reader,
		cfg.PublicBaseURL, notificationID, time.Now,
	)
	recipientService := recipient.NewService(database.NewRecipientStore(pool), box, sessionManager, rand.Reader, cfg.PublicBaseURL.String(), time.Now)
	deliveryID := workerID + "-delivery"
	deliveryWorker := delivery.NewWorker(
		database.NewDeliveryStore(pool), recipientService,
		line.NewMessagingClient(cfg.LineMessagingAccessToken, line.DefaultPushEndpoint, 30*time.Second),
		deliveryID, time.Now,
	)
	retentionID := workerID + "-retention"
	retentionWorker := retention.NewWorker(database.NewRetentionStore(pool), retention.ProductionPolicy(), time.Now)
	quotaWorker := quota.NewWorker(
		line.NewQuotaClient(
			cfg.LineMessagingAccessToken, line.DefaultQuotaEndpoint, line.DefaultQuotaConsumptionEndpoint, 10*time.Second,
		),
		database.NewQuotaStore(pool), time.Now,
	)

	logger.Info("report worker started", "workerId", workerID, "concurrency", cfg.ReportWorkerConcurrency, "summaryQueriesEnabled", cfg.SummaryQueryEnabled)
	var recoveryLoopAt atomic.Int64
	go reportRecoveryLoop(ctx, logger, reportStore, &recoveryLoopAt)
	go heartbeatLoopDynamic(ctx, logger, pool, workerID, "REPORT", hostname, func() map[string]any {
		metadata := map[string]any{"concurrency": cfg.ReportWorkerConcurrency, "summaryQueriesEnabled": cfg.SummaryQueryEnabled}
		if unix := recoveryLoopAt.Load(); unix > 0 {
			metadata["recoveryLoopAt"] = time.Unix(unix, 0).UTC().Format(time.RFC3339)
		}
		return metadata
	})
	go heartbeatLoop(ctx, logger, pool, schedulerID, "SCHEDULER", hostname, map[string]any{"concurrency": 1})
	go heartbeatLoop(ctx, logger, pool, notificationID, "DELIVERY", hostname, map[string]any{"stage": "prepare", "presentationVersion": line.FlexPresentationVersion})
	go heartbeatLoop(ctx, logger, pool, deliveryID, "DELIVERY", hostname, map[string]any{"stage": "send", "concurrency": cfg.DeliveryWorkerConcurrency})
	go heartbeatLoop(ctx, logger, pool, retentionID, "RETENTION", hostname, map[string]any{"snapshotDays": 90, "historyDays": 365})
	go dueScheduleLoop(ctx, logger, dueWorker)
	go notificationLoop(ctx, logger, notificationWorker)
	for lane := 0; lane < cfg.DeliveryWorkerConcurrency; lane++ {
		go deliveryLoop(ctx, logger, deliveryWorker, lane)
	}
	go retentionLoop(ctx, logger, retentionWorker)
	if cfg.LineMessagingAccessToken != "" {
		quotaID := workerID + "-quota"
		go heartbeatLoop(ctx, logger, pool, quotaID, "DELIVERY", hostname, map[string]any{"stage": "quota-sync"})
		go lineQuotaLoop(ctx, logger, quotaWorker)
	}
	for lane := 0; lane < cfg.ReportWorkerConcurrency; lane++ {
		go processLoop(ctx, logger, reportWorker, lane)
	}
	<-ctx.Done()
	logger.Info("report worker stopping", "workerId", workerID)
}

func lineQuotaLoop(ctx context.Context, logger *slog.Logger, quotaWorker *quota.Worker) {
	delay := time.Duration(0)
	for ctx.Err() == nil {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		status, err := quotaWorker.Process(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Warn("LINE quota sync failed", "error", err)
			}
			delay = time.Minute
			continue
		}
		logger.Info("LINE quota synced", "state", status.State, "providerLimit", status.ProviderLimit, "providerConsumed", status.ProviderConsumed)
		delay = 5 * time.Minute
	}
}

func retentionLoop(ctx context.Context, logger *slog.Logger, retentionWorker *retention.Worker) {
	delay := time.Duration(0)
	for ctx.Err() == nil {
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		counts, err := retentionWorker.Process(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Error("retention worker error", "error", err)
			}
			delay = time.Minute
			continue
		}
		logger.Info("retention batch completed", "reportRows", counts.ReportRows, "reportRuns", counts.ReportRuns, "dashboardRefreshes", counts.DashboardRefreshes, "dashboardGenerations", counts.DashboardGenerations, "auditLogs", counts.AuditLogs, "deliveries", counts.Deliveries, "operationalIncidents", counts.OperationalIncidents, "maintenanceWindows", counts.MaintenanceWindows)
		delay = time.Hour
	}
}

func deliveryLoop(ctx context.Context, logger *slog.Logger, deliveryWorker *delivery.Worker, lane int) {
	for ctx.Err() == nil {
		err := deliveryWorker.ProcessOne(ctx)
		switch {
		case err == nil:
			continue
		case errors.Is(err, delivery.ErrNoDeliveryReady):
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case errors.Is(err, context.Canceled):
			return
		default:
			logger.Error("delivery worker error", "lane", lane, "error", err)
		}
	}
}

func notificationLoop(ctx context.Context, logger *slog.Logger, notificationWorker *notification.Worker) {
	for ctx.Err() == nil {
		err := notificationWorker.ProcessOne(ctx)
		switch {
		case err == nil:
			continue
		case errors.Is(err, notification.ErrNoExecutionReady):
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case errors.Is(err, context.Canceled):
			return
		default:
			logger.Error("notification worker error", "error", err)
		}
	}
}

func dueScheduleLoop(ctx context.Context, logger *slog.Logger, dueWorker *schedule.DueWorker) {
	for ctx.Err() == nil {
		execution, err := dueWorker.ProcessOne(ctx)
		switch {
		case err == nil:
			if execution.Status == schedule.ExecutionFailed {
				logger.Warn("due schedule paused by readiness gate", "scheduleId", execution.ScheduleID, "safeErrorCode", execution.SafeErrorCode)
			}
		case errors.Is(err, schedule.ErrNoDueSchedule):
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case errors.Is(err, context.Canceled):
			return
		default:
			logger.Error("schedule worker error", "error", err)
		}
	}
}

func processLoop(ctx context.Context, logger *slog.Logger, reportWorker *worker.ReportWorker, lane int) {
	for ctx.Err() == nil {
		err := reportWorker.ProcessOne(ctx)
		switch {
		case err == nil:
			continue
		case errors.Is(err, report.ErrNoQueuedRun):
			timer := time.NewTimer(500 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case errors.Is(err, context.Canceled):
			return
		default:
			logger.Error("report worker lane error", "lane", lane, "error", err)
		}
	}
}

func heartbeatLoop(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, workerID, workerType, hostname string, metadata map[string]any) {
	heartbeatLoopDynamic(ctx, logger, pool, workerID, workerType, hostname, func() map[string]any { return metadata })
}

func heartbeatLoopDynamic(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, workerID, workerType, hostname string, metadata func() map[string]any) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := database.RecordWorkerHeartbeat(ctx, pool, workerID, workerType, hostname, metadata(), time.Now().UTC()); err != nil && ctx.Err() == nil {
			logger.Error("worker heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func reportRecoveryLoop(ctx context.Context, logger *slog.Logger, store *database.ReportStore, lastSuccess *atomic.Int64) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		recovered, err := store.RecoverExpiredLeases(ctx, now)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("report lease recovery failed", "safeErrorCode", "REPORT_LEASE_RECOVERY_FAILED")
			}
		} else {
			lastSuccess.Store(now.Unix())
			if recovered.RequeuedClaimed > 0 || recovered.FailedRunning > 0 {
				logger.Warn("report leases recovered",
					"requeuedClaimed", recovered.RequeuedClaimed,
					"failedRunning", recovered.FailedRunning,
					"cancelledSiblings", recovered.CancelledSiblings,
				)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
