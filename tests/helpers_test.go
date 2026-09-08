package tests

import (
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

// doRequest sends a request through the full middleware stack and returns the
// recorded response.
func doRequest(t *testing.T, r chi.Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, path, nil)
	// A stable, routable-looking address so the rate limiter and access log have
	// something sensible to key on.
	req.RemoteAddr = "203.0.113.1:12345"

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
