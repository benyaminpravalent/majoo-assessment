// Package server assembles the application: it wires repositories, services and
// handlers into a router, and owns the process lifecycle.
//
// Wiring lives in one file on purpose. Constructor injection by hand is more
// code than a container, but the dependency graph is readable top to bottom and
// a missing dependency is a compile error rather than a nil pointer at runtime.
package server

import (
	"log/slog"
	"net/http"

	"github.com/bpsiregar/majoo-assessment/internal/comment"
	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/health"
	"github.com/bpsiregar/majoo-assessment/internal/middleware"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/post"
	"github.com/bpsiregar/majoo-assessment/internal/user"
	"github.com/go-chi/chi/v5"
)

// APIBasePath is the version prefix for every business endpoint.
//
// The version is in the path rather than in a header because it is the option a
// reviewer can exercise with curl and a browser, and because CDNs and access
// logs treat it as a distinct resource without extra configuration.
const APIBasePath = "/api/v1"

// routerDeps is everything NewRouter needs. It is unexported because only
// New builds one; exporting it would invite callers to assemble a partial
// router with a nil handler.
type routerDeps struct {
	cfg           config.Config
	logger        *slog.Logger
	auth          *middleware.Authenticator
	globalLimiter *middleware.RateLimiter
	authLimiter   *middleware.RateLimiter

	users    *user.Handler
	posts    *post.Handler
	comments *comment.Handler
	health   *health.Handler
	version  string
}

// newRouter builds the HTTP routing tree.
//
// Middleware order is the part of this function worth reading carefully, and it
// is the reason the stack is written out here rather than spread across the
// feature packages:
//
//  1. RequestID first, so every later component — including the panic handler —
//     has a correlation ID to log.
//  2. Logger second, so it observes and reports everything below it, including
//     rejections by the rate limiter.
//  3. Recoverer third: it must be inside the logger (so a panic is logged with
//     request context) but outside everything that could panic.
//  4. SecurityHeaders and CORS before any handler can write a response.
//  5. RateLimit before the timeout and body limit, so shed load costs as little
//     work as possible.
//  6. Timeout then BodyLimit last, closest to the handler that they constrain.
func newRouter(d routerDeps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.Logger(d.logger))
	r.Use(middleware.Recoverer)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.CORS(middleware.CORSConfig{
		AllowedOrigins: d.cfg.CORS.AllowedOrigins,
		// Bearer tokens, not cookies: browsers never need to send credentials.
		AllowCredentials: false,
	}))
	if d.globalLimiter != nil {
		r.Use(d.globalLimiter.Middleware(d.cfg.HTTP.TrustProxyHeaders))
	}
	r.Use(middleware.Timeout(d.cfg.HTTP.HandlerTimeout))
	r.Use(middleware.BodyLimit(d.cfg.HTTP.MaxBodyBytes))

	// Unmatched routes must still answer with the API's error envelope, not with
	// net/http's plain-text default, so a client's error handling works uniformly.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteError(req.Context(), w, apierr.NotFound("endpoint"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteError(req.Context(), w,
			apierr.New(apierr.CodeBadRequest, http.StatusMethodNotAllowed,
				"this method is not allowed on this endpoint"))
	})

	// Infrastructure endpoints, unversioned. See package health for why liveness
	// and readiness differ.
	d.health.RegisterRoutes(r)

	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		httpx.WriteData(req.Context(), w, http.StatusOK, map[string]string{
			"service":       "majoo-blog-api",
			"version":       d.version,
			"api_base_path": APIBasePath,
			"openapi":       "/openapi.yaml",
			"docs":          "/docs",
			"health":        "/healthz",
			"readiness":     "/readyz",
		})
	})

	registerDocsRoutes(r)

	r.Route(APIBasePath, func(v1 chi.Router) {
		// Endpoints that work anonymously but behave differently for a signed-in
		// caller: an author must see their own drafts in a listing.
		v1.Group(func(pub chi.Router) {
			pub.Use(d.auth.Optional)
			d.users.RegisterPublicRoutes(pub)
			d.posts.RegisterPublicRoutes(pub)
			d.comments.RegisterPublicRoutes(pub)
		})

		// Credential endpoints get their own, much stricter limiter. Login,
		// registration and refresh are where brute force and enumeration happen,
		// and the global limit is far too generous to slow either down.
		v1.Group(func(credentials chi.Router) {
			if d.authLimiter != nil {
				credentials.Use(d.authLimiter.Middleware(d.cfg.HTTP.TrustProxyHeaders))
			}
			d.users.RegisterCredentialRoutes(credentials)
		})

		v1.Group(func(protected chi.Router) {
			protected.Use(d.auth.Require)
			d.users.RegisterProtectedRoutes(protected)
			d.posts.RegisterProtectedRoutes(protected)
			d.comments.RegisterProtectedRoutes(protected)
		})
	})

	return r
}
