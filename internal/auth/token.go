package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// signingMethod is fixed at HS256 and enforced on both sign and verify.
//
// Pinning it on verification is the defence against algorithm confusion: a
// library that trusts the token's own "alg" header will happily accept a token
// signed with "none", or accept an HMAC token signed with a public RSA key.
var signingMethod = jwt.SigningMethodHS256

// Claims is the access-token payload. Role is duplicated from the users table
// so that authorisation decisions need no database read; the cost is that a
// role change only takes effect for tokens issued after it, bounded by the
// access-token TTL.
type Claims struct {
	jwt.RegisteredClaims
	Role domain.Role `json:"role"`
}

// TokenService issues and verifies access tokens, and mints opaque refresh
// tokens.
type TokenService struct {
	secret     []byte
	issuer     string
	audience   string
	accessTTL  time.Duration
	refreshTTL time.Duration

	// now is injectable so expiry behaviour can be tested without sleeping.
	now func() time.Time
}

// TokenServiceOption customises a TokenService.
type TokenServiceOption func(*TokenService)

// WithClock replaces the time source. Test-only in practice.
func WithClock(now func() time.Time) TokenServiceOption {
	return func(s *TokenService) { s.now = now }
}

// NewTokenService builds a TokenService. The caller is responsible for having
// validated the secret length; config.Load enforces at least 32 bytes.
func NewTokenService(secret []byte, issuer, audience string, accessTTL, refreshTTL time.Duration, opts ...TokenServiceOption) *TokenService {
	s := &TokenService{
		secret:     secret,
		issuer:     issuer,
		audience:   audience,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// AccessTTL returns the configured access-token lifetime, which handlers report
// to clients as expires_in.
func (s *TokenService) AccessTTL() time.Duration { return s.accessTTL }

// RefreshTTL returns the configured refresh-token lifetime.
func (s *TokenService) RefreshTTL() time.Duration { return s.refreshTTL }

// IssueAccessToken returns a signed JWT for the given principal along with its
// expiry.
//
// Every registered claim that the verifier checks is populated here: iss and
// aud so a token minted for another service cannot be replayed against this
// one, exp and nbf/iat to bound its lifetime, and jti so a specific token can
// be named in an audit log or a future deny-list.
func (s *TokenService) IssueAccessToken(userID uuid.UUID, role domain.Role) (string, time.Time, error) {
	now := s.now().UTC()
	expiresAt := now.Add(s.accessTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    s.issuer,
			Audience:  jwt.ClaimStrings{s.audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        uuid.NewString(),
		},
		Role: role,
	}

	signed, err := jwt.NewWithClaims(signingMethod, claims).SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, expiresAt, nil
}

// ParseAccessToken verifies a token and returns the principal it names.
//
// All failures collapse to the same 401 with the same message. Telling a caller
// whether a token was expired, forged or simply malformed is a small oracle,
// and the distinction is preserved in the wrapped cause for the logs.
func (s *TokenService) ParseAccessToken(raw string) (domain.Actor, error) {
	unauthorized := func(cause error) (domain.Actor, error) {
		return domain.Actor{}, apierr.Unauthorized("invalid or expired access token").WithCause(cause)
	}

	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != signingMethod.Alg() {
			return nil, fmt.Errorf("unexpected signing algorithm %q", t.Method.Alg())
		}
		return s.secret, nil
	},
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(s.audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil {
		return unauthorized(err)
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return unauthorized(fmt.Errorf("subject %q is not a UUID: %w", claims.Subject, err))
	}
	if !claims.Role.Valid() {
		return unauthorized(fmt.Errorf("unknown role %q", claims.Role))
	}

	return domain.Actor{UserID: userID, Role: claims.Role}, nil
}

// refreshTokenBytes is the entropy of an opaque refresh token. 32 bytes is
// well beyond guessing range and matches the SHA-256 digest width stored in the
// database.
const refreshTokenBytes = 32

// ErrMalformedRefreshToken reports a refresh token that is not even the right
// shape, so no database lookup is worth performing.
var ErrMalformedRefreshToken = errors.New("auth: malformed refresh token")

// NewRefreshToken returns a fresh opaque token and the digest to store.
//
// The plaintext is returned to the client exactly once. Only the digest is
// persisted, so a database disclosure does not hand an attacker usable
// sessions — the same reasoning as never storing plaintext passwords.
func (s *TokenService) NewRefreshToken() (plaintext string, digest []byte, expiresAt time.Time, err error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, time.Time{}, fmt.Errorf("auth: generate refresh token: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	return plaintext, HashRefreshToken(plaintext), s.now().UTC().Add(s.refreshTTL), nil
}

// HashRefreshToken returns the digest stored for a refresh token.
//
// A plain SHA-256 is correct here, in contrast to passwords: the token is 256
// bits of uniform randomness, so there is no dictionary to attack and the
// slowness of bcrypt would buy nothing while making every refresh expensive.
func HashRefreshToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// ValidateRefreshTokenFormat cheaply rejects tokens that cannot be ours before
// the database is consulted.
func ValidateRefreshTokenFormat(plaintext string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(plaintext)
	if err != nil || len(decoded) != refreshTokenBytes {
		return ErrMalformedRefreshToken
	}
	return nil
}
