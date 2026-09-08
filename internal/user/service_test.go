package user

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/auth"
	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

var testClock = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)

func nowForTests() time.Time { return testClock }

type fixture struct {
	svc      *Service
	users    *fakeUserStore
	sessions *fakeSessionStore
	bus      *recordingBus
	tokens   *auth.TokenService
	clock    *time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	clock := testClock
	tokens := auth.NewTokenService(
		[]byte(strings.Repeat("s", 48)), "test-issuer", "test-audience",
		15*time.Minute, 24*time.Hour,
		auth.WithClock(func() time.Time { return clock }),
	)

	users := newFakeUserStore()
	sessions := newFakeSessionStore()
	bus := &recordingBus{}

	svc := NewService(users, sessions, auth.NewHasher(bcrypt.MinCost), tokens, bus)
	svc.now = func() time.Time { return clock }

	return &fixture{svc: svc, users: users, sessions: sessions, bus: bus, tokens: tokens, clock: &clock}
}

func validRegistration() RegisterInput {
	return RegisterInput{
		Email:       "Ben@Example.COM",
		Username:    "Ben_Siregar",
		DisplayName: "  Ben Siregar  ",
		Password:    "a-good-enough-password",
	}
}

func TestRegisterCreatesAnOrdinaryUser(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	u, err := f.svc.Register(context.Background(), validRegistration())

	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, u.ID)
	assert.Equal(t, domain.RoleUser, u.Role,
		"registration must never be able to grant the admin role")
	assert.Contains(t, f.bus.names(), "user.registered")
}

// TestRegisterNormalisesIdentity keeps the application's notion of identity
// aligned with the users_email_lowercase / users_username_lowercase CHECKs.
func TestRegisterNormalisesIdentity(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	u, err := f.svc.Register(context.Background(), validRegistration())

	require.NoError(t, err)
	assert.Equal(t, "ben@example.com", u.Email)
	assert.Equal(t, "ben_siregar", u.Username)
	assert.Equal(t, "Ben Siregar", u.DisplayName, "surrounding whitespace must be trimmed")
}

func TestRegisterStoresAHashNotThePlaintext(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()

	u, err := f.svc.Register(context.Background(), in)

	require.NoError(t, err)
	assert.NotEqual(t, in.Password, u.PasswordHash)
	assert.True(t, strings.HasPrefix(u.PasswordHash, "$2"))
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)))
}

func TestRegisterRejectsDuplicateEmail(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, err := f.svc.Register(context.Background(), validRegistration())
	require.NoError(t, err)

	second := validRegistration()
	second.Username = "someone_else"

	_, err = f.svc.Register(context.Background(), second)

	require.Error(t, err)
	assert.Equal(t, apierr.CodeConflict, apierr.From(err).Code)
}

// TestRegisterRejectsDuplicateEmailDifferingOnlyByCase proves the normalisation
// above is what makes the uniqueness constraint case-insensitive.
func TestRegisterRejectsDuplicateEmailDifferingOnlyByCase(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, err := f.svc.Register(context.Background(), validRegistration())
	require.NoError(t, err)

	second := validRegistration()
	second.Email = "BEN@EXAMPLE.COM"
	second.Username = "another_name"

	_, err = f.svc.Register(context.Background(), second)

	assert.Equal(t, apierr.CodeConflict, apierr.From(err).Code)
}

func TestRegisterRejectsDuplicateUsername(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	_, err := f.svc.Register(context.Background(), validRegistration())
	require.NoError(t, err)

	second := validRegistration()
	second.Email = "other@example.com"

	_, err = f.svc.Register(context.Background(), second)

	assert.Equal(t, apierr.CodeConflict, apierr.From(err).Code)
}

func TestLoginIssuesAUsableTokenPair(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	registered, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)

	pair, err := f.svc.Login(context.Background(), in.Email, in.Password, "curl/8.0")

	require.NoError(t, err)
	assert.Equal(t, registered.ID, pair.User.ID)
	assert.Equal(t, int64(900), pair.ExpiresIn)
	assert.NotEmpty(t, pair.RefreshToken)

	actor, err := f.tokens.ParseAccessToken(pair.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, registered.ID, actor.UserID)
	assert.Equal(t, domain.RoleUser, actor.Role)

	assert.Equal(t, 1, f.sessions.liveCount(), "a session must be recorded so it can be revoked")
	assert.Contains(t, f.bus.names(), "user.logged_in")
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)

	_, err = f.svc.Login(context.Background(), in.Email, "wrong-password", "")

	require.Error(t, err)
	assert.Equal(t, apierr.CodeUnauthorized, apierr.From(err).Code)
	assert.Zero(t, f.sessions.liveCount(), "a failed login must not create a session")
}

// TestLoginDoesNotRevealWhetherAnAccountExists is the account-enumeration
// defence: the two failure modes must be indistinguishable to a caller.
func TestLoginDoesNotRevealWhetherAnAccountExists(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)

	_, wrongPassword := f.svc.Login(context.Background(), in.Email, "wrong-password", "")
	_, noSuchUser := f.svc.Login(context.Background(), "nobody@example.com", "any-password", "")

	require.Error(t, wrongPassword)
	require.Error(t, noSuchUser)

	a, b := apierr.From(wrongPassword), apierr.From(noSuchUser)
	assert.Equal(t, a.Code, b.Code)
	assert.Equal(t, a.Status, b.Status)
	assert.Equal(t, a.Message, b.Message)
}

func TestLoginIsCaseInsensitiveOnEmail(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)

	_, err = f.svc.Login(context.Background(), "  BEN@example.com ", in.Password, "")

	assert.NoError(t, err)
}

func TestLoginPropagatesInfrastructureFailures(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.users.getErr = apierr.Internal(errors.New("connection reset by peer"))

	_, err := f.svc.Login(context.Background(), "ben@example.com", "password", "")

	// A database outage must not be reported to the caller as bad credentials.
	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

func TestRefreshRotatesTheStoredToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)
	pair, err := f.svc.Login(context.Background(), in.Email, in.Password, "")
	require.NoError(t, err)

	refreshed, err := f.svc.Refresh(context.Background(), pair.RefreshToken, "")

	require.NoError(t, err)
	assert.NotEqual(t, pair.RefreshToken, refreshed.RefreshToken,
		"rotation must issue a new refresh token")
	assert.NotEmpty(t, refreshed.AccessToken)
	assert.Equal(t, 1, f.sessions.liveCount(), "the old session must be revoked as the new one is created")
}

// TestRefreshRejectsAReplayedToken is the security property rotation exists for.
func TestRefreshRejectsAReplayedToken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)
	pair, err := f.svc.Login(context.Background(), in.Email, in.Password, "")
	require.NoError(t, err)

	_, err = f.svc.Refresh(context.Background(), pair.RefreshToken, "")
	require.NoError(t, err)

	// The attacker (or a buggy client) presents the already-rotated token.
	_, err = f.svc.Refresh(context.Background(), pair.RefreshToken, "")

	require.Error(t, err)
	assert.Equal(t, apierr.CodeUnauthorized, apierr.From(err).Code)
}

// TestRefreshReuseRevokesEverySession: presenting a rotated token is the classic
// signature of theft, so the conservative response is to end all sessions.
func TestRefreshReuseRevokesEverySession(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)

	first, err := f.svc.Login(context.Background(), in.Email, in.Password, "device-a")
	require.NoError(t, err)
	_, err = f.svc.Login(context.Background(), in.Email, in.Password, "device-b")
	require.NoError(t, err)
	require.Equal(t, 2, f.sessions.liveCount())

	_, err = f.svc.Refresh(context.Background(), first.RefreshToken, "")
	require.NoError(t, err)

	_, err = f.svc.Refresh(context.Background(), first.RefreshToken, "")

	require.Error(t, err)
	assert.Zero(t, f.sessions.liveCount(), "token reuse must end every session for the account")
	assert.Contains(t, f.bus.names(), "auth.refresh_token_reused")
}

func TestRefreshRejectsExpiredSession(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)
	pair, err := f.svc.Login(context.Background(), in.Email, in.Password, "")
	require.NoError(t, err)

	*f.clock = testClock.Add(25 * time.Hour) // refresh TTL is 24h

	_, err = f.svc.Refresh(context.Background(), pair.RefreshToken, "")

	require.Error(t, err)
	assert.Equal(t, apierr.CodeUnauthorized, apierr.From(err).Code)
}

func TestRefreshRejectsMalformedAndUnknownTokens(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	for name, token := range map[string]string{
		"empty":      "",
		"not base64": "!!!!!",
		"well formed but unknown": func() string {
			plaintext, _, _, err := f.tokens.NewRefreshToken()
			require.NoError(t, err)
			return plaintext
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.Refresh(context.Background(), token, "")
			require.Error(t, err)
			assert.Equal(t, apierr.CodeUnauthorized, apierr.From(err).Code)
		})
	}
}

func TestLogoutRevokesTheSession(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	_, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)
	pair, err := f.svc.Login(context.Background(), in.Email, in.Password, "")
	require.NoError(t, err)

	require.NoError(t, f.svc.Logout(context.Background(), pair.RefreshToken))

	assert.Zero(t, f.sessions.liveCount())

	_, err = f.svc.Refresh(context.Background(), pair.RefreshToken, "")
	assert.Error(t, err, "a logged-out token must not be refreshable")
}

// TestLogoutIsIdempotent: a client retrying a logout must not get an error, and
// an unknown token must not be reported as unknown.
func TestLogoutIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	assert.NoError(t, f.svc.Logout(context.Background(), "not-a-real-token"))
	assert.NoError(t, f.svc.Logout(context.Background(), ""))
}

func TestLogoutAllRevokesEverySession(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	in := validRegistration()
	registered, err := f.svc.Register(context.Background(), in)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err := f.svc.Login(context.Background(), in.Email, in.Password, "device")
		require.NoError(t, err)
	}

	n, err := f.svc.LogoutAll(context.Background(),
		domain.Actor{UserID: registered.ID, Role: domain.RoleUser})

	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	assert.Zero(t, f.sessions.liveCount())
	assert.Contains(t, f.bus.names(), "user.logged_out_everywhere")
}

func TestUpdateProfileOnlyEverTouchesTheCallersOwnRow(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	mine, err := f.svc.Register(context.Background(), validRegistration())
	require.NoError(t, err)

	other := validRegistration()
	other.Email = "other@example.com"
	other.Username = "other_person"
	theirs, err := f.svc.Register(context.Background(), other)
	require.NoError(t, err)

	// Even an administrator's own actor ID is what gets written: there is no
	// parameter through which another user's profile could be targeted.
	updated, err := f.svc.UpdateProfile(context.Background(),
		domain.Actor{UserID: mine.ID, Role: domain.RoleAdmin}, "  New Name  ")

	require.NoError(t, err)
	assert.Equal(t, mine.ID, updated.ID)
	assert.Equal(t, "New Name", updated.DisplayName)

	unchanged, err := f.svc.ByID(context.Background(), theirs.ID)
	require.NoError(t, err)
	assert.Equal(t, theirs.DisplayName, unchanged.DisplayName)
}

func TestByIDReturnsNotFoundForUnknownUser(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.svc.ByID(context.Background(), uuid.New())

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

func TestTruncateUserAgentMatchesTheColumnLimit(t *testing.T) {
	t.Parallel()

	assert.Len(t, truncateUserAgent(strings.Repeat("x", maxUserAgentLen+100)), maxUserAgentLen)
	assert.Equal(t, "curl/8.0", truncateUserAgent("curl/8.0"))
}

func TestSessionActive(t *testing.T) {
	t.Parallel()

	revoked := testClock
	future := testClock.Add(time.Hour)
	past := testClock.Add(-time.Hour)

	assert.True(t, Session{ExpiresAt: future}.Active(testClock))
	assert.False(t, Session{ExpiresAt: past}.Active(testClock), "expired sessions are not active")
	assert.False(t, Session{ExpiresAt: future, RevokedAt: &revoked}.Active(testClock),
		"revoked sessions are not active")
}
