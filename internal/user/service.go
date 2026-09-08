package user

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/auth"
	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
	"github.com/google/uuid"
)

// userStore and sessionStore are declared here, at the point of use, rather
// than exported next to their implementations. That is the Go convention and it
// has a practical benefit: the interface lists exactly what this service needs,
// so a test double is four methods rather than a whole repository.
type userStore interface {
	Create(ctx context.Context, u *domain.User) error
	ByID(ctx context.Context, id uuid.UUID) (domain.User, error)
	ByEmail(ctx context.Context, email string) (domain.User, error)
	UpdateProfile(ctx context.Context, id uuid.UUID, displayName string) (domain.User, error)
}

type sessionStore interface {
	Create(ctx context.Context, s *Session) error
	ByHash(ctx context.Context, hash []byte) (Session, error)
	Rotate(ctx context.Context, presentedHash []byte, next *Session) error
	Revoke(ctx context.Context, hash []byte) error
	RevokeAllForUser(ctx context.Context, userID uuid.UUID) (int64, error)
}

// publisher is the subset of the event bus this service uses.
type publisher interface {
	Publish(ctx context.Context, e events.Event)
}

// Service implements the account and session use cases.
type Service struct {
	users    userStore
	sessions sessionStore
	hasher   auth.Hasher
	tokens   *auth.TokenService
	bus      publisher

	// now is injectable so session expiry can be tested deterministically.
	now func() time.Time
}

// NewService wires the account service.
func NewService(users userStore, sessions sessionStore, hasher auth.Hasher, tokens *auth.TokenService, bus publisher) *Service {
	return &Service{
		users:    users,
		sessions: sessions,
		hasher:   hasher,
		tokens:   tokens,
		bus:      bus,
		now:      time.Now,
	}
}

// TokenPair is what a successful authentication returns.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	ExpiresIn    int64
	User         domain.User
}

// RegisterInput is the validated input to Register.
type RegisterInput struct {
	Email       string
	Username    string
	DisplayName string
	Password    string
}

// Register creates an account and returns it.
//
// It deliberately does not log the user in. Returning tokens from registration
// is convenient but couples two flows, and it means a client that wants to
// verify an email address first has to throw the tokens away.
func (s *Service) Register(ctx context.Context, in RegisterInput) (domain.User, error) {
	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return domain.User{}, apierr.Internal(fmt.Errorf("hash password during registration: %w", err))
	}

	u := domain.User{
		ID:           uuid.New(),
		Email:        NormalizeEmail(in.Email),
		Username:     NormalizeUsername(in.Username),
		DisplayName:  strings.TrimSpace(in.DisplayName),
		PasswordHash: hash,
		// New accounts are always ordinary users. Promotion to admin is an
		// out-of-band operation (see scripts/promote_admin.sql) precisely so that
		// no request body can ever grant itself privileges.
		Role: domain.RoleUser,
	}

	if err := s.users.Create(ctx, &u); err != nil {
		return domain.User{}, err
	}

	s.bus.Publish(ctx, events.Event{
		Name:      "user.registered",
		ActorID:   u.ID.String(),
		SubjectID: u.ID.String(),
	})

	return u, nil
}

// errInvalidCredentials is the single response to every failed login. Login
// must not distinguish "no such account" from "wrong password": that
// distinction is a free account-enumeration oracle.
var errInvalidCredentials = apierr.Unauthorized("invalid email or password")

// Login verifies credentials and issues a token pair.
func (s *Service) Login(ctx context.Context, email, password, userAgent string) (TokenPair, error) {
	u, err := s.users.ByEmail(ctx, NormalizeEmail(email))
	if err != nil {
		if apierr.IsCode(err, apierr.CodeNotFound) {
			// Spend roughly the same CPU as a real comparison so that response
			// time does not leak whether the address is registered.
			s.hasher.BurnComparison(password)
			return TokenPair{}, errInvalidCredentials.WithCause(err)
		}
		return TokenPair{}, err
	}

	if err := s.hasher.Verify(u.PasswordHash, password); err != nil {
		if errors.Is(err, auth.ErrInvalidPassword) {
			return TokenPair{}, errInvalidCredentials.WithCause(err)
		}
		// A corrupt stored hash is an operational problem, not a bad password.
		return TokenPair{}, apierr.Internal(fmt.Errorf("verify password for user %s: %w", u.ID, err))
	}

	pair, err := s.issue(ctx, u, userAgent)
	if err != nil {
		return TokenPair{}, err
	}

	s.bus.Publish(ctx, events.Event{
		Name:      "user.logged_in",
		ActorID:   u.ID.String(),
		SubjectID: u.ID.String(),
	})

	return pair, nil
}

// Refresh exchanges a valid refresh token for a new pair, rotating the stored
// token in the process.
//
// Rotation means a stolen refresh token is usable at most once, and the theft
// becomes visible: whichever party refreshes second is rejected, and the
// rejection is logged with the session ID.
func (s *Service) Refresh(ctx context.Context, refreshToken, userAgent string) (TokenPair, error) {
	invalid := apierr.Unauthorized("invalid or expired refresh token")

	if err := auth.ValidateRefreshTokenFormat(refreshToken); err != nil {
		return TokenPair{}, invalid.WithCause(err)
	}

	presented := auth.HashRefreshToken(refreshToken)
	session, err := s.sessions.ByHash(ctx, presented)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return TokenPair{}, invalid.WithCause(err)
		}
		return TokenPair{}, err
	}

	if !session.Active(s.now()) {
		// Presenting an already-rotated token is the classic signature of a
		// stolen refresh token being replayed. Ending every session for the user
		// is the conservative response; it costs them one re-login.
		if session.RevokedAt != nil {
			logging.FromContext(ctx).Warn("refresh token reuse detected; revoking all sessions",
				slog.String("user_id", session.UserID.String()),
				slog.String("session_id", session.ID.String()))
			if _, revokeErr := s.sessions.RevokeAllForUser(ctx, session.UserID); revokeErr != nil {
				logging.FromContext(ctx).Error("failed to revoke sessions after token reuse",
					slog.String("user_id", session.UserID.String()),
					slog.Any("error", revokeErr))
			}
			s.bus.Publish(ctx, events.Event{
				Name:      "auth.refresh_token_reused",
				ActorID:   session.UserID.String(),
				SubjectID: session.ID.String(),
			})
		}
		return TokenPair{}, invalid
	}

	u, err := s.users.ByID(ctx, session.UserID)
	if err != nil {
		if apierr.IsCode(err, apierr.CodeNotFound) {
			// The account was deleted while a session was live.
			return TokenPair{}, invalid.WithCause(err)
		}
		return TokenPair{}, err
	}

	plaintext, digest, expiresAt, err := s.tokens.NewRefreshToken()
	if err != nil {
		return TokenPair{}, apierr.Internal(err)
	}
	next := &Session{
		ID:        uuid.New(),
		UserID:    u.ID,
		TokenHash: digest,
		ExpiresAt: expiresAt,
		UserAgent: truncateUserAgent(userAgent),
	}

	if err := s.sessions.Rotate(ctx, presented, next); err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			// Lost a race with a concurrent refresh or a logout.
			return TokenPair{}, invalid.WithCause(err)
		}
		return TokenPair{}, err
	}

	access, accessExp, err := s.tokens.IssueAccessToken(u.ID, u.Role)
	if err != nil {
		return TokenPair{}, apierr.Internal(err)
	}

	return TokenPair{
		AccessToken:  access,
		RefreshToken: plaintext,
		ExpiresAt:    accessExp,
		ExpiresIn:    int64(s.tokens.AccessTTL().Seconds()),
		User:         u,
	}, nil
}

// Logout revokes one session. It is idempotent and never reports failure for an
// unknown token: a client retrying a logout must not receive an error, and
// telling a caller that a token was unknown is another small oracle.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if err := auth.ValidateRefreshTokenFormat(refreshToken); err != nil {
		return nil
	}
	return s.sessions.Revoke(ctx, auth.HashRefreshToken(refreshToken))
}

// LogoutAll ends every session belonging to the actor.
func (s *Service) LogoutAll(ctx context.Context, actor domain.Actor) (int64, error) {
	n, err := s.sessions.RevokeAllForUser(ctx, actor.UserID)
	if err != nil {
		return 0, err
	}
	s.bus.Publish(ctx, events.Event{
		Name:       "user.logged_out_everywhere",
		ActorID:    actor.UserID.String(),
		SubjectID:  actor.UserID.String(),
		Attributes: map[string]any{"sessions_revoked": n},
	})
	return n, nil
}

// ByID returns a user profile.
func (s *Service) ByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return s.users.ByID(ctx, id)
}

// UpdateProfile changes the caller's own display name.
//
// There is no path for changing someone else's profile, not even for an admin:
// the actor's own ID is used, so the authorisation question cannot be got wrong
// at this call site.
func (s *Service) UpdateProfile(ctx context.Context, actor domain.Actor, displayName string) (domain.User, error) {
	return s.users.UpdateProfile(ctx, actor.UserID, strings.TrimSpace(displayName))
}

// issue mints a fresh access/refresh pair and stores the session.
func (s *Service) issue(ctx context.Context, u domain.User, userAgent string) (TokenPair, error) {
	access, accessExp, err := s.tokens.IssueAccessToken(u.ID, u.Role)
	if err != nil {
		return TokenPair{}, apierr.Internal(err)
	}

	plaintext, digest, refreshExp, err := s.tokens.NewRefreshToken()
	if err != nil {
		return TokenPair{}, apierr.Internal(err)
	}

	session := &Session{
		ID:        uuid.New(),
		UserID:    u.ID,
		TokenHash: digest,
		ExpiresAt: refreshExp,
		UserAgent: truncateUserAgent(userAgent),
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return TokenPair{}, err
	}

	return TokenPair{
		AccessToken:  access,
		RefreshToken: plaintext,
		ExpiresAt:    accessExp,
		ExpiresIn:    int64(s.tokens.AccessTTL().Seconds()),
		User:         u,
	}, nil
}

// maxUserAgentLen matches the refresh_tokens_user_agent_length CHECK. Clients
// control this header, so it is truncated rather than allowed to fail an INSERT.
const maxUserAgentLen = 512

func truncateUserAgent(ua string) string {
	if len(ua) <= maxUserAgentLen {
		return ua
	}
	return ua[:maxUserAgentLen]
}

// NormalizeEmail folds an address to the form stored in the database. Only case
// is normalised: stripping dots or plus-addressing would silently merge
// addresses that some providers treat as distinct.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// NormalizeUsername folds a username to the stored form. Usernames are
// case-insensitive for identity, so "Ben" and "ben" are the same account.
func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}
