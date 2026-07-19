package main

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/auth"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/config"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/database"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/httpapi"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/line"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/operations"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/recipient"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/schedule"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sentinel"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tablequery"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/tenant"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/viewer"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		logger.Error("invalid application configuration", "error", err)
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
	sessionManager, err := auth.NewSessionManager(cfg.SessionHMACKey, rand.Reader, time.Now)
	if err != nil {
		logger.Error("create session manager", "error", "session configuration rejected")
		os.Exit(1)
	}
	adminService := auth.NewAdminService(database.NewAdminStore(pool), sessionManager, cfg.AdminPasswordHash, rand.Reader, time.Now)
	tenantService := tenant.NewService(database.NewTenantStore(pool), time.Now).ConfigurePublicBaseURL(cfg.PublicBaseURL.String())
	refreshPolicyService := report.NewRefreshPolicyService(database.NewRefreshPolicyStore(pool), time.Now).
		ConfigureRollout(cfg.SnapshotFirstEnabled, cfg.SnapshotFirstTenantIDs, cfg.StaleRevalidationEnabled)
	secretBox, err := secret.NewBox(cfg.EncryptionMasterKey, cfg.EncryptionKeyID, rand.Reader)
	if err != nil {
		logger.Error("create secret box", "error", "encryption configuration rejected")
		os.Exit(1)
	}
	smlPolicy := sml.EndpointPolicy{AllowedPrefixes: cfg.SMLAllowedPrefixes, AllowedHosts: cfg.SMLAllowedHosts, AllowPublicEndpoints: cfg.SMLAllowPublicEndpoints, AllowedPorts: cfg.SMLAllowedPorts}
	smlClient := sml.NewClient(smlPolicy, 30*time.Second, 32*1024*1024, 200_000)
	smlService := sml.NewConnectionService(database.NewSMLConnectionStore(pool), secretBox, smlPolicy, smlClient, time.Now).
		ConfigureTestCoordinator(database.NewSMLTestCoordinator(pool))
	recipientService := recipient.NewService(database.NewRecipientStore(pool), secretBox, sessionManager, rand.Reader, cfg.PublicBaseURL.String(), time.Now)
	lineVerifier := line.NewIDTokenVerifier(cfg.LineLoginChannelID, line.DefaultIDTokenVerifyEndpoint, 10*time.Second, time.Now)
	viewerService := viewer.NewService(lineVerifier, recipientService, database.NewViewerStore(pool), sessionManager, time.Now)
	viewerReportService := viewer.NewReportService(viewerService, database.NewReportStore(pool).ConfigureGenerationCache(cfg.GenerationCacheEnabled), time.Now).
		ConfigureSnapshotFirst(cfg.SnapshotFirstEnabled, cfg.SnapshotFirstTenantIDs).
		ConfigureStaleRevalidation(cfg.StaleRevalidationEnabled)
	periodObserver := func(preset report.Preset, mode report.ParameterKind, result string) {
		logger.Info("schedule period resolved", "event", "schedule_period_resolution", "preset", preset, "mode", mode, "result", result, "schedulePeriodResolutionTotal", 1)
	}
	scheduleService := schedule.NewService(database.NewScheduleStore(pool).ConfigureSmartPeriods(cfg.SmartSchedulePeriodsEnabled, cfg.SmartSchedulePeriodTenantIDs, periodObserver), cfg.LineMessagingAccessToken != "", time.Now)
	scheduleTestService := schedule.NewTestSendService(database.NewScheduleStore(pool).ConfigureSmartPeriods(cfg.SmartSchedulePeriodsEnabled, cfg.SmartSchedulePeriodTenantIDs, periodObserver), cfg.LineMessagingAccessToken != "", time.Now)
	flexPreviewService := line.NewFlexPreviewService(tenantService, cfg.PublicBaseURL, time.Now).
		ConfigureSmartPeriods(cfg.SmartSchedulePeriodsEnabled, cfg.SmartSchedulePeriodTenantIDs, periodObserver)

	sentinelStore := database.NewSentinelStore(pool)
	var watchdog httpapi.WatchdogAPI
	if cfg.WatchdogEnabled {
		backupPolicy, policyErr := sentinel.ParseBackupPolicy(cfg.BackupPolicy)
		if policyErr != nil {
			logger.Error("invalid backup policy", "safeErrorCode", "BACKUP_POLICY_INVALID")
			os.Exit(1)
		}
		watchdog = sentinel.NewWatchdog(cfg.SentinelRuntimeDirectory, time.Now).ConfigureBackupPolicy(backupPolicy)
	}
	server := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewHandler(httpapi.Dependencies{
			Logger:          logger,
			Readiness:       pool,
			AdminAuth:       adminService,
			Tenants:         tenantService,
			SMLConnections:  smlService,
			Recipients:      recipientService,
			ViewerAuth:      viewerService,
			ViewerReports:   viewerReportService,
			RefreshPolicies: refreshPolicyService,
			Schedules:       scheduleService,
			FlexPreviews:    flexPreviewService,
			ScheduleTests:   scheduleTestService,
			Operations:      operations.NewService(database.NewOperationsStore(pool), recipientService),
			TableQueries:    tablequery.NewService(database.NewTableQueryStore(pool), recipientService, cfg.LineMessagingAccessToken != "", time.Now),
			Incidents:       sentinel.NewAdminService(sentinelStore, time.Now),
			Watchdog:        watchdog,
			SecureCookies:   cfg.Environment == "production",
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("API listening", "address", cfg.HTTPAddr, "environment", cfg.Environment)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("API shutdown failed", "error", err)
			os.Exit(1)
		}
	case err := <-serverErrors:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("API stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	}
}
