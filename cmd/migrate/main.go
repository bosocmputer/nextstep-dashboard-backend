package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/config"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/database"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		logger.Error("invalid migration configuration", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := database.OpenPool(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConnections, cfg.DatabaseMinConnections)
	if err != nil {
		logger.Error("create database pool", "error", "database configuration rejected")
		os.Exit(1)
	}
	defer pool.Close()
	if len(os.Args) == 2 && os.Args[1] == "--pending" {
		pending, pendingErr := database.PendingMigrationCount(ctx, pool)
		if pendingErr != nil {
			logger.Error("inspect pending migrations", "error", "migration status unavailable")
			os.Exit(1)
		}
		fmt.Println(pending)
		return
	}

	if err := database.Migrate(ctx, pool); err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}
	created, err := database.BootstrapAdmin(ctx, pool, cfg.AdminUsername, cfg.AdminPasswordHash)
	if err != nil {
		logger.Error("admin bootstrap failed", "error", "bootstrap transaction failed")
		os.Exit(1)
	}
	if created {
		logger.Info("bootstrap admin created", "username", cfg.AdminUsername)
	}
	logger.Info("database migrations complete")
}
