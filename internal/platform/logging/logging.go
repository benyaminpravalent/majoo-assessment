// Package logging builds the application's structured logger.
//
// Two properties matter here beyond "we emit JSON":
//
//  1. Redaction is enforced by the handler, not by discipline at call sites. Any
//     attribute whose key looks like a credential is replaced before it can be
//     written, so a future contributor logging slog.String("password", pw) still
//     cannot leak it.
//  2. The request-scoped logger carries the request ID, so every line emitted
//     while serving a request is correlatable without threading a field through
//     each function.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// redactedKeys are attribute names whose values are replaced with a placeholder.
// Matching is case-insensitive and substring-based, which intentionally errs
// toward over-redaction: "db_password" and "X-Api-Key" both match.
var redactedKeys = []string{
	"password",
	"passwd",
	"secret",
	"token",
	"authorization",
	"api_key",
	"apikey",
	"cookie",
	"set-cookie",
	"credential",
	"jwt",
}

const redactedPlaceholder = "[REDACTED]"

// New returns a logger writing to w in the requested format at the requested
// level. Unknown levels and formats fall back to info/json; configuration is
// validated up front in package config, so this is defence in depth rather than
// the primary check.
func New(w io.Writer, level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(level),
		ReplaceAttr: redact,
	}

	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// redact replaces the value of any attribute whose key looks like a credential.
// Group keys are left alone so nesting is preserved.
func redact(_ []string, a slog.Attr) slog.Attr {
	if a.Value.Kind() == slog.KindGroup {
		return a
	}
	if IsSensitiveKey(a.Key) {
		return slog.String(a.Key, redactedPlaceholder)
	}
	return a
}

// IsSensitiveKey reports whether a field name should have its value redacted.
// It is exported because the HTTP access log applies the same rule to request
// headers before recording them.
func IsSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, k := range redactedKeys {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// Redacted is the placeholder substituted for sensitive values.
func Redacted() string { return redactedPlaceholder }

// contextKey is unexported so no other package can collide with our key.
type contextKey struct{}

// WithLogger returns a context carrying the given logger.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, contextKey{}, l)
}

// FromContext returns the request-scoped logger, or slog.Default when the
// context carries none. It never returns nil, so callers can log unconditionally.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(contextKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}
