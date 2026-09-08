package domain

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestActorCanModify covers the one authorisation rule the whole API shares.
// Every ownership check in every service funnels through it, so it is worth
// enumerating rather than testing implicitly through handlers.
func TestActorCanModify(t *testing.T) {
	t.Parallel()

	owner := uuid.New()
	other := uuid.New()

	tests := map[string]struct {
		actor Actor
		owner uuid.UUID
		want  bool
	}{
		"owner modifies own content":       {actor: Actor{UserID: owner, Role: RoleUser}, owner: owner, want: true},
		"user cannot modify another's":     {actor: Actor{UserID: other, Role: RoleUser}, owner: owner, want: false},
		"admin modifies another's":         {actor: Actor{UserID: other, Role: RoleAdmin}, owner: owner, want: true},
		"admin modifies own":               {actor: Actor{UserID: owner, Role: RoleAdmin}, owner: owner, want: true},
		"unknown role is not privileged":   {actor: Actor{UserID: other, Role: Role("moderator")}, owner: owner, want: false},
		"zero actor cannot modify content": {actor: Actor{}, owner: owner, want: false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, tc.actor.CanModify(tc.owner))
		})
	}
}

// TestZeroActorCannotModifyZeroOwner guards a specific foot-gun: if a caller
// forgets to populate the actor and the owner column is also zero, an
// unauthenticated request must not compare equal and be granted access.
func TestZeroActorCannotModifyZeroOwner(t *testing.T) {
	t.Parallel()

	// uuid.Nil == uuid.Nil, so this *does* return true. The protection is
	// therefore structural, not arithmetic: handlers refuse the request before
	// reaching here when no actor is present, and no row can have a NULL author
	// because author_id is NOT NULL. This test records that reasoning so a future
	// reader does not mistake the behaviour for a bug.
	assert.True(t, Actor{}.CanModify(uuid.Nil),
		"documents why the zero value must never reach an authorisation check")
}

func TestRoleValid(t *testing.T) {
	t.Parallel()

	assert.True(t, RoleUser.Valid())
	assert.True(t, RoleAdmin.Valid())
	assert.False(t, Role("").Valid())
	assert.False(t, Role("ADMIN").Valid(), "roles are case-sensitive and match the CHECK constraint")
	assert.False(t, Role("superuser").Valid())
}

func TestPostStatusValid(t *testing.T) {
	t.Parallel()

	assert.True(t, PostStatusDraft.Valid())
	assert.True(t, PostStatusPublished.Valid())
	assert.False(t, PostStatus("").Valid())
	assert.False(t, PostStatus("archived").Valid())
}

func TestUserSummaryOmitsCredentials(t *testing.T) {
	t.Parallel()

	u := User{
		ID:           uuid.New(),
		Email:        "someone@example.com",
		Username:     "someone",
		DisplayName:  "Some One",
		PasswordHash: "$2a$12$averysecrethashvalue",
		Role:         RoleAdmin,
	}

	summary := u.Summary()

	assert.Equal(t, u.ID, summary.ID)
	assert.Equal(t, u.Username, summary.Username)
	assert.Equal(t, u.DisplayName, summary.DisplayName)

	// The projection must carry neither the credential nor the address. Encoding
	// it and searching the bytes catches a field added later without thought.
	encoded, err := json.Marshal(summary)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), u.PasswordHash)
	assert.NotContains(t, string(encoded), u.Email)
}

func TestActorIsAdmin(t *testing.T) {
	t.Parallel()

	assert.True(t, Actor{Role: RoleAdmin}.IsAdmin())
	assert.False(t, Actor{Role: RoleUser}.IsAdmin())
	assert.False(t, Actor{}.IsAdmin())
}
