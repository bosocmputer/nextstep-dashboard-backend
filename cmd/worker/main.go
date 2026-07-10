package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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
	policy := sml.EndpointPolicy{AllowedPrefixes: cfg.SMLAllowedPrefixes, AllowedHosts: cfg.SMLAllowedHosts}
	connections := sml.NewConnectionService(database.NewSMLConnectionStore(pool), box, policy, nil, time.Now)
	reportClient := sml.NewClient(policy, 120*time.Second, 32*1024*1024, 200_000)
	hostname, _ := os.Hostname()
	workerID := fmt.Sprintf("%s-%d", hostname, os.Getpid())
	reportWorker := worker.NewReportWorker(database.NewReportStore(pool), connections, reportClient, workerID, time.Now)
	schedulerID := workerID + "-scheduler"
	dueWorker := schedule.NewDueWorker(database.NewScheduleStore(pool), schedulerID, time.Now)
	sessionManager, err := auth.NewSessionManager(cfg.SessionHMACKey, rand.Reader, time.Now)
	if err != nil {
		logger.Error("create worker token manager", "error", "session configuration rejected")
		os.Exit(1)
	}
	notificationID := workerID + "-notification"
	notificationWorker := notification.NewWorker(
		database.NewNotificationStore(pool), line.RenderFlex, sessionManager, rand.Reader,
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

	logger.Info("report worker started", "workerId", workerID, "concurrency", cfg.ReportWorkerConcurrency)
	go heartbeatLoop(ctx, logger, pool, workerID, "REPORT", hostname, map[string]any{"concurrency": cfg.ReportWorkerConcurrency})
	go heartbeatLoop(ctx, logger, pool, schedulerID, "SCHEDULER", hostname, map[string]any{"concurrency": 1})
	go heartbeatLoop(ctx, logger, pool, notificationID, "DELIVERY", hostname, map[string]any{"stage": "prepare"})
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
		logger.Info("retention batch completed", "reportRows", counts.ReportRows, "reportRuns", counts.ReportRuns, "dashboardRefreshes", counts.DashboardRefreshes, "auditLogs", counts.AuditLogs, "deliveries", counts.Deliveries)
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
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := database.RecordWorkerHeartbeat(ctx, pool, workerID, workerType, hostname, metadata, time.Now().UTC()); err != nil && ctx.Err() == nil {
			logger.Error("worker heartbeat failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
