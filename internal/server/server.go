package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/auth"
	"github.com/bpsiregar/majoo-assessment/internal/comment"
	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/health"
	"github.com/bpsiregar/majoo-assessment/internal/middleware"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/bpsiregar/majoo-assessment/internal/platform/validation"
	"github.com/bpsiregar/majoo-assessment/internal/post"
	"github.com/bpsiregar/majoo-assessment/internal/user"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server owns the HTTP listener and everything with a lifecycle attached to it.
type Server struct {
	http    *http.Server
	logger  *slog.Logger
	cfg     config.Config
	bus     *events.Bus
	limiter *middleware.RateLimiter
	authLim *middleware.RateLimiter
}

// New wires the application and returns a Server ready to Run.
//
// The whole dependency graph is constructed here, in dependency order:
// repositories over the pool, services over repositories, handlers over
// services, router over handlers. Nothing is resolved lazily and nothing is
// looked up from a registry, so reading this function tells you exactly what
// the process is made of.
func New(cfg config.Config, logger *slog.Logger, pool *pgxpool.Pool, version string) *Server {
	validator := validation.New()
	hasher := auth.NewHasher(cfg.Auth.BcryptCost)
	tokens := auth.NewTokenService(
		cfg.Auth.JWTSecret, cfg.Auth.Issuer, cfg.Auth.Audience,
		cfg.Auth.AccessTTL, cfg.Auth.RefreshTTL,
	)

	bus := events.NewBus(cfg.Events.Workers, cfg.Events.Buffer, logger)
	bus.Subscribe(auditLogHandler(logger))
	bus.Start()

	userRepo := user.NewRepository(pool)
	sessionRepo := user.NewSessionRepository(pool)
	postRepo := post.NewRepository(pool)
	commentRepo := comment.NewRepository(pool)

	userSvc := user.NewService(userRepo, sessionRepo, hasher, tokens, bus)
	postSvc := post.NewService(postRepo, bus)
	commentSvc := comment.NewService(commentRepo, postSvc, bus)

	var globalLimiter, authLimiter *middleware.RateLimiter
	if cfg.Rate.Enabled {
		globalLimiter = middleware.NewRateLimiter(cfg.Rate.RPS, cfg.Rate.Burst)
		authLimiter = middleware.NewRateLimiter(cfg.Rate.AuthRPS, cfg.Rate.AuthBurst)
	}

	handler := newRouter(routerDeps{
		cfg:           cfg,
		logger:        logger,
		auth:          middleware.NewAuthenticator(tokens),
		globalLimiter: globalLimiter,
		authLimiter:   authLimiter,
		users:         user.NewHandler(userSvc, validator),
		posts:         post.NewHandler(postSvc, validator),
		comments:      comment.NewHandler(commentSvc, validator),
		health:        health.NewHandler(pool, version),
		version:       version,
	})

	return &Server{
		http: &http.Server{
			Addr:    cfg.HTTP.Addr,
			Handler: handler,
			// Every timeout is set. ReadHeaderTimeout in particular is the defence
			// against Slowloris: without it a client can hold a connection open
			// indefinitely by dribbling out headers.
			ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
			ReadTimeout:       cfg.HTTP.ReadTimeout,
			WriteTimeout:      cfg.HTTP.WriteTimeout,
			IdleTimeout:       cfg.HTTP.IdleTimeout,
			// Route net/http's own errors into the structured logger instead of
			// the default, unstructured stderr writer.
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		},
		logger:  logger,
		cfg:     cfg,
		bus:     bus,
		limiter: globalLimiter,
		authLim: authLimiter,
	}
}

// Run serves until ctx is cancelled, then shuts down gracefully.
//
// The sequence on shutdown matters:
//
//  1. Stop accepting connections and let in-flight requests finish, bounded by
//     SHUTDOWN_TIMEOUT. A load balancer will have stopped routing new work here
//     as soon as /readyz started failing, so this window only has to cover
//     requests already being served.
//  2. Drain the event bus, so audit work queued by those requests still runs.
//  3. Stop the rate limiters' eviction goroutines.
//
// The database pool is closed by the caller, after this returns, because
// draining events may still need it.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.logger.Info("http server listening", slog.String("addr", s.cfg.HTTP.Addr))
		// ErrServerClosed is the expected result of a graceful Shutdown and is
		// not an error worth propagating.
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		// The listener failed before any shutdown signal — most often a port
		// already in use. Still tidy up the background goroutines.
		s.stopBackground(s.cfg.HTTP.ShutdownTimeout)
		return err

	case <-ctx.Done():
		s.logger.Info("shutdown signal received; draining",
			slog.Duration("grace_period", s.cfg.HTTP.ShutdownTimeout))

		// A fresh context: the one that was cancelled to trigger shutdown cannot
		// also bound it.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.HTTP.ShutdownTimeout)
		defer cancel()

		var shutdownErr error
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			shutdownErr = fmt.Errorf("graceful shutdown: %w", err)
			s.logger.Warn("graceful shutdown did not complete in time; forcing close",
				slog.Any("error", err))
			_ = s.http.Close()
		}

		// Whatever budget the HTTP drain left over is what the event bus gets.
		remaining := time.Until(deadlineOr(shutdownCtx, time.Now()))
		s.stopBackground(remaining)

		if err := <-errCh; err != nil {
			return errors.Join(shutdownErr, err)
		}
		s.logger.Info("shutdown complete")
		return shutdownErr
	}
}

// stopBackground drains the event bus and stops the limiters' janitors.
func (s *Server) stopBackground(grace time.Duration) {
	if grace < time.Second {
		// Always allow a moment to drain, even if the HTTP phase used the budget.
		grace = time.Second
	}

	busCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if drained := s.bus.Shutdown(busCtx); !drained {
		published, dropped, queued := s.bus.Stats()
		s.logger.Warn("event bus did not fully drain",
			slog.Int64("published", published),
			slog.Int64("dropped", dropped),
			slog.Int("still_queued", queued))
	}

	if s.limiter != nil {
		s.limiter.Close()
	}
	if s.authLim != nil {
		s.authLim.Close()
	}
}

func deadlineOr(ctx context.Context, fallback time.Time) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return fallback
}

// auditLogHandler writes every domain event to the structured log.
//
// This is the honest minimum: it gives an auditable trail with no extra
// infrastructure. A real audit requirement would write to an append-only store
// through the transactional outbox described in docs/architecture-decisions.md
// (ADR-007) — the handler signature is already the right shape for that.
func auditLogHandler(logger *slog.Logger) events.Handler {
	audit := logger.With(slog.String("log_type", "audit"))
	return func(ctx context.Context, e events.Event) {
		attrs := []any{
			slog.String("event", e.Name),
			slog.String("actor_id", e.ActorID),
			slog.String("subject_id", e.SubjectID),
			slog.String("request_id", e.RequestID),
			slog.Time("occurred_at", e.OccurredAt),
		}
		for k, v := range e.Attributes {
			attrs = append(attrs, slog.Any(k, v))
		}
		audit.LogAttrs(ctx, slog.LevelInfo, "domain event", slog.Group("audit", attrs...))
	}
}
