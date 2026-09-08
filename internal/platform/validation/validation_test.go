package validation

import (
	"net/http"
	"testing"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type registration struct {
	Email       string `json:"email"        validate:"required,email,max=254"`
	Username    string `json:"username"     validate:"required,username"`
	DisplayName string `json:"display_name" validate:"required,min=1,max=80"`
	Password    string `json:"password"     validate:"required,min=8,max=72"`
	Status      string `json:"status"       validate:"omitempty,oneof=draft published"`
}

func valid() registration {
	return registration{
		Email:       "ben@example.com",
		Username:    "ben_siregar",
		DisplayName: "Ben Siregar",
		Password:    "a-long-enough-password",
	}
}

func TestStructAcceptsValidInput(t *testing.T) {
	t.Parallel()
	assert.NoError(t, New().Struct(valid()))
}

// TestStructReportsJSONFieldNames is the reason this package exists rather than
// using the library's errors directly: a client that sent "display_name" must
// be told about "display_name", not about the Go field DisplayName.
func TestStructReportsJSONFieldNames(t *testing.T) {
	t.Parallel()

	in := valid()
	in.DisplayName = ""

	err := New().Struct(in)

	fields := fieldMap(t, err)
	assert.Contains(t, fields, "display_name")
	assert.NotContains(t, fields, "DisplayName")
}

func TestStructReportsEveryFailedFieldAtOnce(t *testing.T) {
	t.Parallel()

	err := New().Struct(registration{})

	appErr := apierr.From(err)
	require.Equal(t, apierr.CodeValidation, appErr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, appErr.Status)

	fields := fieldMap(t, err)
	// A caller fixing a form should not need four round trips to discover four
	// problems.
	assert.Len(t, fields, 4)
	for _, name := range []string{"email", "username", "display_name", "password"} {
		assert.Equal(t, "is required", fields[name])
	}
}

func TestStructMessagesAreActionable(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mutate    func(*registration)
		field     string
		wantMatch string
	}{
		"bad email":       {func(r *registration) { r.Email = "not-an-email" }, "email", "must be a valid email address"},
		"short password":  {func(r *registration) { r.Password = "short" }, "password", "must be at least 8 characters long"},
		"long password":   {func(r *registration) { r.Password = repeat(73) }, "password", "must be at most 72 characters long"},
		"long email":      {func(r *registration) { r.Email = repeat(250) + "@example.com" }, "email", "must be at most 254 characters long"},
		"unknown status":  {func(r *registration) { r.Status = "archived" }, "status", "must be one of: draft, published"},
		"bad username":    {func(r *registration) { r.Username = "no spaces allowed" }, "username", "must be 3-30 characters"},
		"short username":  {func(r *registration) { r.Username = "ab" }, "username", "must be 3-30 characters"},
		"long username":   {func(r *registration) { r.Username = repeat(31) }, "username", "must be 3-30 characters"},
		"empty displayed": {func(r *registration) { r.DisplayName = "" }, "display_name", "is required"},
	}

	v := New()
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			in := valid()
			tc.mutate(&in)

			err := v.Struct(in)

			require.Error(t, err)
			fields := fieldMap(t, err)
			require.Contains(t, fields, tc.field)
			assert.Contains(t, fields[tc.field], tc.wantMatch)
		})
	}
}

// TestUsernameRuleMatchesTheDatabaseCheck keeps the application rule and the
// users_username_format CHECK in migrations/0001_init.up.sql aligned. If they
// drift, a request that passes validation fails at the database with a 500.
func TestUsernameRuleMatchesTheDatabaseCheck(t *testing.T) {
	t.Parallel()

	v := New()
	accepted := []string{"abc", "ben_siregar", "user-123", repeat(30), "ABC"}
	rejected := []string{"", "ab", repeat(31), "has space", "dot.name", "emoji😀", "semi;colon"}

	for _, name := range accepted {
		in := valid()
		in.Username = name
		assert.NoError(t, v.Struct(in), "username %q should be accepted", name)
	}
	for _, name := range rejected {
		in := valid()
		in.Username = name
		assert.Error(t, v.Struct(in), "username %q should be rejected", name)
	}
}

// TestStructTreatsUnvalidatableInputAsServerError: passing a non-struct is a
// programming error, and reporting it as a client validation failure would send
// a confusing 422 for a bug on our side.
func TestStructTreatsUnvalidatableInputAsServerError(t *testing.T) {
	t.Parallel()

	err := New().Struct("not a struct")

	require.Error(t, err)
	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

func TestOmitemptySkipsAbsentOptionalFields(t *testing.T) {
	t.Parallel()

	in := valid()
	in.Status = "" // omitempty: absent is fine

	assert.NoError(t, New().Struct(in))
}

func fieldMap(t *testing.T, err error) map[string]string {
	t.Helper()
	require.Error(t, err)
	appErr := apierr.From(err)
	require.NotNil(t, appErr)
	out := make(map[string]string, len(appErr.Fields))
	for _, f := range appErr.Fields {
		out[f.Field] = f.Message
	}
	return out
}

func repeat(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
