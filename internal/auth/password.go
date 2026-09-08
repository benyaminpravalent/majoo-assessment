// Package auth implements credential hashing and the token formats used to
// authenticate API callers.
//
// Two token types are deliberately used:
//
//   - a short-lived, stateless JWT access token, so the hot path (every
//     authenticated request) needs no database round trip;
//   - a long-lived, opaque refresh token whose SHA-256 digest is stored in
//     PostgreSQL, so sessions can actually be revoked.
//
// A stateless-only design cannot revoke; a stateful-only design puts a database
// read in front of every request. This split is the usual compromise, and its
// cost is stated plainly: an access token stays valid until it expires, so the
// access TTL is the worst-case revocation delay.
package auth

import (
	"errors"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// MaxPasswordBytes is bcrypt's hard input limit. Anything longer is rejected by
// the algorithm, so the DTOs enforce the same bound and return a field error
// instead of letting a 500 escape from the hashing call.
const MaxPasswordBytes = 72

// ErrInvalidPassword reports that a plaintext password did not match a hash.
// It is a sentinel rather than an *apierr.Error because the caller decides how
// to present it: the login handler must not reveal whether the account exists.
var ErrInvalidPassword = errors.New("auth: password does not match hash")

// Hasher hashes and verifies passwords with bcrypt.
//
// bcrypt was chosen over Argon2id for one practical reason: it needs no tuning
// beyond a single cost factor, it is in the standard extended library, and its
// output is self-describing, so raising the cost later does not invalidate
// existing hashes. Argon2id is the stronger primitive and would be the choice
// if this service owned a large credential database; the migration path is
// recorded in docs/architecture-decisions.md, ADR-004.
type Hasher struct {
	cost int
}

// NewHasher returns a Hasher using the given bcrypt cost. Costs outside
// bcrypt's accepted range fall back to the library default; config.Load already
// rejects those, so this is only a safety net.
func NewHasher(cost int) Hasher {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = bcrypt.DefaultCost
	}
	return Hasher{cost: cost}
}

// Cost returns the configured bcrypt cost.
func (h Hasher) Cost() int { return h.cost }

// Hash returns the bcrypt hash of plain.
func (h Hasher) Hash(plain string) (string, error) {
	if len(plain) > MaxPasswordBytes {
		return "", fmt.Errorf("auth: password exceeds %d bytes", MaxPasswordBytes)
	}
	digest, err := bcrypt.GenerateFromPassword([]byte(plain), h.cost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(digest), nil
}

// Verify reports whether plain matches hash, returning ErrInvalidPassword on a
// mismatch and a wrapped error if the stored hash is unusable.
//
// bcrypt's own comparison is constant-time with respect to the hash, so no
// additional timing defence is needed here. What does matter is at the call
// site: see user.Service.Login, which runs a dummy comparison for unknown
// accounts so that "no such user" and "wrong password" take similar time.
func (h Hasher) Verify(hash, plain string) error {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
		return ErrInvalidPassword
	default:
		// A malformed or truncated hash lands here. That is a data-integrity
		// problem, not a failed login, and must be distinguishable in the logs.
		return fmt.Errorf("auth: compare password: %w", err)
	}
}

// dummyHash is a valid bcrypt hash used only to burn a comparable amount of CPU
// when an account does not exist. Its plaintext is irrelevant and it can never
// authenticate anyone, because Login discards the result.
//
// Generated with cost 10 from a random string; kept at a fixed low cost so the
// dummy path stays cheap enough not to become its own denial-of-service vector
// while remaining the same order of magnitude as a real comparison.
const dummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// BurnComparison performs a throwaway bcrypt comparison. Login calls it when no
// user matches the submitted email so that response time does not disclose
// whether an address is registered.
func (h Hasher) BurnComparison(plain string) {
	_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(plain))
}
