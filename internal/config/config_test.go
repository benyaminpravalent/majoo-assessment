package config

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validEnv is the smallest environment that produces a usable configuration.
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL": "postgres://blog:blog@localhost:5432/blog?sslmode=disable",
		"JWT_SECRET":   strings.Repeat("k", 48),
	}
}

// setEnv applies the given variables for the duration of the test. t.Setenv
// takes care of restoring the previous values and forbids parallel tests, which
// is exactly right for a package that reads process-wide state.
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
}

func TestLoadAppliesDocumentedDefaults(t *testing.T) {
	setEnv(t, validEnv())

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, EnvDevelopment, cfg.AppEnv)
	assert.Equal(t, ":8080", cfg.HTTP.Addr)
	assert.Equal(t, 15*time.Minute, cfg.Auth.AccessTTL)
	assert.Equal(t, 720*time.Hour, cfg.Auth.RefreshTTL)
	assert.Equal(t, 12, cfg.Auth.BcryptCost)
	assert.Equal(t, int64(1<<20), cfg.HTTP.MaxBodyBytes)
	assert.True(t, cfg.Rate.Enabled)
	assert.False(t, cfg.HTTP.TrustProxyHeaders,
		"proxy headers must not be trusted unless explicitly enabled")
	assert.Equal(t, "json", cfg.Log.Format)
}

func TestLoadReadsOverrides(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = EnvProduction
	env["HTTP_ADDR"] = ":9090"
	env["JWT_ACCESS_TTL"] = "5m"
	env["JWT_REFRESH_TTL"] = "48h"
	env["BCRYPT_COST"] = "11"
	env["DB_MAX_CONNS"] = "50"
	env["RATE_LIMIT_ENABLED"] = "false"
	env["CORS_ALLOWED_ORIGINS"] = "https://a.example.com, https://b.example.com"
	env["LOG_FORMAT"] = "text"
	env["TRUST_PROXY_HEADERS"] = "true"
	setEnv(t, env)

	cfg, err := Load()

	require.NoError(t, err)
	assert.True(t, cfg.IsProduction())
	assert.Equal(t, ":9090", cfg.HTTP.Addr)
	assert.Equal(t, 5*time.Minute, cfg.Auth.AccessTTL)
	assert.Equal(t, int32(50), cfg.DB.MaxConns)
	assert.False(t, cfg.Rate.Enabled)
	assert.Equal(t, []string{"https://a.example.com", "https://b.example.com"}, cfg.CORS.AllowedOrigins,
		"comma-separated origins must be trimmed")
	assert.Equal(t, "text", cfg.Log.Format)
	assert.True(t, cfg.HTTP.TrustProxyHeaders)
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	env := validEnv()
	delete(env, "DATABASE_URL")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", env["JWT_SECRET"])

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

// TestLoadRejectsWeakJWTSecret: an HS256 key shorter than the hash output
// weakens the MAC, so the process must refuse to start rather than run with it.
func TestLoadRejectsWeakJWTSecret(t *testing.T) {
	for name, secret := range map[string]string{
		"missing": "",
		"short":   "too-short",
		"31 byte": strings.Repeat("k", 31),
	} {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			env["JWT_SECRET"] = secret
			setEnv(t, env)
			if secret == "" {
				t.Setenv("JWT_SECRET", "")
			}

			_, err := Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "JWT_SECRET")
		})
	}
}

func TestLoadRejectsWeakBcryptCost(t *testing.T) {
	env := validEnv()
	env["BCRYPT_COST"] = "4"
	setEnv(t, env)

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "BCRYPT_COST")
}

// TestLoadRejectsWildcardCORSInProduction guards the single most common CORS
// misconfiguration.
func TestLoadRejectsWildcardCORSInProduction(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = EnvProduction
	env["CORS_ALLOWED_ORIGINS"] = "*"
	setEnv(t, env)

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "CORS_ALLOWED_ORIGINS")
}

func TestLoadAllowsWildcardCORSInDevelopment(t *testing.T) {
	env := validEnv()
	env["CORS_ALLOWED_ORIGINS"] = "*"
	setEnv(t, env)

	cfg, err := Load()

	require.NoError(t, err)
	assert.Equal(t, []string{"*"}, cfg.CORS.AllowedOrigins)
}

// TestLoadRejectsHandlerTimeoutAtOrAboveWriteTimeout: if the write timeout
// fires first the connection is closed before any error can be rendered, so the
// relationship between the two is a configuration error, not a preference.
func TestLoadRejectsHandlerTimeoutAtOrAboveWriteTimeout(t *testing.T) {
	env := validEnv()
	env["HTTP_HANDLER_TIMEOUT"] = "30s"
	env["HTTP_WRITE_TIMEOUT"] = "20s"
	setEnv(t, env)

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP_HANDLER_TIMEOUT")
}

func TestLoadRejectsRefreshTTLShorterThanAccessTTL(t *testing.T) {
	env := validEnv()
	env["JWT_ACCESS_TTL"] = "1h"
	env["JWT_REFRESH_TTL"] = "30m"
	setEnv(t, env)

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "JWT_REFRESH_TTL")
}

func TestLoadRejectsInconsistentPoolSizes(t *testing.T) {
	env := validEnv()
	env["DB_MIN_CONNS"] = "30"
	env["DB_MAX_CONNS"] = "10"
	setEnv(t, env)

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DB_MIN_CONNS")
}

func TestLoadReportsUnparsableValues(t *testing.T) {
	tests := map[string]struct {
		key, value string
	}{
		"duration": {"JWT_ACCESS_TTL", "fifteen-minutes"},
		"integer":  {"DB_MAX_CONNS", "many"},
		"boolean":  {"RATE_LIMIT_ENABLED", "yes-please"},
		"float":    {"RATE_LIMIT_RPS", "fast"},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			env[tc.key] = tc.value
			setEnv(t, env)

			_, err := Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.key)
		})
	}
}

// TestLoadReportsEveryProblemAtOnce: an operator fixing a broken deployment
// should not have to restart the process once per mistake.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("JWT_SECRET", "short")
	t.Setenv("BCRYPT_COST", "2")
	t.Setenv("LOG_LEVEL", "verbose")

	_, err := Load()

	require.Error(t, err)
	msg := err.Error()
	for _, want := range []string{"DATABASE_URL", "JWT_SECRET", "BCRYPT_COST", "LOG_LEVEL"} {
		assert.Contains(t, msg, want)
	}
}

func TestLoadRejectsUnknownAppEnv(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = "staging-ish"
	setEnv(t, env)

	_, err := Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "APP_ENV")
}
