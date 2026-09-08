package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testSecret = []byte("a-test-secret-that-is-long-enough-32")
	fixedNow   = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
)

func newTestTokens(t *testing.T, now func() time.Time) *TokenService {
	t.Helper()
	if now == nil {
		now = func() time.Time { return fixedNow }
	}
	return NewTokenService(testSecret, "test-issuer", "test-audience",
		15*time.Minute, 24*time.Hour, WithClock(now))
}

func TestIssueAndParseAccessToken(t *testing.T) {
	t.Parallel()

	svc := newTestTokens(t, nil)
	userID := uuid.New()

	token, expiresAt, err := svc.IssueAccessToken(userID, domain.RoleAdmin)
	require.NoError(t, err)
	assert.Equal(t, fixedNow.Add(15*time.Minute), expiresAt)

	actor, err := svc.ParseAccessToken(token)

	require.NoError(t, err)
	assert.Equal(t, userID, actor.UserID)
	assert.Equal(t, domain.RoleAdmin, actor.Role)
}

func TestIssuedTokenCarriesEveryVerifiedClaim(t *testing.T) {
	t.Parallel()

	svc := newTestTokens(t, nil)
	userID := uuid.New()

	raw, _, err := svc.IssueAccessToken(userID, domain.RoleUser)
	require.NoError(t, err)

	var claims Claims
	_, _, err = jwt.NewParser().ParseUnverified(raw, &claims)
	require.NoError(t, err)

	assert.Equal(t, userID.String(), claims.Subject)
	assert.Equal(t, "test-issuer", claims.Issuer)
	assert.Contains(t, claims.Audience, "test-audience")
	assert.Equal(t, domain.RoleUser, claims.Role)
	assert.NotEmpty(t, claims.ID, "jti must be set so a token can be named in an audit log")
	require.NotNil(t, claims.ExpiresAt)
	require.NotNil(t, claims.IssuedAt)
	require.NotNil(t, claims.NotBefore)
	assert.Equal(t, fixedNow.Add(15*time.Minute).Unix(), claims.ExpiresAt.Unix())
}

func TestParseAccessTokenRejectsExpiredToken(t *testing.T) {
	t.Parallel()

	clock := fixedNow
	svc := newTestTokens(t, func() time.Time { return clock })

	token, _, err := svc.IssueAccessToken(uuid.New(), domain.RoleUser)
	require.NoError(t, err)

	// One second past expiry, with no leeway configured.
	clock = fixedNow.Add(15*time.Minute + time.Second)

	_, err = svc.ParseAccessToken(token)

	requireUnauthorized(t, err)
}

func TestParseAccessTokenRejectsTokenSignedWithAnotherSecret(t *testing.T) {
	t.Parallel()

	attacker := NewTokenService([]byte("a-different-secret-also-long-enough!"),
		"test-issuer", "test-audience", 15*time.Minute, 24*time.Hour, WithClock(func() time.Time { return fixedNow }))
	forged, _, err := attacker.IssueAccessToken(uuid.New(), domain.RoleAdmin)
	require.NoError(t, err)

	_, err = newTestTokens(t, nil).ParseAccessToken(forged)

	requireUnauthorized(t, err)
}

func TestParseAccessTokenRejectsWrongIssuerAndAudience(t *testing.T) {
	t.Parallel()

	tests := map[string]*TokenService{
		"wrong issuer": NewTokenService(testSecret, "other-issuer", "test-audience",
			15*time.Minute, 24*time.Hour, WithClock(func() time.Time { return fixedNow })),
		"wrong audience": NewTokenService(testSecret, "test-issuer", "other-audience",
			15*time.Minute, 24*time.Hour, WithClock(func() time.Time { return fixedNow })),
	}

	verifier := newTestTokens(t, nil)
	for name, issuer := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			token, _, err := issuer.IssueAccessToken(uuid.New(), domain.RoleUser)
			require.NoError(t, err)

			_, err = verifier.ParseAccessToken(token)

			requireUnauthorized(t, err)
		})
	}
}

// TestParseAccessTokenRejectsAlgNone is the algorithm-confusion case: a token
// whose header claims "alg":"none" must never be accepted, however well-formed
// its claims are.
func TestParseAccessTokenRejectsAlgNone(t *testing.T) {
	t.Parallel()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			Issuer:    "test-issuer",
			Audience:  jwt.ClaimStrings{"test-audience"},
			ExpiresAt: jwt.NewNumericDate(fixedNow.Add(time.Hour)),
		},
		Role: domain.RoleAdmin,
	}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, err = newTestTokens(t, nil).ParseAccessToken(unsigned)

	requireUnauthorized(t, err)
}

func TestParseAccessTokenRejectsMalformedInput(t *testing.T) {
	t.Parallel()

	svc := newTestTokens(t, nil)
	for name, raw := range map[string]string{
		"empty":           "",
		"not a jwt":       "hello",
		"two segments":    "aaa.bbb",
		"garbage payload": "eyJhbGciOiJIUzI1NiJ9.!!!.zzz",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := svc.ParseAccessToken(raw)
			requireUnauthorized(t, err)
		})
	}
}

func TestParseAccessTokenRejectsUnknownRole(t *testing.T) {
	t.Parallel()

	// Signed with the right key but carrying a role the application does not
	// know: it must not be accepted as a valid principal.
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			Issuer:    "test-issuer",
			Audience:  jwt.ClaimStrings{"test-audience"},
			ExpiresAt: jwt.NewNumericDate(fixedNow.Add(time.Hour)),
		},
		Role: domain.Role("superuser"),
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	require.NoError(t, err)

	_, err = newTestTokens(t, nil).ParseAccessToken(raw)

	requireUnauthorized(t, err)
}

func TestParseAccessTokenRejectsNonUUIDSubject(t *testing.T) {
	t.Parallel()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "not-a-uuid",
			Issuer:    "test-issuer",
			Audience:  jwt.ClaimStrings{"test-audience"},
			ExpiresAt: jwt.NewNumericDate(fixedNow.Add(time.Hour)),
		},
		Role: domain.RoleUser,
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	require.NoError(t, err)

	_, err = newTestTokens(t, nil).ParseAccessToken(raw)

	requireUnauthorized(t, err)
}

func TestParseAccessTokenRequiresExpiry(t *testing.T) {
	t.Parallel()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  uuid.NewString(),
			Issuer:   "test-issuer",
			Audience: jwt.ClaimStrings{"test-audience"},
			// No ExpiresAt: a token that never expires must be refused.
		},
		Role: domain.RoleUser,
	}
	raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(testSecret)
	require.NoError(t, err)

	_, err = newTestTokens(t, nil).ParseAccessToken(raw)

	requireUnauthorized(t, err)
}

// TestParseAccessTokenErrorMessageIsUniform checks that the API does not tell a
// caller *why* their token failed. Expired, forged and malformed must be
// indistinguishable from outside.
func TestParseAccessTokenErrorMessageIsUniform(t *testing.T) {
	t.Parallel()

	clock := fixedNow
	svc := newTestTokens(t, func() time.Time { return clock })

	expired, _, err := svc.IssueAccessToken(uuid.New(), domain.RoleUser)
	require.NoError(t, err)
	clock = fixedNow.Add(time.Hour)

	_, expiredErr := svc.ParseAccessToken(expired)
	_, malformedErr := svc.ParseAccessToken("garbage")

	assert.Equal(t, apierr.From(expiredErr).Message, apierr.From(malformedErr).Message)
}

func TestNewRefreshTokenIsOpaqueAndHashed(t *testing.T) {
	t.Parallel()

	svc := newTestTokens(t, nil)

	plaintext, digest, expiresAt, err := svc.NewRefreshToken()
	require.NoError(t, err)

	assert.Len(t, digest, 32, "the digest width must match the refresh_tokens_hash_is_sha256 CHECK")
	assert.Equal(t, fixedNow.Add(24*time.Hour), expiresAt)
	assert.NoError(t, ValidateRefreshTokenFormat(plaintext))

	// The stored digest must not be derivable from the token by simple encoding:
	// it is a hash, so the plaintext must not appear inside it.
	assert.NotContains(t, base64.RawURLEncoding.EncodeToString(digest), plaintext)
	assert.Equal(t, digest, HashRefreshToken(plaintext), "hashing must be deterministic")
}

func TestNewRefreshTokensAreUnique(t *testing.T) {
	t.Parallel()

	svc := newTestTokens(t, nil)
	seen := make(map[string]struct{}, 100)

	for i := 0; i < 100; i++ {
		plaintext, _, _, err := svc.NewRefreshToken()
		require.NoError(t, err)
		_, duplicate := seen[plaintext]
		require.False(t, duplicate, "refresh tokens must not repeat")
		seen[plaintext] = struct{}{}
	}
}

func TestValidateRefreshTokenFormat(t *testing.T) {
	t.Parallel()

	valid, _, _, err := newTestTokens(t, nil).NewRefreshToken()
	require.NoError(t, err)

	tests := map[string]struct {
		token   string
		wantErr bool
	}{
		"generated token":  {token: valid},
		"empty":            {token: "", wantErr: true},
		"not base64":       {token: "!!!!not-base64!!!!", wantErr: true},
		"wrong length":     {token: base64.RawURLEncoding.EncodeToString([]byte("short")), wantErr: true},
		"padded base64":    {token: base64.URLEncoding.EncodeToString(make([]byte, 32)), wantErr: true},
		"standard alphabt": {token: strings.Repeat("+", 43), wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRefreshTokenFormat(tc.token)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}

func requireUnauthorized(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	appErr := apierr.From(err)
	require.NotNil(t, appErr)
	assert.Equal(t, apierr.CodeUnauthorized, appErr.Code)
	assert.Equal(t, 401, appErr.Status)
}
