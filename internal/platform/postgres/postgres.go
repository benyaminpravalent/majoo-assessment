// Package postgres owns the PostgreSQL connection pool, the small interface the
// repositories are written against, and the translation of driver errors into
// application errors.
//
// There is deliberately no generic "repository" abstraction here. Repositories
// write their own SQL and scan their own rows; this package only supplies the
// plumbing that every one of them needs.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL SQLSTATE codes we react to by name rather than by number at the
// call site. See https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
	sqlStateCheckViolation      = "23514"
	sqlStateSerializationFail   = "40001"
	sqlStateDeadlockDetected    = "40P01"
)

// Executor is the read/write surface shared by *pgxpool.Pool and pgx.Tx.
//
// Repository methods accept an Executor so the same query code runs both inside
// and outside a transaction, and so unit tests can substitute pgxmock without
// any production-side indirection.
type Executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is an Executor that can also open a transaction. Repositories that need
// atomicity across two statements hold a Store; the rest hold an Executor.
type Store interface {
	Executor
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Compile-time proof that the real pool satisfies the interfaces the
// repositories are written against.
var _ Store = (*pgxpool.Pool)(nil)

// Connect opens the pool and verifies it can actually reach the database.
//
// pgxpool.New is lazy — it returns a usable pool without contacting the server —
// so the explicit Ping is what turns a bad DATABASE_URL into a start-up failure
// instead of a 500 on the first request.
func Connect(ctx context.Context, cfg config.DBConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.ConnConfig.ConnectTimeout = cfg.ConnectTimeout

	// Stagger recycling so a pool of N connections does not expire all at once
	// and stampede the database with reconnects.
	poolCfg.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 10

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// InTx runs fn inside a transaction, committing on success and rolling back on
// any error or panic.
//
// The rollback is deferred with a background context on purpose: if the request
// context has already been cancelled — which is exactly when a rollback matters
// most — a rollback issued on that context would itself fail and leak the
// transaction until the connection is recycled.
func InTx(ctx context.Context, store Store, fn func(pgx.Tx) error) (err error) {
	tx, err := store.Begin(ctx)
	if err != nil {
		return apierr.Internal(fmt.Errorf("begin transaction: %w", err))
	}

	defer func() {
		if p := recover(); p != nil {
			rollback(tx)
			panic(p)
		}
		if err != nil {
			rollback(tx)
		}
	}()

	if err = fn(tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return apierr.Internal(fmt.Errorf("commit transaction: %w", err))
	}
	return nil
}

func rollback(tx pgx.Tx) {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// pgx returns ErrTxClosed when the transaction already ended; anything else
	// is swallowed because the caller's original error is the one worth
	// reporting, and the connection is discarded by the pool either way.
	_ = tx.Rollback(rollbackCtx)
}

// pgErr extracts the driver's structured error, if this is one.
func pgErr(err error) *pgconn.PgError {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// IsUniqueViolation reports whether err is a unique-constraint violation. When
// constraint is non-empty it must also match, so a handler can distinguish
// "duplicate email" from "duplicate username" without parsing messages.
func IsUniqueViolation(err error, constraint string) bool {
	e := pgErr(err)
	return e != nil && e.Code == sqlStateUniqueViolation &&
		(constraint == "" || e.ConstraintName == constraint)
}

// IsForeignKeyViolation reports whether err is a foreign-key violation.
func IsForeignKeyViolation(err error, constraint string) bool {
	e := pgErr(err)
	return e != nil && e.Code == sqlStateForeignKeyViolation &&
		(constraint == "" || e.ConstraintName == constraint)
}

// IsCheckViolation reports whether err is a CHECK-constraint violation.
func IsCheckViolation(err error, constraint string) bool {
	e := pgErr(err)
	return e != nil && e.Code == sqlStateCheckViolation &&
		(constraint == "" || e.ConstraintName == constraint)
}

// IsRetryable reports whether a failed transaction could succeed if replayed.
// Serialization failures and deadlocks are the two cases where the correct
// response is to retry rather than to surface an error.
func IsRetryable(err error) bool {
	e := pgErr(err)
	return e != nil && (e.Code == sqlStateSerializationFail || e.Code == sqlStateDeadlockDetected)
}

// IsNoRows reports whether a query returned no rows.
func IsNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// NotFoundOr converts pgx.ErrNoRows into a 404 for the named resource and
// anything else into a 500. It exists so that every repository read ends with
// the same two-line error branch instead of an ad-hoc one.
func NotFoundOr(err error, resource string) error {
	if IsNoRows(err) {
		return apierr.NotFound(resource)
	}
	return apierr.Internal(err)
}
