package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CORSConfig describes the browser origins allowed to call this API.
type CORSConfig struct {
	// AllowedOrigins holds exact origins ("https://app.example.com"). The single
	// wildcard "*" is accepted for local development; config.Load refuses it when
	// APP_ENV=production.
	AllowedOrigins []string
	// AllowCredentials controls whether browsers may send cookies. It is false
	// here because this API authenticates with a bearer token, and the
	// combination of credentials and a wildcard origin is forbidden by the spec
	// for good reason.
	AllowCredentials bool
	MaxAge           time.Duration
}

// DefaultCORSMaxAge caps how long a browser may cache a preflight result.
const DefaultCORSMaxAge = 10 * time.Minute

var (
	corsAllowedMethods = strings.Join([]string{
		http.MethodGet, http.MethodPost, http.MethodPatch,
		http.MethodDelete, http.MethodOptions,
	}, ", ")

	corsAllowedHeaders = strings.Join([]string{
		"Authorization", "Content-Type", RequestIDHeader,
	}, ", ")

	corsExposedHeaders = strings.Join([]string{
		RequestIDHeader, "Retry-After", "Location",
	}, ", ")
)

// CORS answers preflight requests and adds the response headers a browser needs
// to accept a cross-origin reply.
//
// This is written out rather than pulled from a library because the policy is
// twelve lines and the failure mode of getting it wrong — reflecting whatever
// Origin arrives — is a real vulnerability that a configurable library makes
// easy to reach for by accident.
func CORS(cfg CORSConfig) func(http.Handler) http.Handler {
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultCORSMaxAge
	}
	maxAgeSeconds := strconv.Itoa(int(maxAge.Seconds()))

	allowAll := len(cfg.AllowedOrigins) == 1 && cfg.AllowedOrigins[0] == "*"
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[o] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// Not a browser cross-origin request; nothing to negotiate.
				next.ServeHTTP(w, r)
				return
			}

			_, ok := allowed[origin]
			if !ok && !allowAll {
				// Deny by omission: send no CORS headers and let the browser block
				// the response. Rejecting with a 403 would be worse, because a
				// non-browser client with no CORS obligations would also be blocked.
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			h := w.Header()
			if allowAll && !cfg.AllowCredentials {
				h.Set("Access-Control-Allow-Origin", "*")
			} else {
				// Echoing the specific origin requires Vary, or a shared cache can
				// serve one origin's response to another.
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
			}
			if cfg.AllowCredentials {
				h.Set("Access-Control-Allow-Credentials", "true")
			}
			h.Set("Access-Control-Expose-Headers", corsExposedHeaders)

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Set("Access-Control-Allow-Methods", corsAllowedMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				h.Set("Access-Control-Max-Age", maxAgeSeconds)
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
