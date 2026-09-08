package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMockStore(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, mock.ExpectationsWereMet(), "not all expected statements were issued")
		mock.Close()
	})
	return mock
}

// InTx is the transaction boundary every repository in the application is built
// on. These tests cover it directly rather than only through its callers.

func TestInTxCommitsWhenTheFunctionSucceeds(t *testing.T) {
	t.Parallel()

	mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO t`).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()

	err := InTx(context.Background(), mock, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "INSERT INTO t VALUES (1)")
		return err
	})

	assert.NoError(t, err)
}

func TestInTxRollsBackAndReturnsTheFunctionsError(t *testing.T) {
	t.Parallel()

	mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	sentinel := errors.New("business rule violated")
	err := InTx(context.Background(), mock, func(pgx.Tx) error { return sentinel })

	// The caller's error must survive untouched: InTx must not reclassify a
	// domain error as an internal one on its way out.
	assert.ErrorIs(t, err, sentinel)
}

// TestInTxRollsBackAndRepanics is the important one. A panic inside a
// transaction must not leave it open, and it must not be swallowed either —
// converting a panic into a nil error would hide a bug behind a successful
// response.
func TestInTxRollsBackAndRepanics(t *testing.T) {
	t.Parallel()

	mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	assert.PanicsWithValue(t, "boom", func() {
		_ = InTx(context.Background(), mock, func(pgx.Tx) error { panic("boom") })
	})
}

func TestInTxSurfacesABeginFailureAsInternal(t *testing.T) {
	t.Parallel()

	mock := newMockStore(t)
	mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))

	called := false
	err := InTx(context.Background(), mock, func(pgx.Tx) error { called = true; return nil })

	require.Error(t, err)
	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
	assert.False(t, called, "fn must not run when the transaction never opened")
	assert.NotContains(t, apierr.From(err).Message, "pool exhausted",
		"the driver's text must not reach the client")
}

func TestInTxSurfacesACommitFailureAsInternal(t *testing.T) {
	t.Parallel()

	mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectCommit().WillReturnError(errors.New("connection reset"))
	// A failed commit still triggers the deferred rollback, because err is set.
	mock.ExpectRollback()

	err := InTx(context.Background(), mock, func(pgx.Tx) error { return nil })

	require.Error(t, err)
	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

// TestInTxRollsBackEvenWhenTheCallersContextIsCancelled covers the reason the
// rollback uses a fresh background context. A cancelled request context is
// exactly when a rollback matters most, and issuing it on that context would
// fail and leak the transaction until the pool recycled the connection.
func TestInTxRollsBackEvenWhenTheCallersContextIsCancelled(t *testing.T) {
	t.Parallel()

	mock := newMockStore(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	ctx, cancel := context.WithCancel(context.Background())

	err := InTx(ctx, mock, func(pgx.Tx) error {
		cancel() // the client hung up mid-transaction
		return context.Canceled
	})

	assert.ErrorIs(t, err, context.Canceled)
	// ExpectationsWereMet in the cleanup is the assertion that matters here:
	// the rollback was issued despite the caller's context being dead.
}

func TestConnectRejectsAnUnparseableURL(t *testing.T) {
	t.Parallel()

	_, err := Connect(context.Background(), config.DBConfig{URL: "://not-a-url"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse DATABASE_URL")
}

// TestConnectFailsWhenTheServerIsUnreachable proves the explicit Ping earns its
// place: pgxpool.New is lazy and would hand back a usable-looking pool, turning
// a bad DATABASE_URL into a 500 on the first request instead of a start-up
// failure. Port 1 is used because nothing serves there.
func TestConnectFailsWhenTheServerIsUnreachable(t *testing.T) {
	t.Parallel()

	_, err := Connect(context.Background(), config.DBConfig{
		URL:            "postgres://blog:blog@127.0.0.1:1/blog?sslmode=disable",
		MaxConns:       2,
		MinConns:       0,
		ConnectTimeout: 2 * time.Second,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping database")
}
