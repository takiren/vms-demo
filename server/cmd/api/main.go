// Command api is the vms-demo API server.
//
// Usage:
//
//	api                            run the server
//	api create-admin <username>    create an admin (password from stdin)
//	api reset-password <username>  set a user's password (from stdin) and log them out everywhere
//
// Both subcommands read the configuration from the same environment as the
// server and never take the password as an argument (it would show up in
// the process list and shell history).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v5"

	"github.com/takiren/vms-demo/server/internal/api"
	"github.com/takiren/vms-demo/server/internal/auth"
	"github.com/takiren/vms-demo/server/internal/camera"
	"github.com/takiren/vms-demo/server/internal/config"
	"github.com/takiren/vms-demo/server/internal/db"
	"github.com/takiren/vms-demo/server/internal/mediamtx"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// The logger may not exist yet, so write directly to stderr.
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

const usage = `usage:
  api                            run the server
  api create-admin <username>    create an admin user (password from stdin)
  api reset-password <username>  set a user's password (from stdin)`

func run(args []string) error {
	if len(args) == 0 {
		return serve()
	}
	switch args[0] {
	case "create-admin", "reset-password":
		if len(args) != 2 {
			return errors.New(usage)
		}
		return userCommand(args[0], args[1], os.Stdin, os.Stderr)
	case "-h", "--help", "help":
		fmt.Println(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

// setup loads the configuration, connects to the database, applies the
// migrations and builds the auth service. The caller closes the pool.
func setup(ctx context.Context, logger *slog.Logger, cfg config.Config) (*auth.Service, *pgxpool.Pool, error) {
	pool, err := db.Open(ctx, cfg.DatabaseURL.Value())
	if err != nil {
		return nil, nil, err
	}
	if err := db.Migrate(ctx, pool, logger); err != nil {
		pool.Close()
		return nil, nil, err
	}
	svc, err := auth.NewService(ctx, pool, logger, auth.Config{
		IdleTimeout: cfg.SessionIdleTimeout,
		MaxLifetime: cfg.SessionMaxLifetime,
	})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return svc, pool, nil
}

// startCameras builds the camera service and starts syncing the cameras to
// MediaMTX in the background. Startup does not wait for MediaMTX: if it is
// down, the reconciler keeps retrying.
func startCameras(ctx context.Context, logger *slog.Logger, cfg config.Config, pool *pgxpool.Pool) (*camera.Service, error) {
	cams, err := camera.NewService(pool, cfg.SecretKey.Bytes(), logger)
	if err != nil {
		return nil, err
	}
	client := mediamtx.NewClient(cfg.MediaMTXAPIURL, cfg.MediaMTXAPIUser, cfg.MediaMTXAPIPassword.Value())
	rec := mediamtx.NewReconciler(client, cams, logger, cfg.MediaMTXSyncInterval)
	cams.SetOnChange(rec.Trigger)
	go rec.Run(ctx)
	return cams, nil
}

func serve() error {
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
	svc, pool, err := setup(ctx, logger, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	bootstrapAdmin(ctx, logger, svc, cfg)
	go svc.RunCleanup(ctx)

	cams, err := startCameras(ctx, logger, cfg, pool)
	if err != nil {
		return err
	}

	store, err := newStore(cfg)
	if err != nil {
		return err
	}
	ingestSvc, err := newIngest(logger, cfg, pool, store)
	if err != nil {
		return err
	}
	var ingestAPI api.IngestService
	if ingestSvc != nil {
		ingestAPI = ingestSvc
		done := make(chan struct{})
		go func() { defer close(done); ingestSvc.Run(ctx) }()
		// Let the workers release their leases before the pool closes.
		defer func() { <-done }()
	} else {
		logger.Warn("recording ingest is disabled (" + config.EnvRecordingsDir + " is not set)")
	}

	retentionJob, err := newRetention(logger, cfg, pool, store)
	if err != nil {
		return err
	}
	if retentionJob != nil {
		done := make(chan struct{})
		go func() { defer close(done); retentionJob.Run(ctx) }()
		defer func() { <-done }()
	} else {
		logger.Warn("the retention job is disabled (object storage is not configured)")
	}

	pb, err := newPlayback(logger, cfg, pool, store)
	if err != nil {
		return err
	}
	var playbackAPI api.PlaybackService
	if pb != nil {
		playbackAPI = pb
	} else {
		logger.Warn("recording playback is disabled (" + config.EnvS3Endpoint + " is not set)")
	}
	clipSvc, err := newClip(logger, cfg, pool, store, pb)
	if err != nil {
		return err
	}
	var clipAPI api.ClipService
	if clipSvc != nil {
		clipAPI = clipSvc
		go clipSvc.Run(ctx)
	}

	e, err := api.New(logger, api.Options{
		Auth:           svc,
		PublicOrigin:   cfg.PublicOrigin,
		TrustedProxies: cfg.TrustedProxies,
		Cameras:        cams,
		HookToken:      cfg.Ingest.HookToken.Value(),
		Ingest:         ingestAPI,
		Playback:       playbackAPI,
		Clip:           clipAPI,
	})
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

// bootstrapAdmin creates the initial admin from VMS_ADMIN_USERNAME and
// VMS_ADMIN_PASSWORD if there is no admin. It never changes existing users
// and never logs the password. Failures are logged, not fatal: the server
// is still useful, and the CLI can create an admin.
func bootstrapAdmin(ctx context.Context, logger *slog.Logger, svc *auth.Service, cfg config.Config) {
	if cfg.AdminUsername == "" {
		return
	}
	created, err := svc.BootstrapAdmin(ctx, cfg.AdminUsername, cfg.AdminPassword.Value())
	switch {
	case errors.Is(err, auth.ErrUsernameTaken):
		logger.Warn("no admin exists, but the initial admin username is taken by another user; "+
			"not changing it. Create an admin with: api create-admin <username>",
			slog.String("username", cfg.AdminUsername))
	case err != nil:
		logger.Error("failed to create the initial admin", slog.String("error", err.Error()))
	case created:
		logger.Info("created the initial admin", slog.String("username", cfg.AdminUsername))
	}
}

// userCommand runs create-admin or reset-password. Logs go to stderr at
// warn level so the output stays readable.
func userCommand(cmd, username string, stdin *os.File, stderr io.Writer) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := auth.ValidateUsername(username); err != nil {
		return err
	}
	password, err := readPassword(stdin, stderr)
	if err != nil {
		return err
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc, pool, err := setup(ctx, logger, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	switch cmd {
	case "create-admin":
		u, err := svc.CreateUser(ctx, username, password, auth.RoleAdmin)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "created admin %s\n", u.Username)
	case "reset-password":
		if err := svc.ResetPassword(ctx, username, password); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "password changed for %s; all of their sessions were ended\n", username)
	}
	return nil
}
