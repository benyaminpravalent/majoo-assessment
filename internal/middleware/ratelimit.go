package middleware

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
)

// RateLimiter is an in-process token-bucket limiter keyed by caller.
//
// Scope and honesty about it: this limits one process. Behind N replicas the
// effective limit is N times the configured rate, and a restart resets every
// bucket. That is acceptable for the abuse this control targets — credential
// stuffing and accidental client loops — and it costs no extra infrastructure.
// A cluster-wide limit needs shared state; the Redis design is written up in
// docs/architecture-decisions.md (ADR-008) and is the same algorithm with the
// bucket kept in a Lua script.
//
// golang.org/x/time/rate would do the arithmetic below, but it has no eviction:
// its Limiter is per-key and the map still has to be managed. Since the map and
// its janitor are the actual work here, the ~40 lines of bucket maths are
// written out rather than pulling in a dependency for a third of the job.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	rate  float64 // tokens added per second
	burst float64 // bucket capacity

	// idleTTL is how long an untouched bucket survives before the janitor
	// reclaims it. Without eviction the map grows once per distinct client
	// address and never shrinks, which is a slow memory leak on a public API.
	idleTTL time.Duration

	now  func() time.Time
	stop chan struct{}
	done chan struct{}
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiterOption customises a RateLimiter.
type RateLimiterOption func(*RateLimiter)

// WithClock replaces the time source. Test-only in practice: it is what lets
// the limiter's refill behaviour be tested without sleeping.
func WithClock(now func() time.Time) RateLimiterOption {
	return func(l *RateLimiter) { l.now = now }
}

// WithIdleTTL sets how long an idle bucket is retained.
func WithIdleTTL(d time.Duration) RateLimiterOption {
	return func(l *RateLimiter) { l.idleTTL = d }
}

// NewRateLimiter returns a limiter allowing rate requests per second with the
// given burst, and starts its eviction goroutine. Close must be called to stop
// that goroutine; the server does so during shutdown.
func NewRateLimiter(rate float64, burst int, opts ...RateLimiterOption) *RateLimiter {
	l := &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   float64(burst),
		idleTTL: 10 * time.Minute,
		now:     time.Now,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}

	go l.janitor()
	return l
}

// Allow reports whether a request from key may proceed, and if not, how long
// the caller should wait before retrying.
func (l *RateLimiter) Allow(key string) (bool, time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		// A new caller starts with a full bucket minus this request.
		l.buckets[key] = &bucket{tokens: l.burst - 1, lastSeen: now}
		return true, 0
	}

	// Refill lazily rather than on a ticker: a bucket only needs to be accurate
	// at the moment it is read, so there is no work at all for idle keys.
	elapsed := now.Sub(b.lastSeen).Seconds()
	if elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.rate)
	}
	b.lastSeen = now

	if b.tokens < 1 {
		// Time until one whole token is available.
		deficit := 1 - b.tokens
		return false, time.Duration(deficit / l.rate * float64(time.Second))
	}

	b.tokens--
	return true, 0
}

// Len reports the number of tracked buckets. Used by tests and by the readiness
// endpoint's diagnostics.
func (l *RateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// Close stops the eviction goroutine and blocks until it has exited.
func (l *RateLimiter) Close() {
	select {
	case <-l.stop:
		// Already closed; Close is idempotent so shutdown paths can be defensive.
	default:
		close(l.stop)
	}
	<-l.done
}

func (l *RateLimiter) janitor() {
	defer close(l.done)

	ticker := time.NewTicker(l.idleTTL / 2)
	defer ticker.Stop()

	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.evictIdle()
		}
	}
}

func (l *RateLimiter) evictIdle() {
	cutoff := l.now().Add(-l.idleTTL)

	l.mu.Lock()
	defer l.mu.Unlock()
	for key, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, key)
		}
	}
}

// Middleware rejects requests that exceed the limiter's budget.
//
// Requests are keyed by authenticated user when one is present and by client
// address otherwise. Keying an authenticated request by user rather than by IP
// means a company behind one NAT address does not throttle itself, while an
// anonymous flood is still bounded.
func (l *RateLimiter) Middleware(trustProxy bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			key := "ip:" + ClientIP(r, trustProxy)
			if actor, ok := httpx.Actor(ctx); ok {
				key = "user:" + actor.UserID.String()
			}

			allowed, retryAfter := l.Allow(key)
			if !allowed {
				seconds := int(math.Ceil(retryAfter.Seconds()))
				if seconds < 1 {
					seconds = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				httpx.WriteError(ctx, w, apierr.RateLimited("too many requests; please retry later"))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
