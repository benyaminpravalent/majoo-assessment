package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func corsHandler(origins ...string) http.Handler {
	return CORS(CORSConfig{AllowedOrigins: origins})(okHandler())
}

func TestCORSIgnoresRequestsWithoutAnOrigin(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	corsHandler("https://app.example.com").
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
		"a same-origin or non-browser request needs no CORS headers")
}

func TestCORSAllowsConfiguredOrigin(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()

	corsHandler("https://app.example.com").ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Contains(t, w.Header().Values("Vary"), "Origin",
		"echoing a specific origin requires Vary, or a cache can cross-serve it")
	assert.Contains(t, w.Header().Get("Access-Control-Expose-Headers"), RequestIDHeader)
}

// TestCORSDoesNotReflectUnknownOrigins is the vulnerability this hand-written
// middleware exists to avoid: reflecting whatever Origin arrives defeats the
// entire same-origin policy.
func TestCORSDoesNotReflectUnknownOrigins(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()

	corsHandler("https://app.example.com").ServeHTTP(w, req)

	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	// The request itself still runs: a non-browser client has no CORS obligation
	// and must not be blocked by a header policy meant for browsers.
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestCORSMatchesOriginsExactly(t *testing.T) {
	t.Parallel()

	for _, origin := range []string{
		"https://app.example.com.evil.com",
		"https://app.example.com:8443",
		"http://app.example.com",
		"https://APP.example.com",
	} {
		t.Run(origin, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()

			corsHandler("https://app.example.com").ServeHTTP(w, req)

			assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"),
				"origin matching must be exact, not prefix or suffix based")
		})
	}
}

func TestCORSAnswersPreflight(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/posts", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
	w := httptest.NewRecorder()

	corsHandler("https://app.example.com").ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))

	methods := w.Header().Get("Access-Control-Allow-Methods")
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete} {
		assert.Contains(t, methods, m)
	}

	headers := strings.ToLower(w.Header().Get("Access-Control-Allow-Headers"))
	assert.Contains(t, headers, "authorization")
	assert.Contains(t, headers, "content-type")
	assert.NotEmpty(t, w.Header().Get("Access-Control-Max-Age"))
}

func TestCORSPreflightFromUnknownOriginIsNotApproved(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodOptions, "/api/v1/posts", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	w := httptest.NewRecorder()

	corsHandler("https://app.example.com").ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
	assert.Empty(t, w.Header().Get("Access-Control-Allow-Methods"))
}

func TestCORSWildcardIsSentLiterally(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://anything.example.com")
	w := httptest.NewRecorder()

	corsHandler("*").ServeHTTP(w, req)

	// Without credentials the literal wildcard is correct and cache-friendly;
	// config.Load refuses this setting in production.
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestCORSWithCredentialsEchoesOriginInsteadOfWildcard(t *testing.T) {
	t.Parallel()

	h := CORS(CORSConfig{AllowedOrigins: []string{"*"}, AllowCredentials: true})(okHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()

	h.ServeHTTP(w, req)

	// "*" with credentials is rejected outright by browsers, so the specific
	// origin must be echoed instead.
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", w.Header().Get("Access-Control-Allow-Credentials"))
}

func TestCORSWithNoConfiguredOriginsDeniesEverything(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()

	corsHandler().ServeHTTP(w, req)

	assert.Empty(t, w.Header().Get("Access-Control-Allow-Origin"))
}
