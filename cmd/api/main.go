// Command api runs the blog HTTP service.
//
// main does four things and delegates everything else: load configuration,
// build a logger, open the database, and hand control to internal/server. Every
// exit path goes through run() returning an error, so there is exactly one place
// that calls os.Exit and no deferred cleanup is skipped by an early exit
// buried in the middle of start-up.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
	"github.com/bpsiregar/majoo-assessment/internal/platform/postgres"
	"github.com/bpsiregar/majoo-assessment/internal/server"
)

// version is stamped at build time with:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
//
// The default makes it obvious when a binary was built without that flag rather
// than silently reporting a plausible-looking version.
var version = "dev"

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet if configuration failed, so this last
		// resort writes plainly to stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	logger := logging.New(os.Stdout, cfg.Log.Level, cfg.Log.Format).With(
		slog.String("service", "majoo-blog-api"),
		slog.String("version", version),
		slog.String("env", cfg.AppEnv),
	)
	// Anything that logs before it receives a logger — a library, a panic
	// handler — lands in the same stream and format.
	slog.SetDefault(logger)

	// signal.NotifyContext cancels ctx on SIGINT or SIGTERM. SIGTERM is what
	// Kubernetes sends before the kill grace period, and honouring it is the
	// whole basis of a zero-downtime rolling deploy.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	// Closed after Run returns, because draining background work may still need
	// the pool.
	defer pool.Close()

	logger.Info("database pool ready",
		slog.Int("max_conns", int(cfg.DB.MaxConns)),
		slog.Int("min_conns", int(cfg.DB.MinConns)))

	srv := server.New(cfg, logger, pool, version)
	return srv.Run(ctx)
}
