// Package user owns registration, authentication, session lifecycle and the
// public user profile.
//
// The package is organised by feature rather than by layer: the repository, the
// service and the HTTP handler for users all live here. That keeps a change to
// "how a user is represented" inside one directory, and it makes the dependency
// direction obvious — handler depends on service depends on repository, and
// nothing points back.
package user

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Constraint names from migrations/0001_init.up.sql. They are referenced rather
// than pattern-matched on the driver message, so a Postgres upgrade that
// rewords the error text cannot silently turn a 409 into a 500.
const (
	constraintUserEmailUnique    = "users_email_unique"
	constraintUserUsernameUnique = "users_username_unique"
)

// Repository reads and writes users.
type Repository struct {
	db postgres.Executor
}

// NewRepository returns a Repository backed by db.
func NewRepository(db postgres.Executor) *Repository {
	return &Repository{db: db}
}

// userColumns is the exact projection every read uses, so a new column cannot
// be added to one query and forgotten in another.
const userColumns = `id, email, username, display_name, password_hash, role, created_at, updated_at`

// Create inserts a new user.
//
// Uniqueness is enforced by the database rather than by a pre-flight SELECT.
// A check-then-insert has a race window that two concurrent registrations will
// eventually find; letting the unique index arbitrate is both correct and one
// round trip cheaper.
func (r *Repository) Create(ctx context.Context, u *domain.User) error {
	const q = `
		INSERT INTO users (id, email, username, display_name, password_hash, role)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`

	err := r.db.QueryRow(ctx, q, u.ID, u.Email, u.Username, u.DisplayName, u.PasswordHash, u.Role).
		Scan(&u.CreatedAt, &u.UpdatedAt)
	if err == nil {
		return nil
	}

	switch {
	case postgres.IsUniqueViolation(err, constraintUserEmailUnique):
		return apierr.Conflict("an account with this email address already exists").WithCause(err)
	case postgres.IsUniqueViolation(err, constraintUserUsernameUnique):
		return apierr.Conflict("this username is already taken").WithCause(err)
	case postgres.IsCheckViolation(err, ""):
		// The application validated this input already, so a CHECK failure here
		// means the two rule sets have drifted apart. Surface it as a 500 so it
		// is investigated rather than blamed on the client.
		return apierr.Internal(fmt.Errorf("user row rejected by a database constraint: %w", err))
	default:
		return apierr.Internal(fmt.Errorf("insert user: %w", err))
	}
}

// ByID returns the user with the given id.
func (r *Repository) ByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return r.one(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id)
}

// ByEmail returns the user registered with the given email address. The caller
// is expected to have normalised the address; see NormalizeEmail.
func (r *Repository) ByEmail(ctx context.Context, email string) (domain.User, error) {
	return r.one(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email)
}

func (r *Repository) one(ctx context.Context, q string, args ...any) (domain.User, error) {
	var u domain.User
	err := r.db.QueryRow(ctx, q, args...).Scan(
		&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return domain.User{}, postgres.NotFoundOr(fmt.Errorf("select user: %w", err), "user")
	}
	return u, nil
}

// UpdateProfile changes the mutable profile fields and returns the fresh row.
// updated_at is set by the users_set_updated_at trigger, not by this statement.
func (r *Repository) UpdateProfile(ctx context.Context, id uuid.UUID, displayName string) (domain.User, error) {
	const q = `
		UPDATE users
		   SET display_name = $2
		 WHERE id = $1
		RETURNING ` + userColumns

	var u domain.User
	err := r.db.QueryRow(ctx, q, id, displayName).Scan(
		&u.ID, &u.Email, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return domain.User{}, postgres.NotFoundOr(fmt.Errorf("update user profile: %w", err), "user")
	}
	return u, nil
}

// Session is a stored refresh token. The plaintext token never appears here:
// TokenHash is the SHA-256 digest handed over by auth.TokenService.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
	ExpiresAt  time.Time
	RevokedAt  *time.Time
	ReplacedBy *uuid.UUID
	UserAgent  string
	CreatedAt  time.Time
}

// Active reports whether the session can still be exchanged for a new token
// pair at time now.
func (s Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && s.ExpiresAt.After(now)
}

// SessionRepository stores refresh tokens.
//
// It holds a postgres.Store rather than an Executor because Rotate needs a
// transaction: revoking the presented token and issuing its replacement must
// either both happen or neither, otherwise a crash in between either leaves the
// user logged out or leaves two live tokens.
type SessionRepository struct {
	db postgres.Store
}

// NewSessionRepository returns a SessionRepository backed by db.
func NewSessionRepository(db postgres.Store) *SessionRepository {
	return &SessionRepository{db: db}
}

// ErrSessionNotFound reports that no stored session matches a token hash. It is
// a sentinel rather than an apierr so callers can decide the status code; the
// refresh handler deliberately returns 401, not 404.
var ErrSessionNotFound = errors.New("user: session not found")

const sessionColumns = `id, user_id, token_hash, expires_at, revoked_at, replaced_by, coalesce(user_agent, ''), created_at`

// Create stores a new session.
func (r *SessionRepository) Create(ctx context.Context, s *Session) error {
	const q = `
		INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, user_agent)
		VALUES ($1, $2, $3, $4, nullif($5, ''))
		RETURNING created_at`

	if err := r.db.QueryRow(ctx, q, s.ID, s.UserID, s.TokenHash, s.ExpiresAt, s.UserAgent).
		Scan(&s.CreatedAt); err != nil {
		return apierr.Internal(fmt.Errorf("insert refresh token: %w", err))
	}
	return nil
}

// ByHash returns the session with the given token digest, or ErrSessionNotFound.
func (r *SessionRepository) ByHash(ctx context.Context, hash []byte) (Session, error) {
	s, err := scanSession(r.db.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM refresh_tokens WHERE token_hash = $1`, hash))
	if err != nil {
		if postgres.IsNoRows(err) {
			return Session{}, ErrSessionNotFound
		}
		return Session{}, apierr.Internal(fmt.Errorf("select refresh token: %w", err))
	}
	return s, nil
}

// Rotate atomically revokes the presented session and stores its replacement.
//
// The UPDATE is guarded by `revoked_at IS NULL`, so two concurrent refreshes
// using the same token cannot both succeed: exactly one updates a row, and the
// loser is told the session is gone. That single predicate is what turns token
// rotation from a nice idea into a usable theft signal.
func (r *SessionRepository) Rotate(ctx context.Context, presentedHash []byte, next *Session) error {
	return postgres.InTx(ctx, r.db, func(tx pgx.Tx) error {
		const revoke = `
			UPDATE refresh_tokens
			   SET revoked_at = now(), replaced_by = $2
			 WHERE token_hash = $1
			   AND revoked_at IS NULL
			   AND expires_at > now()`

		tag, err := tx.Exec(ctx, revoke, presentedHash, next.ID)
		if err != nil {
			return apierr.Internal(fmt.Errorf("revoke refresh token: %w", err))
		}
		if tag.RowsAffected() == 0 {
			// Either the token never existed, was already used, or has expired.
			// All three mean the same thing to the client, and the transaction
			// rolls back so no replacement is created.
			return ErrSessionNotFound
		}

		const insert = `
			INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at, user_agent)
			VALUES ($1, $2, $3, $4, nullif($5, ''))
			RETURNING created_at`

		if err := tx.QueryRow(ctx, insert, next.ID, next.UserID, next.TokenHash, next.ExpiresAt, next.UserAgent).
			Scan(&next.CreatedAt); err != nil {
			return apierr.Internal(fmt.Errorf("insert rotated refresh token: %w", err))
		}
		return nil
	})
}

// Revoke marks a single session as revoked. Revoking an unknown or
// already-revoked token is not an error: logout must be idempotent.
func (r *SessionRepository) Revoke(ctx context.Context, hash []byte) error {
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`
	if _, err := r.db.Exec(ctx, q, hash); err != nil {
		return apierr.Internal(fmt.Errorf("revoke refresh token: %w", err))
	}
	return nil
}

// RevokeAllForUser ends every live session for a user and reports how many were
// revoked. Used by "log out everywhere" and, in an incident, by an operator.
func (r *SessionRepository) RevokeAllForUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	const q = `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`
	tag, err := r.db.Exec(ctx, q, userID)
	if err != nil {
		return 0, apierr.Internal(fmt.Errorf("revoke all refresh tokens: %w", err))
	}
	return tag.RowsAffected(), nil
}

// DeleteExpired removes sessions that expired before cutoff, returning the
// number deleted. Intended for a periodic maintenance job.
func (r *SessionRepository) DeleteExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM refresh_tokens WHERE expires_at < $1`
	tag, err := r.db.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, apierr.Internal(fmt.Errorf("delete expired refresh tokens: %w", err))
	}
	return tag.RowsAffected(), nil
}

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &s.ExpiresAt, &s.RevokedAt, &s.ReplacedBy, &s.UserAgent, &s.CreatedAt)
	return s, err
}
