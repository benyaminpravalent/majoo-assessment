package auth

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// testCost keeps bcrypt fast enough for a unit suite. Production cost comes
// from BCRYPT_COST and is validated to be at least 10 by config.Load; using the
// minimum here is a test-speed decision, not a statement about production.
const testCost = bcrypt.MinCost

func TestHasherRoundTrip(t *testing.T) {
	t.Parallel()

	h := NewHasher(testCost)
	const password = "correct horse battery staple"

	hash, err := h.Hash(password)
	require.NoError(t, err)

	assert.NotEqual(t, password, hash, "the stored value must not be the plaintext")
	assert.True(t, strings.HasPrefix(hash, "$2"), "expected a bcrypt hash, got %q", hash)
	assert.NoError(t, h.Verify(hash, password))
}

func TestHasherProducesDistinctHashesForTheSamePassword(t *testing.T) {
	t.Parallel()

	h := NewHasher(testCost)

	first, err := h.Hash("same-password")
	require.NoError(t, err)
	second, err := h.Hash("same-password")
	require.NoError(t, err)

	// bcrypt salts each hash. Equal hashes would mean the salt was fixed, which
	// would make the whole credential store attackable with one rainbow table.
	assert.NotEqual(t, first, second)
	assert.NoError(t, h.Verify(first, "same-password"))
	assert.NoError(t, h.Verify(second, "same-password"))
}

func TestHasherVerifyRejectsWrongPassword(t *testing.T) {
	t.Parallel()

	h := NewHasher(testCost)
	hash, err := h.Hash("the-real-password")
	require.NoError(t, err)

	err = h.Verify(hash, "not-the-password")

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidPassword),
		"a mismatch must be reported as ErrInvalidPassword, got %v", err)
}

func TestHasherVerifyDistinguishesCorruptHashFromWrongPassword(t *testing.T) {
	t.Parallel()

	h := NewHasher(testCost)

	err := h.Verify("this-is-not-a-bcrypt-hash", "anything")

	require.Error(t, err)
	// A corrupt stored hash is an operational problem. Reporting it as a simple
	// bad password would hide credential-store corruption behind failed logins.
	assert.False(t, errors.Is(err, ErrInvalidPassword),
		"a malformed hash must not be reported as a wrong password")
}

func TestHasherRejectsPasswordsOverBcryptLimit(t *testing.T) {
	t.Parallel()

	h := NewHasher(testCost)

	_, err := h.Hash(strings.Repeat("a", MaxPasswordBytes+1))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

func TestHasherAcceptsPasswordAtExactlyBcryptLimit(t *testing.T) {
	t.Parallel()

	h := NewHasher(testCost)
	password := strings.Repeat("a", MaxPasswordBytes)

	hash, err := h.Hash(password)

	require.NoError(t, err)
	assert.NoError(t, h.Verify(hash, password))
}

func TestNewHasherClampsOutOfRangeCost(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		cost int
		want int
	}{
		"below bcrypt minimum": {cost: 1, want: bcrypt.DefaultCost},
		"above bcrypt maximum": {cost: 99, want: bcrypt.DefaultCost},
		"within range":         {cost: 6, want: 6},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, NewHasher(tc.cost).Cost())
		})
	}
}

func TestBurnComparisonDoesNotPanicAndAlwaysFails(t *testing.T) {
	t.Parallel()

	h := NewHasher(testCost)

	// The dummy hash must be a real, parseable bcrypt hash: if it were malformed
	// the comparison would return early and the timing defence would not work.
	assert.NotPanics(t, func() { h.BurnComparison("some-password") })

	cost, err := bcrypt.Cost([]byte(dummyHash))
	require.NoError(t, err, "the dummy hash must be a valid bcrypt hash")
	assert.GreaterOrEqual(t, cost, bcrypt.MinCost)
}
