package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestServer builds the real Server with the real dependency graph.
//
// pgxpool.NewWithConfig does not dial when MinConns is zero, so the whole
// application can be assembled without a database. Nothing in this file reaches
// a repository; these tests are about wiring and lifecycle, not queries.
func newTestServer(t *testing.T, addr string, rateLimited bool) *Server {
	t.Helper()

	t.Setenv("DATABASE_URL", "postgres://blog:blog@127.0.0.1:5432/blog?sslmode=disable")
	t.Setenv("JWT_SECRET", strings.Repeat("k", 48))
	t.Setenv("HTTP_ADDR", addr)
	t.Setenv("LOG_LEVEL", "error")
	if rateLimited {
		t.Setenv("RATE_LIMIT_ENABLED", "true")
	} else {
		t.Setenv("RATE_LIMIT_ENABLED", "false")
	}

	cfg, err := config.Load()
	require.NoError(t, err)

	poolCfg, err := pgxpool.ParseConfig(cfg.DB.URL)
	require.NoError(t, err)
	poolCfg.MinConns = 0

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	return New(cfg, logging.New(&bytes.Buffer{}, "error", "json"), pool, "test")
}

func get(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	s.Router().ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// TestNewAssemblesAServerThatServesItsRoutes is the smoke test for the whole
// wiring function: if any dependency were missing the constructor would not
// return, and if the router were not attached the index would not answer.
func TestNewAssemblesAServerThatServesItsRoutes(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:0", false)
	t.Cleanup(s.Close)

	w := get(t, s, http.MethodGet, "/")

	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "majoo-blog-api", body.Data["service"])
	assert.Equal(t, "test", body.Data["version"])
	assert.Equal(t, APIBasePath, body.Data["api_base_path"])
}

func TestRouterServesTheEmbeddedOpenAPIDocument(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:0", false)
	t.Cleanup(s.Close)

	w := get(t, s, http.MethodGet, "/openapi.yaml")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/yaml")
	assert.Contains(t, w.Body.String(), "openapi:", "the embedded spec must not be empty")
}

func TestRouterServesTheDocsPage(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:0", false)
	t.Cleanup(s.Close)

	w := get(t, s, http.MethodGet, "/docs")

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, w.Body.String(), "/openapi.yaml", "the page must point at the spec it renders")
}

// TestRouterUsesTheErrorEnvelopeForUnroutableRequests matters because the
// default net/http responses are plain text. A client that parses the API's
// error shape must not have to special-case 404 and 405.
func TestRouterUsesTheErrorEnvelopeForUnroutableRequests(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:0", false)
	t.Cleanup(s.Close)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		status int
	}{
		{"unknown endpoint", http.MethodGet, "/no/such/thing", http.StatusNotFound},
		{"wrong method on a real route", http.MethodDelete, "/healthz", http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := get(t, s, tc.method, tc.path)

			require.Equal(t, tc.status, w.Code)

			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
				RequestID string `json:"request_id"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			assert.NotEmpty(t, body.Error.Code)
			assert.NotEmpty(t, body.Error.Message)
			assert.NotEmpty(t, body.RequestID, "every response carries a correlation ID")
		})
	}
}

// TestNewInstallsTheRateLimitersWhenEnabled covers the conditional wiring: with
// rate limiting on, two limiters exist and must be closed by the lifecycle.
func TestNewInstallsTheRateLimitersWhenEnabled(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:0", true)

	require.NotNil(t, s.limiter, "the global limiter should be wired")
	require.NotNil(t, s.authLim, "the credential limiter should be wired")

	// Close must stop both janitor goroutines. Under -race a double close or a
	// leaked goroutine writing after the test would be reported.
	s.Close()
}

func TestNewOmitsTheRateLimitersWhenDisabled(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:0", false)
	t.Cleanup(s.Close)

	assert.Nil(t, s.limiter)
	assert.Nil(t, s.authLim)
}

// TestRunShutsDownGracefullyOnContextCancellation is the direct test of a
// requirement the brief names explicitly. Run must return cleanly — not with
// ErrServerClosed, which is the expected result of a deliberate shutdown.
func TestRunShutsDownGracefullyOnContextCancellation(t *testing.T) {
	addr := freeAddr(t)
	s := newTestServer(t, addr, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait until the listener actually accepts a connection, so this test
	// exercises the shutdown path rather than racing start-up.
	waitUntilServing(t, addr)
	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err, "a cancelled context is a clean shutdown, not a failure")
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// The listener must be gone once Run has returned.
	_, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	assert.Error(t, err, "the port must be released after shutdown")
}

// TestRunReturnsTheListenerError covers the other branch: the server failed
// before any shutdown signal. Background goroutines must still be released, or
// a failed start would leak the event bus workers.
func TestRunReturnsTheListenerError(t *testing.T) {
	// Occupy a port so ListenAndServe cannot bind it.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = occupied.Close() })

	s := newTestServer(t, occupied.Addr().String(), false)

	runErr := s.Run(context.Background())

	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "http server")
}

// TestCloseReleasesBackgroundWorkWithoutServing covers the path a test or a
// failed start-up takes: a Server was built but never Run, and its event bus
// workers must still stop.
func TestCloseReleasesBackgroundWorkWithoutServing(t *testing.T) {
	s := newTestServer(t, "127.0.0.1:0", true)

	assert.NotPanics(t, s.Close)
}

func TestDeadlineOrPrefersTheContextsDeadline(t *testing.T) {
	t.Parallel()

	fallback := time.Now().Add(time.Hour)

	assert.Equal(t, fallback, deadlineOr(context.Background(), fallback),
		"a context with no deadline must yield the fallback")

	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	assert.WithinDuration(t, deadline, deadlineOr(ctx, fallback), time.Millisecond,
		"a context with a deadline must yield it, not the fallback")
}

// TestAuditLogHandlerRecordsEveryEventField guards the audit trail. A dropped
// actor or subject would make the log useless for the thing it exists to answer:
// who did what, to what, and under which request.
func TestAuditLogHandlerRecordsEveryEventField(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	handler := auditLogHandler(logging.New(&buf, "info", "json"))

	handler(context.Background(), events.Event{
		Name:       "post.published",
		ActorID:    "actor-1",
		SubjectID:  "post-9",
		RequestID:  "req-7",
		OccurredAt: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
		Attributes: map[string]any{"slug": "hello-world"},
	})

	line := buf.String()
	require.NotEmpty(t, line, "the handler must write something")

	var entry map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &entry))

	assert.Equal(t, "audit", entry["log_type"], "audit lines must be filterable by type")

	audit, ok := entry["audit"].(map[string]any)
	require.True(t, ok, "the event fields are grouped under \"audit\"")
	assert.Equal(t, "post.published", audit["event"])
	assert.Equal(t, "actor-1", audit["actor_id"])
	assert.Equal(t, "post-9", audit["subject_id"])
	assert.Equal(t, "req-7", audit["request_id"])
	assert.Equal(t, "hello-world", audit["slug"], "event attributes are carried through")
}

// TestAuditLogHandlerIsSafeForConcurrentEvents: the bus calls handlers from
// several workers at once, so this runs under -race in CI.
func TestAuditLogHandlerIsSafeForConcurrentEvents(t *testing.T) {
	t.Parallel()

	var buf lockedBuffer
	handler := auditLogHandler(logging.New(&buf, "info", "json"))

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			handler(context.Background(), events.Event{Name: "post.created", ActorID: "a"})
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}

	assert.Equal(t, 8, strings.Count(buf.String(), "domain event"))
}

// lockedBuffer is a bytes.Buffer safe for concurrent writers. slog serialises
// its own output, but the buffer underneath it does not, and the race detector
// would flag the test harness rather than the code under test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// freeAddr reserves a port by binding and immediately releasing it. There is a
// theoretical race with another process claiming it in between, which is
// tolerable in a test and is the standard way to get a usable fixed address.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())
	return addr
}

// waitUntilServing blocks until addr accepts a TCP connection.
func waitUntilServing(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing was listening on %s in time", addr)
}
