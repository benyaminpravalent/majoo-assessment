package post

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// maxSlugLen leaves room under the 220-character bound in the
// posts_slug_format CHECK for the disambiguating suffix appended on collision.
const maxSlugLen = 200

// Slugify converts a title into a URL-safe slug matching the
// posts_slug_format CHECK: lower-case alphanumeric groups joined by single
// hyphens.
//
// Non-ASCII letters and digits are kept when Go classifies them as letters or
// digits — but the CHECK only accepts [a-z0-9], so they are dropped instead of
// being transliterated. Transliteration is a large problem (there is no correct
// language-independent answer for "Ø" or "北京"), and getting it half right is
// worse than a stable fallback: a title that slugifies to nothing gets a random
// slug, which is ugly but never wrong. This is called out in the README's
// known-limitations section.
func Slugify(title string) string {
	var b strings.Builder
	b.Grow(len(title))

	lastWasHyphen := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastWasHyphen = false
		default:
			if !lastWasHyphen {
				b.WriteByte('-')
				lastWasHyphen = true
			}
		}
	}

	slug := strings.Trim(b.String(), "-")
	if len(slug) > maxSlugLen {
		slug = strings.Trim(slug[:maxSlugLen], "-")
	}
	// The CHECK requires at least three characters.
	if len(slug) < 3 {
		return "post-" + randomSuffix()
	}
	return slug
}

// WithSuffix appends a short random discriminator to a slug, used when the base
// slug is already taken.
func WithSuffix(slug string) string {
	return slug + "-" + randomSuffix()
}

// suffixEncoding is lower-case base32 without padding, so the result matches
// the [a-z0-9] character class the slug CHECK enforces.
var suffixEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// randomSuffix returns six random slug-safe characters.
//
// A random suffix is used rather than a "-2", "-3" counter on purpose: a
// counter needs a read-then-write to discover the next free number, which is
// racy under concurrency and costs an extra query. Thirty bits of randomness
// makes a second collision vanishingly unlikely, and the caller retries anyway.
func randomSuffix() string {
	buf := make([]byte, 4)
	// As of Go 1.24 crypto/rand.Read is documented never to return an error: it
	// terminates the program if the operating system's entropy source fails. So
	// there is no error branch worth writing here.
	_, _ = rand.Read(buf)
	return suffixEncoding.EncodeToString(buf)[:6]
}
