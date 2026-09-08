package post

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slugPattern is the posts_slug_format CHECK from migrations/0001_init.up.sql,
// transcribed. Every generated slug is asserted against it, so a change to
// Slugify that the database would reject fails here first.
var slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func TestSlugify(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		title string
		want  string
	}{
		"simple":                  {"Hello World", "hello-world"},
		"already a slug":          {"hello-world", "hello-world"},
		"mixed case":              {"HELLO World", "hello-world"},
		"punctuation":             {"Go 1.24: What's New?", "go-1-24-what-s-new"},
		"multiple spaces":         {"too    many   spaces", "too-many-spaces"},
		"leading and trailing":    {"  --Trimmed--  ", "trimmed"},
		"digits":                  {"Top 10 Tips", "top-10-tips"},
		"underscores become dash": {"snake_case_title", "snake-case-title"},
		"slashes":                 {"a/b/c", "a-b-c"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := Slugify(tc.title)

			assert.Equal(t, tc.want, got)
			assert.Regexp(t, slugPattern, got, "every slug must satisfy the database CHECK")
		})
	}
}

// TestSlugifyFallsBackForUnslugifiableTitles documents the deliberate
// limitation: non-ASCII is dropped rather than transliterated, because there is
// no correct language-independent transliteration and a half-right one is worse
// than a stable random fallback.
func TestSlugifyFallsBackForUnslugifiableTitles(t *testing.T) {
	t.Parallel()

	for _, title := range []string{"北京欢迎你", "!!!", "   ", "Ø", "ab"} {
		got := Slugify(title)

		assert.True(t, strings.HasPrefix(got, "post-"),
			"title %q should fall back to a generated slug, got %q", title, got)
		assert.Regexp(t, slugPattern, got)
		assert.GreaterOrEqual(t, len(got), 3)
	}
}

func TestSlugifyKeepsAsciiFromMixedTitles(t *testing.T) {
	t.Parallel()

	got := Slugify("Go in 北京 2026")

	assert.Equal(t, "go-in-2026", got)
	assert.Regexp(t, slugPattern, got)
}

// TestSlugifyRespectsTheLengthBudget leaves room under the 220-character CHECK
// for the disambiguating suffix a collision appends.
func TestSlugifyRespectsTheLengthBudget(t *testing.T) {
	t.Parallel()

	got := Slugify(strings.Repeat("word ", 200))

	assert.LessOrEqual(t, len(got), maxSlugLen)
	assert.Regexp(t, slugPattern, got, "truncation must not leave a trailing hyphen")
	assert.LessOrEqual(t, len(WithSuffix(got)), 220, "the suffixed slug must still fit the CHECK")
}

func TestSlugifyTruncationDoesNotLeaveATrailingHyphen(t *testing.T) {
	t.Parallel()

	// Construct a title whose cut point lands exactly on a separator.
	title := strings.Repeat("ab ", maxSlugLen)

	got := Slugify(title)

	assert.False(t, strings.HasSuffix(got, "-"))
	assert.Regexp(t, slugPattern, got)
}

func TestWithSuffixProducesAValidDistinctSlug(t *testing.T) {
	t.Parallel()

	base := "my-post"
	seen := map[string]struct{}{}

	for i := 0; i < 200; i++ {
		got := WithSuffix(base)

		require.Regexp(t, slugPattern, got)
		assert.True(t, strings.HasPrefix(got, base+"-"))
		assert.Len(t, got, len(base)+7)
		seen[got] = struct{}{}
	}

	// Six characters of base32 is 30 bits; 200 draws colliding more than a
	// handful of times would mean the suffix is not actually random.
	assert.Greater(t, len(seen), 195)
}
