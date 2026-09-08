// Package config loads and validates all runtime configuration from the
// environment. Configuration is read exactly once at start-up and then passed
// explicitly down the call graph, so the process holds no mutable globals.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment names recognised by the application.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
	EnvTest        = "test"
)

// minJWTSecretLen is the shortest secret we accept. HS256 keys shorter than the
// hash output (32 bytes) weaken the MAC, so we refuse to start with one.
const minJWTSecretLen = 32

// Config is the fully-resolved application configuration.
type Config struct {
	AppEnv string
	HTTP   HTTPConfig
	DB     DBConfig
	Auth   AuthConfig
	Rate   RateLimitConfig
	CORS   CORSConfig
	Log    LogConfig
	Events EventsConfig
}

// HTTPConfig holds server-level networking limits. Every timeout is set
// explicitly: Go's zero values mean "no timeout", which is unsafe in production.
type HTTPConfig struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	HandlerTimeout    time.Duration
	TrustProxyHeaders bool
	MaxBodyBytes      int64
	ShutdownTimeout   time.Duration
}

// DBConfig holds the connection URL and pool sizing for PostgreSQL.
type DBConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// AuthConfig holds password-hashing and JWT parameters.
type AuthConfig struct {
	JWTSecret  []byte
	Issuer     string
	Audience   string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
	BcryptCost int
}

// RateLimitConfig configures the in-process token-bucket limiter.
type RateLimitConfig struct {
	Enabled   bool
	RPS       float64
	Burst     int
	AuthRPS   float64
	AuthBurst int
}

// CORSConfig lists the browser origins allowed to call the API.
type CORSConfig struct {
	AllowedOrigins []string
}

// LogConfig configures the structured logger.
type LogConfig struct {
	Level  string
	Format string
}

// EventsConfig sizes the asynchronous domain-event worker pool.
type EventsConfig struct {
	Workers int
	Buffer  int
}

// IsProduction reports whether the process runs with production defaults.
func (c Config) IsProduction() bool { return c.AppEnv == EnvProduction }

// Load reads configuration from the process environment, applies defaults and
// validates the result. It returns every validation problem at once so an
// operator can fix a misconfigured deployment in a single pass.
func Load() (Config, error) {
	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	cfg := Config{
		AppEnv: envString("APP_ENV", EnvDevelopment),
	}

	cfg.HTTP.Addr = envString("HTTP_ADDR", ":8080")
	cfg.HTTP.ReadHeaderTimeout = envDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second, collect)
	cfg.HTTP.ReadTimeout = envDuration("HTTP_READ_TIMEOUT", 15*time.Second, collect)
	cfg.HTTP.WriteTimeout = envDuration("HTTP_WRITE_TIMEOUT", 20*time.Second, collect)
	cfg.HTTP.IdleTimeout = envDuration("HTTP_IDLE_TIMEOUT", 60*time.Second, collect)
	cfg.HTTP.HandlerTimeout = envDuration("HTTP_HANDLER_TIMEOUT", 10*time.Second, collect)
	cfg.HTTP.MaxBodyBytes = int64(envInt("HTTP_MAX_BODY_BYTES", 1<<20, collect))
	cfg.HTTP.ShutdownTimeout = envDuration("SHUTDOWN_TIMEOUT", 15*time.Second, collect)
	cfg.HTTP.TrustProxyHeaders = envBool("TRUST_PROXY_HEADERS", false, collect)

	cfg.DB.URL = envString("DATABASE_URL", "")
	cfg.DB.MaxConns = int32(envInt("DB_MAX_CONNS", 20, collect))
	cfg.DB.MinConns = int32(envInt("DB_MIN_CONNS", 2, collect))
	cfg.DB.MaxConnLifetime = envDuration("DB_MAX_CONN_LIFETIME", 30*time.Minute, collect)
	cfg.DB.MaxConnIdleTime = envDuration("DB_MAX_CONN_IDLE_TIME", 5*time.Minute, collect)
	cfg.DB.ConnectTimeout = envDuration("DB_CONNECT_TIMEOUT", 10*time.Second, collect)

	cfg.Auth.JWTSecret = []byte(envString("JWT_SECRET", ""))
	cfg.Auth.Issuer = envString("JWT_ISSUER", "majoo-blog-api")
	cfg.Auth.Audience = envString("JWT_AUDIENCE", "majoo-blog-api")
	cfg.Auth.AccessTTL = envDuration("JWT_ACCESS_TTL", 15*time.Minute, collect)
	cfg.Auth.RefreshTTL = envDuration("JWT_REFRESH_TTL", 720*time.Hour, collect)
	cfg.Auth.BcryptCost = envInt("BCRYPT_COST", 12, collect)

	cfg.Rate.Enabled = envBool("RATE_LIMIT_ENABLED", true, collect)
	cfg.Rate.RPS = envFloat("RATE_LIMIT_RPS", 20, collect)
	cfg.Rate.Burst = envInt("RATE_LIMIT_BURST", 40, collect)
	cfg.Rate.AuthRPS = envFloat("AUTH_RATE_LIMIT_RPS", 1, collect)
	cfg.Rate.AuthBurst = envInt("AUTH_RATE_LIMIT_BURST", 10, collect)

	cfg.CORS.AllowedOrigins = envCSV("CORS_ALLOWED_ORIGINS", nil)

	cfg.Log.Level = strings.ToLower(envString("LOG_LEVEL", "info"))
	cfg.Log.Format = strings.ToLower(envString("LOG_FORMAT", "json"))

	cfg.Events.Workers = envInt("EVENTS_WORKERS", 4, collect)
	cfg.Events.Buffer = envInt("EVENTS_BUFFER", 256, collect)

	collect(cfg.validate())
	return cfg, errors.Join(errs...)
}

func (c Config) validate() error {
	var errs []error

	switch c.AppEnv {
	case EnvDevelopment, EnvProduction, EnvTest:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV: %q is not one of development|production|test", c.AppEnv))
	}

	if c.DB.URL == "" {
		errs = append(errs, errors.New("DATABASE_URL: required"))
	}
	if c.DB.MinConns < 0 || c.DB.MaxConns <= 0 || c.DB.MinConns > c.DB.MaxConns {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS/DB_MAX_CONNS: need 0 <= min (%d) <= max (%d) and max > 0",
			c.DB.MinConns, c.DB.MaxConns))
	}

	if len(c.Auth.JWTSecret) < minJWTSecretLen {
		errs = append(errs, fmt.Errorf("JWT_SECRET: required, at least %d bytes", minJWTSecretLen))
	}
	if c.Auth.AccessTTL <= 0 {
		errs = append(errs, errors.New("JWT_ACCESS_TTL: must be positive"))
	}
	if c.Auth.RefreshTTL <= c.Auth.AccessTTL {
		errs = append(errs, errors.New("JWT_REFRESH_TTL: must be greater than JWT_ACCESS_TTL"))
	}
	// bcrypt itself accepts 4..31; below 10 is too cheap for stored credentials.
	if c.Auth.BcryptCost < 10 || c.Auth.BcryptCost > 31 {
		errs = append(errs, fmt.Errorf("BCRYPT_COST: %d out of accepted range 10..31", c.Auth.BcryptCost))
	}

	if c.HTTP.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("HTTP_MAX_BODY_BYTES: must be positive"))
	}
	if c.HTTP.HandlerTimeout >= c.HTTP.WriteTimeout {
		// The handler deadline must fire first, otherwise the server closes the
		// connection before the middleware can render a 503 response.
		errs = append(errs, fmt.Errorf("HTTP_HANDLER_TIMEOUT (%s): must be less than HTTP_WRITE_TIMEOUT (%s)",
			c.HTTP.HandlerTimeout, c.HTTP.WriteTimeout))
	}

	if c.Rate.Enabled && (c.Rate.RPS <= 0 || c.Rate.Burst <= 0 || c.Rate.AuthRPS <= 0 || c.Rate.AuthBurst <= 0) {
		errs = append(errs, errors.New("RATE_LIMIT_*: rates and bursts must be positive when rate limiting is enabled"))
	}

	for _, o := range c.CORS.AllowedOrigins {
		if o == "*" && c.IsProduction() {
			errs = append(errs, errors.New(`CORS_ALLOWED_ORIGINS: "*" is not allowed when APP_ENV=production`))
		}
	}

	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %q is not one of debug|info|warn|error", c.Log.Level))
	}
	switch c.Log.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT: %q is not one of json|text", c.Log.Format))
	}

	if c.Events.Workers <= 0 || c.Events.Buffer <= 0 {
		errs = append(errs, errors.New("EVENTS_WORKERS/EVENTS_BUFFER: must be positive"))
	}

	return errors.Join(errs...)
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envCSV(key string, def []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envInt(key string, def int, collect func(error)) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not an integer", key, raw))
		return def
	}
	return v
}

func envFloat(key string, def float64, collect func(error)) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not a number", key, raw))
		return def
	}
	return v
}

func envBool(key string, def bool, collect func(error)) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not a boolean", key, raw))
		return def
	}
	return v
}

func envDuration(key string, def time.Duration, collect func(error)) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		collect(fmt.Errorf("%s: %q is not a duration (e.g. 15s, 5m, 1h)", key, raw))
		return def
	}
	return v
}
