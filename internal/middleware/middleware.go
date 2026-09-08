// Package middleware holds the cross-cutting HTTP concerns: correlation IDs,
// access logging, panic recovery, body limits, deadlines, CORS, security
// headers, authentication and rate limiting.
//
// Order matters, and the intended order is documented once in
// internal/server/router.go rather than being implied by import order here.
package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
	"github.com/google/uuid"
)

// RequestIDHeader is the header used to accept and return a correlation ID.
const RequestIDHeader = "X-Request-Id"

// maxInboundRequestIDLen bounds a client-supplied ID. It is echoed into every
// log line for the request, so an unbounded value is a log-inflation vector.
const maxInboundRequestIDLen = 64

// RequestID attaches a correlation ID to the request context and echoes it in
// the response.
//
// A client-supplied X-Request-Id is honoured so a trace can span the gateway
// and this service — but only after being length-checked and stripped of
// characters that would let it forge structure inside a log line.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := sanitizeRequestID(r.Header.Get(RequestIDHeader))
		if id == "" {
			id = uuid.NewString()
		}

		ctx := httpx.WithRequestID(r.Context(), id)
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func sanitizeRequestID(raw string) string {
	if raw == "" || len(raw) > maxInboundRequestIDLen {
		return ""
	}
	for _, r := range raw {
		isSafe := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !isSafe {
			return ""
		}
	}
	return raw
}

// Logger attaches a request-scoped logger and writes one structured line per
// completed request.
//
// One line per request, not one on entry and one on exit: the entry line
// carries no information the exit line lacks, and doubling log volume for that
// is a poor trade at any real traffic level.
func Logger(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			reqLogger := base.With(
				slog.String("request_id", httpx.RequestID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			ctx := logging.WithLogger(r.Context(), reqLogger)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			attrs := []any{
				slog.Int("status", rec.status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
				slog.String("client_ip", ClientIP(r, false)),
				// The query string is logged, the request body never is: bodies
				// carry passwords and tokens.
				slog.String("query", r.URL.RawQuery),
			}
			if actor, ok := httpx.Actor(ctx); ok {
				attrs = append(attrs, slog.String("user_id", actor.UserID.String()))
			}

			switch {
			case rec.status >= 500:
				reqLogger.Error("request completed", attrs...)
			case rec.status >= 400:
				reqLogger.Warn("request completed", attrs...)
			default:
				reqLogger.Info("request completed", attrs...)
			}
		})
	}
}

// statusRecorder captures the status code and body size for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int64
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Recoverer converts a panic into a 500 instead of killing the connection and,
// with net/http's default behaviour, logging a stack trace to stderr with no
// request context attached.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			// A panic caused by the client disconnecting mid-write is not a bug
			// and must not be reported as one.
			if err, ok := rec.(error); ok && strings.Contains(err.Error(), "http: abort Handler") {
				panic(rec)
			}

			ctx := r.Context()
			logging.FromContext(ctx).Error("recovered from panic in handler",
				slog.Any("panic", rec),
				slog.String("stack", stackTrace()))

			httpx.WriteError(ctx, w, apierr.Internal(errFromPanic(rec)))
		}()

		next.ServeHTTP(w, r)
	})
}

// BodyLimit caps the number of bytes a handler will read from a request body.
//
// Without this, a single client can stream gigabytes into a JSON decoder. The
// limit is applied by wrapping the body rather than by checking Content-Length,
// because Content-Length can be absent (chunked encoding) or simply wrong.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout gives each request a deadline on its context.
//
// It deliberately does not use http.TimeoutHandler. That handler buffers the
// entire response in memory so it can discard it and substitute an error, which
// turns every response into an allocation proportional to its size — a real
// cost on list endpoints, paid on every request to protect against a rare one.
//
// Instead the deadline is propagated: pgx aborts the in-flight query, the
// repository returns context.DeadlineExceeded, and apierr.From maps it to 504.
// Handlers that ignore their context are backstopped by the server's
// WriteTimeout, which config.Load requires to be longer than this deadline.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// SecurityHeaders sets the response headers that cost nothing and remove whole
// classes of browser-side problems.
//
// This is a JSON API, so the set is small and deliberate: no CSP (there is no
// document to constrain) and no HSTS (TLS terminates at the ingress, which owns
// that header and can set it for the whole domain).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		// Responses are per-user whenever a token is presented; letting a shared
		// cache store them would be a cross-user disclosure.
		if r.Header.Get("Authorization") != "" {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ClientIP returns the address to attribute a request to.
//
// X-Forwarded-For is honoured only when trustProxy is true. That flag defaults
// to false because the header is client-controlled: trusting it unconditionally
// lets anyone bypass the per-IP rate limit by inventing an address. Enable it
// only when the service is genuinely behind a proxy that overwrites the header.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The left-most entry is the original client; entries appended by
			// intermediaries follow.
			if first, _, found := strings.Cut(xff, ","); found || first != "" {
				if ip := strings.TrimSpace(first); ip != "" {
					return ip
				}
			}
		}
		if xrip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xrip != "" {
			return xrip
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr has no port in some test and unix-socket setups.
		return r.RemoteAddr
	}
	return host
}
