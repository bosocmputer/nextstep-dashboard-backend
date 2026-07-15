package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/config"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/database"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/report"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/secret"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/sml"
	"github.com/google/uuid"
	"os/signal"
)

const smokeQueryTimeout = 60 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("SML smoke check failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return errors.New("invalid smoke-check configuration")
	}
	tenantID, err := uuid.Parse(strings.TrimSpace(os.Getenv("SML_SMOKE_TENANT_ID")))
	if err != nil {
		return errors.New("SML_SMOKE_TENANT_ID must be a UUID")
	}
	period, err := smokePeriod(os.Getenv("SML_SMOKE_DATE_FROM"), os.Getenv("SML_SMOKE_DATE_TO"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := database.OpenPool(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConnections, cfg.DatabaseMinConnections)
	if err != nil {
		return errors.New("database is unavailable")
	}
	defer pool.Close()
	box, err := secret.NewBox(cfg.EncryptionMasterKey, cfg.EncryptionKeyID, rand.Reader)
	if err != nil {
		return errors.New("encryption configuration is invalid")
	}
	policy := sml.EndpointPolicy{AllowedPrefixes: cfg.SMLAllowedPrefixes, AllowedHosts: cfg.SMLAllowedHosts, AllowPublicEndpoints: cfg.SMLAllowPublicEndpoints, AllowedPorts: cfg.SMLAllowedPorts}
	connectionService := sml.NewConnectionService(database.NewSMLConnectionStore(pool), box, policy, nil, time.Now)
	connection, err := connectionService.Open(ctx, tenantID)
	if err != nil {
		return errors.New("tenant SML connection could not be opened")
	}
	client := sml.NewClient(policy, smokeQueryTimeout, 4*1024*1024, 2)

	failures := 0
	for _, key := range report.Keys() {
		plan, planErr := report.BuildQueryPlan(key, period)
		if planErr != nil {
			return errors.New("approved report plan could not be built")
		}
		for _, step := range plan.Steps {
			rendered, renderErr := report.RenderSQL(step.Query)
			if renderErr != nil {
				return errors.New("approved report SQL could not be rendered")
			}
			startedAt := time.Now()
			queryCtx, cancel := context.WithTimeout(ctx, smokeQueryTimeout)
			rows, queryErr := client.Query(queryCtx, connection, smokeSQL(rendered))
			cancel()
			if queryErr != nil {
				failures++
				logger.Error("SML smoke step failed", "reportKey", key, "step", step.Name, "safeError", queryErr, "elapsedMs", time.Since(startedAt).Milliseconds())
				continue
			}
			logger.Info("SML smoke step passed", "reportKey", key, "step", step.Name, "sampleRows", len(rows), "elapsedMs", time.Since(startedAt).Milliseconds())
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d approved report query steps failed", failures)
	}
	logger.Info("all approved SML report queries passed", "reportCount", len(report.Keys()), "dateFrom", period.DateFrom, "dateTo", period.DateTo)
	return nil
}

func smokePeriod(rawFrom, rawTo string) (report.Period, error) {
	from, err := time.Parse(time.DateOnly, strings.TrimSpace(rawFrom))
	if err != nil {
		return report.Period{}, errors.New("SML_SMOKE_DATE_FROM must use YYYY-MM-DD")
	}
	to, err := time.Parse(time.DateOnly, strings.TrimSpace(rawTo))
	if err != nil {
		return report.Period{}, errors.New("SML_SMOKE_DATE_TO must use YYYY-MM-DD")
	}
	if to.Before(from) || to.Sub(from) > 31*24*time.Hour {
		return report.Period{}, errors.New("SML smoke date range must be ordered and at most 32 days")
	}
	return report.Period{Preset: report.Custom, DateFrom: from.Format(time.DateOnly), DateTo: to.Format(time.DateOnly)}, nil
}

func smokeSQL(sql string) string {
	return "select * from (\n" + strings.TrimSpace(sql) + "\n) as nextstep_smoke limit 1"
}
