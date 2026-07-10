package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/bosocmputer/nextstep-dashboard-backend/internal/config"
	"github.com/bosocmputer/nextstep-dashboard-backend/internal/database"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		fail()
	}
	pool, err := database.OpenPool(ctx, cfg.DatabaseURL, 2, 0)
	if err != nil {
		fail()
	}
	defer pool.Close()
	hostname, err := os.Hostname()
	if err != nil {
		fail()
	}
	healthy, err := database.WorkerNodeHealthy(ctx, pool, hostname, time.Now().UTC())
	if err != nil || !healthy {
		fail()
	}
}

func fail() {
	_, _ = fmt.Fprintln(os.Stderr, "worker not healthy")
	os.Exit(1)
}
