package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubPinger struct {
	err    error
	called int
	block  time.Duration
}

func (s *stubPinger) Ping(ctx context.Context) error {
	s.called++
	if s.block > 0 {
		select {
		case <-time.After(s.block):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

func serve(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	h.RegisterRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// TestLivenessDoesNotTouchTheDatabase is the point of having two endpoints. If
// /healthz checked the database, a brief outage would fail liveness on every
// replica and get them all killed, turning a recoverable blip into an outage.
func TestLivenessDoesNotTouchTheDatabase(t *testing.T) {
	t.Parallel()

	db := &stubPinger{err: errors.New("database is down")}

	w := serve(t, NewHandler(db, "v1.2.3"), "/healthz")

	assert.Equal(t, http.StatusOK, w.Code, "liveness must succeed even with a dead database")
	assert.Zero(t, db.called, "liveness must not probe any dependency")

	var body healthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
	assert.Equal(t, "v1.2.3", body.Version)
}

func TestReadinessSucceedsWhenTheDatabaseAnswers(t *testing.T) {
	t.Parallel()

	db := &stubPinger{}

	w := serve(t, NewHandler(db, "v1"), "/readyz")

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, db.called)

	var body healthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
	require.Contains(t, body.Checks, "database")
	assert.Equal(t, "ok", body.Checks["database"].Status)
}

// TestReadinessFailsWhenTheDatabaseIsUnreachable: a replica that cannot serve
// must leave the load-balancer rotation, without dying.
func TestReadinessFailsWhenTheDatabaseIsUnreachable(t *testing.T) {
	t.Parallel()

	db := &stubPinger{err: errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")}

	w := serve(t, NewHandler(db, "v1"), "/readyz")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)

	var body healthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "degraded", body.Status)
	require.Contains(t, body.Checks, "database")
	assert.Equal(t, "error", body.Checks["database"].Status)
	// Operators need the reason; these endpoints are not public.
	assert.Contains(t, body.Checks["database"].Error, "connection refused")
}

// TestReadinessBoundsASlowDatabase stops a hung dependency from holding the
// probe open until the orchestrator's own timeout fires.
func TestReadinessBoundsASlowDatabase(t *testing.T) {
	t.Parallel()

	db := &stubPinger{block: 10 * time.Second}
	h := NewHandler(db, "v1")

	start := time.Now()
	w := serve(t, h, "/readyz")
	elapsed := time.Since(start)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Less(t, elapsed, 5*time.Second, "the probe must give up after probeTimeout")
	assert.GreaterOrEqual(t, elapsed, probeTimeout)
}

func TestUptimeIsReported(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	h := NewHandler(&stubPinger{}, "v1")
	h.started = now
	h.now = func() time.Time { return now.Add(90 * time.Second) }

	r := chi.NewRouter()
	h.RegisterRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body healthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, int64(90), body.UptimeSeconds)
}
