// Command api is the vms-demo API server.
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

	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/config"
	"github.com/takiren/vms-demo/server/internal/db"
)

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet, so write directly to stderr.
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	logger.Info("starting api", slog.Any("config", cfg))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Fail fast: without the database (and an up-to-date schema) nothing works.
	pool, err := db.Open(ctx, cfg.DatabaseURL.Value())
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool, logger); err != nil {
		return err
	}

	e, err := api.New(logger, &api.Handlers{})
	if err != nil {
		return err
	}

	sc := echo.StartConfig{
		Address:         cfg.ListenAddr,
		HideBanner:      true,
		GracefulTimeout: cfg.ShutdownTimeout,
		OnShutdownError: func(err error) {
			logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
		},
		BeforeServeFunc: func(s *http.Server) error {
			s.ReadHeaderTimeout = 10 * time.Second
			s.IdleTimeout = 120 * time.Second
			return nil
		},
	}
	if err := sc.Start(ctx, e); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	logger.Info("api stopped")
	return nil
}
