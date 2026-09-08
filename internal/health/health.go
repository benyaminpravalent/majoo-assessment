// Package health implements the liveness and readiness endpoints.
//
// The two are deliberately different, because Kubernetes uses them for opposite
// purposes and conflating them causes outages:
//
//   - /healthz (liveness) answers "is this process wedged?" It touches no
//     dependency. If it checked the database, a brief database outage would make
//     every replica fail its liveness probe and be killed and restarted — turning
//     a recoverable dependency blip into a full outage with a cold pool.
//   - /readyz (readiness) answers "can this replica serve traffic right now?" It
//     does check the database, because a replica that cannot reach it should be
//     taken out of the load-balancer rotation while staying alive.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/go-chi/chi/v5"
)

// pinger is the readiness probe's view of the database pool.
type pinger interface {
	Ping(ctx context.Context) error
}

// probeTimeout bounds the dependency check so a hung database cannot hold the
// probe open until the orchestrator's own timeout fires.
const probeTimeout = 2 * time.Second

// Handler serves the health endpoints.
type Handler struct {
	db      pinger
	version string
	started time.Time
	now     func() time.Time
}

// NewHandler returns a Handler reporting the given build version.
func NewHandler(db pinger, version string) *Handler {
	now := time.Now
	return &Handler{db: db, version: version, started: now(), now: now}
}

// RegisterRoutes mounts the probes at the root of the server, outside /api/v1.
// They are infrastructure endpoints, not part of the versioned public API, and
// versioning them would mean an orchestrator config change on every API bump.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Get("/healthz", h.live)
	r.Get("/readyz", h.ready)
}

type checkResult struct {
	Status    string `json:"status"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type healthResponse struct {
	Status        string                 `json:"status"`
	Version       string                 `json:"version"`
	UptimeSeconds int64                  `json:"uptime_seconds"`
	Checks        map[string]checkResult `json:"checks,omitempty"`
}

func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(r.Context(), w, http.StatusOK, healthResponse{
		Status:        "ok",
		Version:       h.version,
		UptimeSeconds: int64(h.now().Sub(h.started).Seconds()),
	})
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
	defer cancel()

	start := h.now()
	err := h.db.Ping(ctx)
	latency := h.now().Sub(start)

	dbCheck := checkResult{Status: "ok", LatencyMS: latency.Milliseconds()}
	status := http.StatusOK
	overall := "ok"

	if err != nil {
		// The error text goes into the response because these endpoints are for
		// operators, not end users, and "not ready" without a reason wastes the
		// first ten minutes of an incident. They are not exposed publicly; see
		// the ingress notes in the README.
		dbCheck = checkResult{Status: "error", LatencyMS: latency.Milliseconds(), Error: err.Error()}
		status = http.StatusServiceUnavailable
		overall = "degraded"
	}

	httpx.WriteJSON(ctx, w, status, healthResponse{
		Status:        overall,
		Version:       h.version,
		UptimeSeconds: int64(h.now().Sub(h.started).Seconds()),
		Checks:        map[string]checkResult{"database": dbCheck},
	})
}
