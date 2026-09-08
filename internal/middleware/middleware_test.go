package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
}

func TestRequestIDGeneratesOneWhenAbsent(t *testing.T) {
	t.Parallel()

	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestID(r.Context())
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.NotEmpty(t, seen)
	_, err := uuid.Parse(seen)
	assert.NoError(t, err, "a generated ID should be a UUID")
	assert.Equal(t, seen, w.Header().Get(RequestIDHeader), "the ID must be echoed to the client")
}

func TestRequestIDHonoursSafeClientSuppliedID(t *testing.T) {
	t.Parallel()

	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestID(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "trace-abc_123.4")
	h.ServeHTTP(httptest.NewRecorder(), req)

	assert.Equal(t, "trace-abc_123.4", seen, "a well-formed upstream trace ID must be preserved")
}

// TestRequestIDRejectsUnsafeClientInput: the ID is echoed into every log line
// for the request, so newline injection and unbounded length are both real
// log-forging vectors.
func TestRequestIDRejectsUnsafeClientInput(t *testing.T) {
	t.Parallel()

	for name, supplied := range map[string]string{
		"newline injection": "abc\nlevel=error msg=\"forged\"",
		"too long":          strings.Repeat("a", maxInboundRequestIDLen+1),
		"spaces":            "has spaces",
		"quotes":            `"quoted"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var seen string
			h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen = httpx.RequestID(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(RequestIDHeader, supplied)
			h.ServeHTTP(httptest.NewRecorder(), req)

			assert.NotEqual(t, supplied, seen)
			_, err := uuid.Parse(seen)
			assert.NoError(t, err, "an unsafe ID must be replaced by a generated one")
		})
	}
}

func TestLoggerWritesOneStructuredLinePerRequest(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	h := RequestID(Logger(logger)(okHandler()))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/posts?page=2", nil))

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 1, "exactly one access-log line per request")

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
	assert.Equal(t, "GET", entry["method"])
	assert.Equal(t, "/posts", entry["path"])
	assert.Equal(t, "page=2", entry["query"])
	assert.Equal(t, float64(200), entry["status"])
	assert.NotEmpty(t, entry["request_id"])
	assert.Contains(t, entry, "duration")
}

func TestLoggerEscalatesLevelWithStatus(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		status    int
		wantLevel string
	}{
		"success":      {http.StatusOK, "INFO"},
		"client error": {http.StatusNotFound, "WARN"},
		"server error": {http.StatusInternalServerError, "ERROR"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			h := Logger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))

			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

			var entry map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
			assert.Equal(t, tc.wantLevel, entry["level"])
		})
	}
}

func TestLoggerRecordsAuthenticatedUser(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	userID := uuid.New()

	h := Logger(slog.New(slog.NewJSONHandler(&buf, nil)))(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(httpx.WithActor(req.Context(), domain.Actor{UserID: userID, Role: domain.RoleUser}))
	h.ServeHTTP(httptest.NewRecorder(), req)

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, userID.String(), entry["user_id"])
}

func TestRecovererTurnsPanicIntoJSON500(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	h := RequestID(Logger(slog.New(slog.NewJSONHandler(&buf, nil)))(
		Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("something went badly wrong with secret data 12345")
		}))))

	w := httptest.NewRecorder()
	assert.NotPanics(t, func() {
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	})

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "an internal error occurred")
	// The panic value is diagnostic detail, not something to hand a client.
	assert.NotContains(t, w.Body.String(), "secret data 12345")
	// It must, however, reach the log.
	assert.Contains(t, buf.String(), "secret data 12345")
}

func TestBodyLimitRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	var readErr error
	h := BodyLimit(16)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 100)))
	h.ServeHTTP(httptest.NewRecorder(), req)

	require.Error(t, readErr)
	var maxErr *http.MaxBytesError
	assert.ErrorAs(t, readErr, &maxErr)
}

func TestBodyLimitAllowsBodyAtTheLimit(t *testing.T) {
	t.Parallel()

	var body []byte
	var readErr error
	h := BodyLimit(16)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, readErr = io.ReadAll(r.Body)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 16)))
	h.ServeHTTP(httptest.NewRecorder(), req)

	require.NoError(t, readErr)
	assert.Len(t, body, 16)
}

func TestTimeoutGivesTheHandlerADeadline(t *testing.T) {
	t.Parallel()

	var (
		hasDeadline bool
		remaining   time.Duration
	)
	h := Timeout(50 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Deadline()
		hasDeadline = ok
		remaining = time.Until(deadline)
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.True(t, hasDeadline)
	assert.Positive(t, remaining)
	assert.LessOrEqual(t, remaining, 50*time.Millisecond)
}

func TestTimeoutCancelsAHandlerThatOverruns(t *testing.T) {
	t.Parallel()

	var ctxErr error
	h := Timeout(10 * time.Millisecond)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		ctxErr = r.Context().Err()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.ErrorIs(t, ctxErr, context.DeadlineExceeded)
}

func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	SecurityHeaders(okHandler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
	assert.Empty(t, w.Header().Get("Cache-Control"), "anonymous responses may be cached")
}

// TestSecurityHeadersMarksAuthenticatedResponsesUncacheable: a per-user response
// stored by a shared cache is a cross-user disclosure.
func TestSecurityHeadersMarksAuthenticatedResponsesUncacheable(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer something")
	w := httptest.NewRecorder()

	SecurityHeaders(okHandler()).ServeHTTP(w, req)

	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
}

func TestClientIP(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		remoteAddr string
		headers    map[string]string
		trustProxy bool
		want       string
	}{
		"remote address when proxies are untrusted": {
			remoteAddr: "203.0.113.9:54321",
			headers:    map[string]string{"X-Forwarded-For": "10.1.2.3"},
			trustProxy: false,
			want:       "203.0.113.9",
		},
		"forwarded-for when proxies are trusted": {
			remoteAddr: "10.0.0.1:443",
			headers:    map[string]string{"X-Forwarded-For": "203.0.113.9, 10.0.0.1"},
			trustProxy: true,
			want:       "203.0.113.9",
		},
		"real-ip fallback when trusted": {
			remoteAddr: "10.0.0.1:443",
			headers:    map[string]string{"X-Real-Ip": "198.51.100.7"},
			trustProxy: true,
			want:       "198.51.100.7",
		},
		"remote address when trusted but no headers": {
			remoteAddr: "192.0.2.5:1234",
			trustProxy: true,
			want:       "192.0.2.5",
		},
		"address without a port": {
			remoteAddr: "192.0.2.5",
			want:       "192.0.2.5",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			assert.Equal(t, tc.want, ClientIP(req, tc.trustProxy))
		})
	}
}
