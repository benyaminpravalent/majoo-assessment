// Package tests holds checks that span more than one internal package.
//
// The contract test below is the one that earns its keep: it walks the routes
// the router actually registers and compares them with api/openapi.yaml, in
// both directions. Hand-written API documentation drifts from the
// implementation the first time someone adds an endpoint in a hurry; this makes
// that drift a failing test rather than a surprise for whoever reads the docs.
package tests

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
	"github.com/bpsiregar/majoo-assessment/internal/server"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// openAPIDocument is the subset of the specification this test needs.
type openAPIDocument struct {
	OpenAPI string `yaml:"openapi"`
	Info    struct {
		Title   string `yaml:"title"`
		Version string `yaml:"version"`
	} `yaml:"info"`
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas         map[string]any `yaml:"schemas"`
		Responses       map[string]any `yaml:"responses"`
		Parameters      map[string]any `yaml:"parameters"`
		SecuritySchemes map[string]any `yaml:"securitySchemes"`
	} `yaml:"components"`
}

// httpMethods are the keys in a path item that describe an operation. Anything
// else at that level (`parameters`, `summary`) is not an operation.
var httpMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

func loadSpec(t *testing.T) openAPIDocument {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "api", "openapi.yaml"))
	require.NoError(t, err, "api/openapi.yaml must exist")

	var doc openAPIDocument
	// KnownFields is not set: this struct is a deliberate subset of the spec.
	require.NoError(t, yaml.Unmarshal(raw, &doc), "api/openapi.yaml must be valid YAML")
	return doc
}

// buildRouter constructs the real application router.
//
// pgxpool.NewWithConfig does not dial when MinConns is zero, so the whole
// dependency graph can be wired without a database. That is enough for a
// routing check: no request in this file reaches a repository.
func buildRouter(t *testing.T) chi.Router {
	t.Helper()

	t.Setenv("DATABASE_URL", "postgres://blog:blog@127.0.0.1:5432/blog?sslmode=disable")
	t.Setenv("JWT_SECRET", strings.Repeat("k", 48))
	t.Setenv("RATE_LIMIT_ENABLED", "false")
	t.Setenv("LOG_LEVEL", "error")

	cfg, err := config.Load()
	require.NoError(t, err)

	poolCfg, err := pgxpool.ParseConfig(cfg.DB.URL)
	require.NoError(t, err)
	poolCfg.MinConns = 0

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	srv := server.New(cfg, logging.New(os.Stderr, "error", "json"), pool, "test")
	t.Cleanup(srv.Close)

	return srv.Router()
}

// routeKey identifies one operation as "METHOD path".
type routeKey struct {
	method string
	path   string
}

func (r routeKey) String() string { return r.method + " " + r.path }

// registeredRoutes enumerates the operations the router serves.
func registeredRoutes(t *testing.T, r chi.Router) map[routeKey]bool {
	t.Helper()

	out := map[routeKey]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi reports the root as "/*" and appends a trailing slash to mounted
		// subtrees; normalise both so the comparison is like for like.
		route = strings.TrimSuffix(route, "/*")
		if route == "" {
			route = "/"
		}
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		out[routeKey{method: strings.ToUpper(method), path: route}] = true
		return nil
	})
	require.NoError(t, err)
	return out
}

// documentedRoutes enumerates the operations the specification describes.
func documentedRoutes(t *testing.T, doc openAPIDocument) map[routeKey]bool {
	t.Helper()

	out := map[routeKey]bool{}
	for path, item := range doc.Paths {
		for key := range item {
			if httpMethods[strings.ToLower(key)] {
				out[routeKey{method: strings.ToUpper(key), path: path}] = true
			}
		}
	}
	return out
}

func TestSpecificationIsWellFormed(t *testing.T) {
	doc := loadSpec(t)

	assert.True(t, strings.HasPrefix(doc.OpenAPI, "3."), "must declare OpenAPI 3.x, got %q", doc.OpenAPI)
	assert.NotEmpty(t, doc.Info.Title)
	assert.NotEmpty(t, doc.Info.Version)
	assert.NotEmpty(t, doc.Paths)
	assert.Contains(t, doc.Components.SecuritySchemes, "bearerAuth")
}

// TestEveryRouteIsDocumented fails when an endpoint is added to the router but
// not to the specification.
func TestEveryRouteIsDocumented(t *testing.T) {
	doc := loadSpec(t)
	documented := documentedRoutes(t, doc)

	var missing []string
	for route := range registeredRoutes(t, buildRouter(t)) {
		if !documented[route] {
			missing = append(missing, route.String())
		}
	}
	sort.Strings(missing)

	assert.Empty(t, missing,
		"these routes are served but absent from api/openapi.yaml:\n  %s",
		strings.Join(missing, "\n  "))
}

// TestEveryDocumentedOperationExists fails the other way: when the
// specification promises an endpoint the service does not serve.
func TestEveryDocumentedOperationExists(t *testing.T) {
	doc := loadSpec(t)
	registered := registeredRoutes(t, buildRouter(t))

	var phantom []string
	for route := range documentedRoutes(t, doc) {
		if !registered[route] {
			phantom = append(phantom, route.String())
		}
	}
	sort.Strings(phantom)

	assert.Empty(t, phantom,
		"api/openapi.yaml documents endpoints the router does not serve:\n  %s",
		strings.Join(phantom, "\n  "))
}

// TestEveryOperationHasAnIDAndSummary keeps the generated clients and the
// rendered reference usable.
func TestEveryOperationHasAnIDAndSummary(t *testing.T) {
	doc := loadSpec(t)

	seenIDs := map[string]string{}
	for path, item := range doc.Paths {
		for method, raw := range item {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			op, ok := raw.(map[string]any)
			require.True(t, ok, "%s %s: operation must be a mapping", method, path)

			id, _ := op["operationId"].(string)
			summary, _ := op["summary"].(string)
			where := fmt.Sprintf("%s %s", strings.ToUpper(method), path)

			assert.NotEmpty(t, id, "%s: operationId is required", where)
			assert.NotEmpty(t, summary, "%s: summary is required", where)
			assert.NotEmpty(t, op["responses"], "%s: at least one response is required", where)

			if previous, duplicate := seenIDs[id]; duplicate {
				t.Errorf("operationId %q is used by both %s and %s", id, previous, where)
			}
			seenIDs[id] = where
		}
	}
}

// TestDocumentedRequestBodiesAreDescribed catches a half-written operation.
func TestDocumentedRequestBodiesAreDescribed(t *testing.T) {
	doc := loadSpec(t)

	for path, item := range doc.Paths {
		for method, raw := range item {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			op := raw.(map[string]any)
			body, present := op["requestBody"]
			if !present {
				continue
			}

			where := fmt.Sprintf("%s %s", strings.ToUpper(method), path)
			content, ok := body.(map[string]any)["content"].(map[string]any)
			require.True(t, ok, "%s: requestBody needs a content block", where)
			assert.Contains(t, content, "application/json",
				"%s: this API only accepts JSON bodies", where)
		}
	}
}

// TestWriteOperationsDocumentTheirFailureModes: an endpoint that can be
// rejected must say so, or a client author has to discover it in production.
func TestWriteOperationsDocumentTheirFailureModes(t *testing.T) {
	doc := loadSpec(t)

	for path, item := range doc.Paths {
		if !strings.HasPrefix(path, "/api/v1/") {
			continue
		}
		for method, raw := range item {
			lower := strings.ToLower(method)
			if lower != "post" && lower != "patch" && lower != "delete" {
				continue
			}

			op := raw.(map[string]any)
			responses, ok := op["responses"].(map[string]any)
			require.True(t, ok)
			where := fmt.Sprintf("%s %s", strings.ToUpper(method), path)

			assert.Contains(t, responses, "500", "%s: must document the server-error response", where)

			// Endpoints that take a body must document how a bad body is reported.
			if _, hasBody := op["requestBody"]; hasBody {
				assert.Contains(t, responses, "422", "%s: must document validation failures", where)
			}
		}
	}
}

// TestRouterServesTheDocumentedMetaEndpoints exercises the three endpoints that
// need no database, end to end through the real middleware stack.
func TestRouterServesTheDocumentedMetaEndpoints(t *testing.T) {
	r := buildRouter(t)

	for _, tc := range []struct {
		path        string
		wantStatus  int
		wantInBody  string
		contentType string
	}{
		{path: "/", wantStatus: http.StatusOK, wantInBody: "majoo-blog-api", contentType: "application/json"},
		{path: "/healthz", wantStatus: http.StatusOK, wantInBody: `"status":"ok"`, contentType: "application/json"},
		{path: "/openapi.yaml", wantStatus: http.StatusOK, wantInBody: "openapi: 3.", contentType: "application/yaml"},
		{path: "/docs", wantStatus: http.StatusOK, wantInBody: "swagger-ui", contentType: "text/html"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := doRequest(t, r, http.MethodGet, tc.path)

			assert.Equal(t, tc.wantStatus, w.Code)
			assert.Contains(t, w.Header().Get("Content-Type"), tc.contentType)
			assert.Contains(t, w.Body.String(), tc.wantInBody)
			assert.NotEmpty(t, w.Header().Get("X-Request-Id"),
				"every response must carry a correlation ID")
		})
	}
}

// TestUnknownRoutesUseTheErrorEnvelope: a 404 from the router must look like a
// 404 from a handler, or client error handling has to special-case it.
func TestUnknownRoutesUseTheErrorEnvelope(t *testing.T) {
	r := buildRouter(t)

	w := doRequest(t, r, http.MethodGet, "/api/v1/no-such-endpoint")

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	assert.Contains(t, w.Body.String(), `"code":"not_found"`)
}

func TestWrongMethodReturns405WithTheErrorEnvelope(t *testing.T) {
	r := buildRouter(t)

	w := doRequest(t, r, http.MethodPut, "/api/v1/posts")

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"bad_request"`)
}

func TestProtectedRoutesRejectAnonymousCallers(t *testing.T) {
	r := buildRouter(t)

	for _, path := range []string{"/api/v1/users/me", "/api/v1/posts"} {
		method := http.MethodGet
		if path == "/api/v1/posts" {
			method = http.MethodPost
		}

		w := doRequest(t, r, method, path)

		assert.Equal(t, http.StatusUnauthorized, w.Code, "%s %s", method, path)
		assert.Contains(t, w.Header().Get("WWW-Authenticate"), "Bearer")
	}
}

func TestSecurityHeadersArePresentOnEveryResponse(t *testing.T) {
	r := buildRouter(t)

	w := doRequest(t, r, http.MethodGet, "/")

	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	assert.Equal(t, "no-referrer", w.Header().Get("Referrer-Policy"))
}

// TestReadinessFailsWithoutADatabase confirms the probe is actually wired to
// the pool: there is no server listening on the configured address here, so it
// must report degraded rather than optimistically returning 200.
func TestReadinessFailsWithoutADatabase(t *testing.T) {
	r := buildRouter(t)

	start := time.Now()
	w := doRequest(t, r, http.MethodGet, "/readyz")

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"status":"degraded"`)
	assert.Less(t, time.Since(start), 10*time.Second, "the probe must be bounded")
}
