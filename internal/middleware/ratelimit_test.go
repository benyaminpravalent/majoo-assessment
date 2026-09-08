package middleware

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeClock lets the refill behaviour be tested without sleeping, which keeps
// the suite fast and, more importantly, deterministic.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestRateLimiterAllowsUpToBurstThenRejects(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(1, 3, WithClock(clock.Now))
	defer l.Close()

	for i := 0; i < 3; i++ {
		allowed, _ := l.Allow("client")
		assert.True(t, allowed, "request %d should be within the burst of 3", i+1)
	}

	allowed, retryAfter := l.Allow("client")

	assert.False(t, allowed, "the fourth request exceeds the burst")
	assert.Positive(t, retryAfter, "a rejected caller must be told how long to wait")
}

func TestRateLimiterRefillsOverTime(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(2, 2, WithClock(clock.Now)) // 2 tokens per second
	defer l.Close()

	require.True(t, first(l.Allow("client")))
	require.True(t, first(l.Allow("client")))
	require.False(t, first(l.Allow("client")))

	// Half a second at 2 rps is exactly one token.
	clock.Advance(500 * time.Millisecond)

	assert.True(t, first(l.Allow("client")), "one token should have been refilled")
	assert.False(t, first(l.Allow("client")), "but only one")
}

func TestRateLimiterDoesNotRefillBeyondBurst(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(10, 2, WithClock(clock.Now))
	defer l.Close()

	require.True(t, first(l.Allow("client")))
	// A long idle period must not let a caller bank unlimited requests.
	clock.Advance(time.Hour)

	assert.True(t, first(l.Allow("client")))
	assert.True(t, first(l.Allow("client")))
	assert.False(t, first(l.Allow("client")), "the bucket is capped at the burst size")
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(1, 1, WithClock(clock.Now))
	defer l.Close()

	assert.True(t, first(l.Allow("client-a")))
	assert.False(t, first(l.Allow("client-a")))
	assert.True(t, first(l.Allow("client-b")), "one caller's budget must not affect another's")
}

func TestRateLimiterRetryAfterShrinksAsTokensAccrue(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(1, 1, WithClock(clock.Now))
	defer l.Close()

	require.True(t, first(l.Allow("client")))
	_, longWait := l.Allow("client")

	clock.Advance(500 * time.Millisecond)
	_, shortWait := l.Allow("client")

	assert.Less(t, shortWait, longWait)
}

// TestRateLimiterEvictsIdleBuckets: without eviction the map grows once per
// distinct client address and never shrinks, which is a slow memory leak on a
// public API.
func TestRateLimiterEvictsIdleBuckets(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(1, 1, WithClock(clock.Now), WithIdleTTL(20*time.Millisecond))
	defer l.Close()

	for i := 0; i < 5; i++ {
		l.Allow("client-" + strconv.Itoa(i))
	}
	require.Equal(t, 5, l.Len())

	// Move the fake clock past the TTL, then wait for the janitor's real ticker.
	clock.Advance(time.Minute)
	assert.Eventually(t, func() bool { return l.Len() == 0 }, 2*time.Second, 5*time.Millisecond,
		"idle buckets should be reclaimed by the janitor")
}

func TestRateLimiterCloseIsIdempotentAndStopsTheJanitor(t *testing.T) {
	t.Parallel()

	l := NewRateLimiter(1, 1, WithIdleTTL(time.Minute))

	l.Close()
	assert.NotPanics(t, l.Close, "shutdown paths may call Close more than once")
}

// TestRateLimiterUnderConcurrentLoad is the case to run with -race: many
// goroutines hitting one bucket is exactly what a burst of traffic looks like.
func TestRateLimiterUnderConcurrentLoad(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	const burst = 50
	l := NewRateLimiter(0.0001, burst, WithClock(clock.Now)) // effectively no refill
	defer l.Close()

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if ok, _ := l.Allow("shared"); ok {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// 400 attempts against a burst of 50 with no meaningful refill: exactly the
	// burst must get through, no more and no fewer.
	assert.Equal(t, int64(burst), allowed.Load())
}

func TestMiddlewareReturns429WithRetryAfter(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(1, 1, WithClock(clock.Now))
	defer l.Close()

	var served atomic.Int64
	h := l.Middleware(false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = "203.0.113.5:1234"
		return r
	}

	first := httptest.NewRecorder()
	h.ServeHTTP(first, req())
	assert.Equal(t, http.StatusOK, first.Code)

	second := httptest.NewRecorder()
	h.ServeHTTP(second, req())

	assert.Equal(t, http.StatusTooManyRequests, second.Code)
	assert.Equal(t, int64(1), served.Load(), "a throttled request must not reach the handler")
	assert.NotEmpty(t, second.Header().Get("Retry-After"))
	assert.Contains(t, second.Body.String(), `"code":"rate_limited"`)
}

// TestMiddlewareKeysAuthenticatedRequestsByUser means a whole office behind one
// NAT address does not throttle itself once its users are signed in.
func TestMiddlewareKeysAuthenticatedRequestsByUser(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	l := NewRateLimiter(1, 1, WithClock(clock.Now))
	defer l.Close()

	h := l.Middleware(false)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	sharedIP := "198.51.100.10:5555"
	for _, actor := range []domain.Actor{
		{UserID: uuid.New(), Role: domain.RoleUser},
		{UserID: uuid.New(), Role: domain.RoleUser},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = sharedIP
		r = r.WithContext(httpx.WithActor(r.Context(), actor))

		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		assert.Equal(t, http.StatusOK, w.Code,
			"two different users sharing an address each get their own budget")
	}
}

func first(allowed bool, _ time.Duration) bool { return allowed }
