package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewEmitsStructuredJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, "info", "json")

	logger.Info("hello", slog.String("key", "value"))

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	assert.Equal(t, "hello", entry["msg"])
	assert.Equal(t, "value", entry["key"])
}

func TestNewRespectsLevel(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, "warn", "json")

	logger.Debug("suppressed")
	logger.Info("also suppressed")
	logger.Warn("emitted")

	assert.NotContains(t, buf.String(), "suppressed")
	assert.Contains(t, buf.String(), "emitted")
}

func TestNewFallsBackToInfoJSONForUnknownSettings(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, "verbose", "yaml")

	logger.Debug("suppressed")
	logger.Info("emitted")

	assert.NotContains(t, buf.String(), "suppressed")
	assert.Contains(t, buf.String(), `"msg":"emitted"`, "unknown format falls back to JSON")
}

// TestRedactionIsEnforcedByTheHandler is the property that makes redaction
// reliable: it does not depend on every call site remembering to omit a secret.
func TestRedactionIsEnforcedByTheHandler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, "info", "json")

	logger.Info("login attempt",
		slog.String("password", "hunter2"),
		slog.String("Authorization", "Bearer abc.def.ghi"),
		slog.String("refresh_token", "opaque-token-value"),
		slog.String("db_password", "s3cret"),
		slog.String("X-Api-Key", "key-123"),
		slog.String("email", "ben@example.com"),
	)

	out := buf.String()
	for _, secret := range []string{"hunter2", "abc.def.ghi", "opaque-token-value", "s3cret", "key-123"} {
		assert.NotContains(t, out, secret, "a credential-shaped field must never reach the log")
	}
	// Non-sensitive fields pass through: over-redacting everything would make
	// the logs useless.
	assert.Contains(t, out, "ben@example.com")
	assert.Equal(t, 5, strings.Count(out, Redacted()))
}

func TestRedactionMatchesCaseInsensitivelyAndOnSubstrings(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"password", "PASSWORD", "user_password", "passwd",
		"secret", "client_secret", "token", "access_token", "jwt",
		"authorization", "Cookie", "Set-Cookie", "api_key", "apiKey", "credential",
	} {
		assert.True(t, IsSensitiveKey(key), "%q should be treated as sensitive", key)
	}

	for _, key := range []string{"email", "user_id", "status", "path", "duration", "method"} {
		assert.False(t, IsSensitiveKey(key), "%q should not be redacted", key)
	}
}

func TestRedactionDoesNotFlattenGroups(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, "info", "json")

	logger.Info("audit", slog.Group("event", slog.String("name", "post.created")))

	var entry map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))
	group, ok := entry["event"].(map[string]any)
	require.True(t, ok, "group structure must be preserved")
	assert.Equal(t, "post.created", group["name"])
}

func TestContextLogger(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := New(&buf, "info", "json").With(slog.String("request_id", "req-1"))

	ctx := WithLogger(context.Background(), logger)
	FromContext(ctx).Info("scoped")

	assert.Contains(t, buf.String(), "req-1")
}

// FromContext must never return nil, so callers can log unconditionally.
func TestFromContextFallsBackToDefault(t *testing.T) {
	t.Parallel()

	assert.NotNil(t, FromContext(context.Background()))
	assert.NotNil(t, FromContext(WithLogger(context.Background(), nil)))
}
