package user

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests use pgxmock rather than a live database.
//
// What they verify is precisely what a fake repository cannot: that the SQL is
// issued with the arguments we think, that driver errors are translated into the
// right HTTP semantics by constraint name, and — most importantly — that the
// transactional paths commit on success and roll back on failure.
//
// What they cannot verify is that the SQL is valid against the real schema.
// That is the job of the integration tests in tests/integration, which run the
// same repositories against a real PostgreSQL instance.

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, mock.ExpectationsWereMet(), "not all expected queries were issued")
		mock.Close()
	})
	return mock
}

// pgError builds the driver error PostgreSQL would return for a violated
// constraint, so the repository's classification can be tested without one.
func pgError(code, constraint string) error {
	return &pgconn.PgError{Code: code, ConstraintName: constraint}
}

func TestRepositoryCreateInsertsAndReturnsTimestamps(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	created := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	u := &domain.User{
		ID: uuid.New(), Email: "ben@example.com", Username: "ben_siregar",
		DisplayName: "Ben Siregar", PasswordHash: "$2a$10$hash", Role: domain.RoleUser,
	}

	mock.ExpectQuery(`INSERT INTO users`).
		WithArgs(u.ID, u.Email, u.Username, u.DisplayName, u.PasswordHash, u.Role).
		WillReturnRows(pgxmock.NewRows([]string{"created_at", "updated_at"}).AddRow(created, created))

	require.NoError(t, repo.Create(context.Background(), u))
	assert.Equal(t, created, u.CreatedAt)
	assert.Equal(t, created, u.UpdatedAt)
}

// TestRepositoryCreateTranslatesUniqueViolationsByConstraintName is why the
// constraint names are constants rather than string matches on the driver
// message: a Postgres release that rewords the message must not turn a 409 into
// a 500.
func TestRepositoryCreateTranslatesUniqueViolationsByConstraintName(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		constraint  string
		wantCode    apierr.Code
		wantMessage string
	}{
		"duplicate email": {
			constraint: constraintUserEmailUnique, wantCode: apierr.CodeConflict,
			wantMessage: "an account with this email address already exists",
		},
		"duplicate username": {
			constraint: constraintUserUsernameUnique, wantCode: apierr.CodeConflict,
			wantMessage: "this username is already taken",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mock := newMockPool(t)
			repo := NewRepository(mock)

			mock.ExpectQuery(`INSERT INTO users`).WithArgs(anyArgs(6)...).WillReturnError(pgError("23505", tc.constraint))

			err := repo.Create(context.Background(), &domain.User{ID: uuid.New()})

			require.Error(t, err)
			appErr := apierr.From(err)
			assert.Equal(t, tc.wantCode, appErr.Code)
			assert.Equal(t, tc.wantMessage, appErr.Message)
		})
	}
}

// A CHECK violation means the application's validation and the database's
// constraints have drifted apart. That is our bug, so it must be a 500 that
// gets investigated, not a 4xx blamed on the client.
func TestRepositoryCreateReportsCheckViolationsAsInternal(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	mock.ExpectQuery(`INSERT INTO users`).WithArgs(anyArgs(6)...).WillReturnError(pgError("23514", "users_username_format"))

	err := repo.Create(context.Background(), &domain.User{ID: uuid.New()})

	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

func TestRepositoryByIDMapsNoRowsTo404(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id := uuid.New()
	mock.ExpectQuery(`SELECT .* FROM users WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows(userScanColumns()))

	_, err := repo.ByID(context.Background(), id)

	appErr := apierr.From(err)
	assert.Equal(t, apierr.CodeNotFound, appErr.Code)
	assert.Equal(t, "user not found", appErr.Message)
}

func TestRepositoryByEmailScansTheWholeRow(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id := uuid.New()
	now := time.Now().UTC().Truncate(time.Second)
	mock.ExpectQuery(`SELECT .* FROM users WHERE email = \$1`).
		WithArgs("ben@example.com").
		WillReturnRows(pgxmock.NewRows(userScanColumns()).
			AddRow(id, "ben@example.com", "ben_siregar", "Ben Siregar", "$2a$10$hash", domain.RoleAdmin, now, now))

	got, err := repo.ByEmail(context.Background(), "ben@example.com")

	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, domain.RoleAdmin, got.Role)
	assert.Equal(t, "$2a$10$hash", got.PasswordHash)
}

func TestSessionRepositoryCreate(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	s := &Session{
		ID: uuid.New(), UserID: uuid.New(), TokenHash: make([]byte, 32),
		ExpiresAt: time.Now().Add(time.Hour), UserAgent: "curl/8.0",
	}

	mock.ExpectQuery(`INSERT INTO refresh_tokens`).
		WithArgs(s.ID, s.UserID, s.TokenHash, s.ExpiresAt, s.UserAgent).
		WillReturnRows(pgxmock.NewRows([]string{"created_at"}).AddRow(time.Now()))

	assert.NoError(t, repo.Create(context.Background(), s))
}

func TestSessionRepositoryByHashReportsMissingSession(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	mock.ExpectQuery(`SELECT .* FROM refresh_tokens WHERE token_hash = \$1`).
		WithArgs(anyArgs(1)...).WillReturnRows(pgxmock.NewRows(sessionScanColumns()))

	_, err := repo.ByHash(context.Background(), make([]byte, 32))

	// A sentinel, not an apierr: the caller decides that a missing session means
	// 401 rather than 404.
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

// TestSessionRepositoryRotateCommitsBothStatements is the happy path of the
// service's only cross-statement invariant.
func TestSessionRepositoryRotateCommitsBothStatements(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	presented := make([]byte, 32)
	next := &Session{
		ID: uuid.New(), UserID: uuid.New(), TokenHash: []byte("next-hash"),
		ExpiresAt: time.Now().Add(time.Hour),
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE refresh_tokens`).
		WithArgs(presented, next.ID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(`INSERT INTO refresh_tokens`).WithArgs(anyArgs(5)...).
		WillReturnRows(pgxmock.NewRows([]string{"created_at"}).AddRow(time.Now()))
	mock.ExpectCommit()

	assert.NoError(t, repo.Rotate(context.Background(), presented, next))
}

// TestSessionRepositoryRotateRollsBackWhenTheTokenIsAlreadyUsed is the
// transaction-rollback test the assessment asks for, on the path where it
// matters most: if the revoke matches no row the replacement must never be
// inserted, or a replayed token would mint a working session.
func TestSessionRepositoryRotateRollsBackWhenTheTokenIsAlreadyUsed(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	presented := make([]byte, 32)
	next := &Session{ID: uuid.New(), UserID: uuid.New(), TokenHash: []byte("next-hash")}

	mock.ExpectBegin()
	// Zero rows affected: the "AND revoked_at IS NULL AND expires_at > now()"
	// guard did not match.
	mock.ExpectExec(`UPDATE refresh_tokens`).
		WithArgs(presented, next.ID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err := repo.Rotate(context.Background(), presented, next)

	assert.ErrorIs(t, err, ErrSessionNotFound)
}

// TestSessionRepositoryRotateRollsBackWhenTheInsertFails covers the other
// direction: a failure after the revoke must not leave the user logged out with
// no replacement token.
func TestSessionRepositoryRotateRollsBackWhenTheInsertFails(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	presented := make([]byte, 32)
	next := &Session{ID: uuid.New(), UserID: uuid.New(), TokenHash: []byte("next-hash")}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE refresh_tokens`).WithArgs(anyArgs(2)...).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(`INSERT INTO refresh_tokens`).WithArgs(anyArgs(5)...).WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	err := repo.Rotate(context.Background(), presented, next)

	require.Error(t, err)
	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

func TestSessionRepositoryRotateSurfacesBeginFailure(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))

	err := repo.Rotate(context.Background(), make([]byte, 32), &Session{ID: uuid.New()})

	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

func TestSessionRepositoryRevokeIsIdempotent(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	// Zero rows affected — the token was unknown or already revoked. Logout must
	// still report success.
	mock.ExpectExec(`UPDATE refresh_tokens SET revoked_at`).WithArgs(anyArgs(1)...).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	assert.NoError(t, repo.Revoke(context.Background(), make([]byte, 32)))
}

func TestSessionRepositoryRevokeAllReportsCount(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	userID := uuid.New()
	mock.ExpectExec(`UPDATE refresh_tokens SET revoked_at`).
		WithArgs(userID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 4))

	n, err := repo.RevokeAllForUser(context.Background(), userID)

	require.NoError(t, err)
	assert.Equal(t, int64(4), n)
}

func TestSessionRepositoryDeleteExpired(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewSessionRepository(mock)

	cutoff := time.Now()
	mock.ExpectExec(`DELETE FROM refresh_tokens`).
		WithArgs(cutoff).
		WillReturnResult(pgxmock.NewResult("DELETE", 17))

	n, err := repo.DeleteExpired(context.Background(), cutoff)

	require.NoError(t, err)
	assert.Equal(t, int64(17), n)
}

func userScanColumns() []string {
	return []string{"id", "email", "username", "display_name", "password_hash", "role", "created_at", "updated_at"}
}

func sessionScanColumns() []string {
	return []string{"id", "user_id", "token_hash", "expires_at", "revoked_at", "replaced_by", "user_agent", "created_at"}
}

// anyArgs returns n wildcard argument matchers, for expectations where the
// argument values are not what the test is about.
func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}
