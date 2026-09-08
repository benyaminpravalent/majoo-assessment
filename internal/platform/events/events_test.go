package events

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestBusDeliversEveryPublishedEvent(t *testing.T) {
	t.Parallel()

	const total = 200

	var (
		mu       sync.Mutex
		received []string
		wg       sync.WaitGroup
	)
	wg.Add(total)

	bus := NewBus(4, total, quietLogger())
	bus.Subscribe(func(_ context.Context, e Event) {
		mu.Lock()
		received = append(received, e.Name)
		mu.Unlock()
		wg.Done()
	})
	bus.Start()

	for i := 0; i < total; i++ {
		bus.Publish(context.Background(), Event{Name: "post.created"})
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.True(t, bus.Shutdown(ctx))

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, received, total)

	published, dropped, _ := bus.Stats()
	assert.Equal(t, int64(total), published)
	assert.Zero(t, dropped)
}

func TestBusFansOutToEveryHandler(t *testing.T) {
	t.Parallel()

	var first, second atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)

	bus := NewBus(1, 4, quietLogger())
	bus.Subscribe(func(context.Context, Event) { first.Add(1); wg.Done() })
	bus.Subscribe(func(context.Context, Event) { second.Add(1); wg.Done() })
	bus.Start()

	bus.Publish(context.Background(), Event{Name: "user.registered"})
	wg.Wait()
	drain(t, bus)

	assert.Equal(t, int64(1), first.Load())
	assert.Equal(t, int64(1), second.Load())
}

// TestPublishDropsRatherThanBlockingWhenFull is the backpressure decision this
// package exists to make explicit: a full queue must never slow down the HTTP
// request that produced the event.
func TestPublishDropsRatherThanBlockingWhenFull(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)

	// One worker, queue depth of one. The worker is parked inside the handler,
	// so after one queued event every further publish must be dropped.
	bus := NewBus(1, 1, quietLogger())
	bus.Subscribe(func(context.Context, Event) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	})
	bus.Start()

	bus.Publish(context.Background(), Event{Name: "first"})
	<-started // the worker now holds the first event and is blocked

	// Fill the single queue slot, then overflow it.
	bus.Publish(context.Background(), Event{Name: "queued"})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			bus.Publish(context.Background(), Event{Name: "overflow"})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked when the queue was full; it must drop instead")
	}

	_, dropped, _ := bus.Stats()
	assert.Positive(t, dropped, "overflowing events must be counted as dropped")

	close(release)
	drain(t, bus)
}

// TestHandlerPanicDoesNotKillTheWorker: one bad handler must not stop every
// later event from being processed.
func TestHandlerPanicDoesNotKillTheWorker(t *testing.T) {
	t.Parallel()

	var survived atomic.Int64
	var wg sync.WaitGroup
	wg.Add(3)

	bus := NewBus(1, 8, quietLogger())
	bus.Subscribe(func(_ context.Context, e Event) {
		defer wg.Done()
		if e.Name == "boom" {
			panic("handler exploded")
		}
		survived.Add(1)
	})
	bus.Start()

	bus.Publish(context.Background(), Event{Name: "ok"})
	bus.Publish(context.Background(), Event{Name: "boom"})
	bus.Publish(context.Background(), Event{Name: "ok"})
	wg.Wait()
	drain(t, bus)

	assert.Equal(t, int64(2), survived.Load())
}

func TestPublishStampsOccurredAtAndRequestID(t *testing.T) {
	t.Parallel()

	var (
		got Event
		wg  sync.WaitGroup
	)
	wg.Add(1)

	bus := NewBus(1, 4, quietLogger())
	bus.Subscribe(func(_ context.Context, e Event) { got = e; wg.Done() })
	bus.Start()

	ctx := httpx.WithRequestID(context.Background(), "req-42")
	bus.Publish(ctx, Event{Name: "post.published"})
	wg.Wait()
	drain(t, bus)

	assert.Equal(t, "req-42", got.RequestID, "events must inherit the request's correlation ID")
	assert.False(t, got.OccurredAt.IsZero())
}

func TestPublishKeepsCallerSuppliedRequestID(t *testing.T) {
	t.Parallel()

	var got Event
	var wg sync.WaitGroup
	wg.Add(1)

	bus := NewBus(1, 4, quietLogger())
	bus.Subscribe(func(_ context.Context, e Event) { got = e; wg.Done() })
	bus.Start()

	bus.Publish(httpx.WithRequestID(context.Background(), "from-context"),
		Event{Name: "x", RequestID: "explicit"})
	wg.Wait()
	drain(t, bus)

	assert.Equal(t, "explicit", got.RequestID)
}

func TestShutdownDrainsQueuedWork(t *testing.T) {
	t.Parallel()

	var handled atomic.Int64

	bus := NewBus(2, 64, quietLogger())
	bus.Subscribe(func(context.Context, Event) {
		time.Sleep(time.Millisecond)
		handled.Add(1)
	})
	bus.Start()

	const queued = 40
	for i := 0; i < queued; i++ {
		bus.Publish(context.Background(), Event{Name: "audit"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.True(t, bus.Shutdown(ctx), "shutdown must complete within the grace period")

	published, dropped, _ := bus.Stats()
	assert.Equal(t, published, handled.Load(),
		"every accepted event must be handled before shutdown returns")
	assert.Zero(t, dropped)
}

// TestPublishAfterShutdownIsSafe: a request finishing concurrently with
// shutdown must not panic on a send to a closed channel.
func TestPublishAfterShutdownIsSafe(t *testing.T) {
	t.Parallel()

	bus := NewBus(1, 4, quietLogger())
	bus.Subscribe(func(context.Context, Event) {})
	bus.Start()
	drain(t, bus)

	assert.NotPanics(t, func() {
		bus.Publish(context.Background(), Event{Name: "late"})
	})
	_, dropped, _ := bus.Stats()
	assert.Positive(t, dropped)
}

func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()

	bus := NewBus(1, 4, quietLogger())
	bus.Subscribe(func(context.Context, Event) {})
	bus.Start()

	drain(t, bus)
	assert.NotPanics(t, func() { drain(t, bus) })
}

// TestConcurrentPublishAndShutdown is the case worth running under -race: many
// producers racing a shutdown is exactly the shape of a rolling deploy.
func TestConcurrentPublishAndShutdown(t *testing.T) {
	t.Parallel()

	bus := NewBus(4, 32, quietLogger())
	bus.Subscribe(func(context.Context, Event) {})
	bus.Start()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				bus.Publish(context.Background(), Event{Name: "concurrent"})
			}
		}()
	}

	go func() {
		time.Sleep(2 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		bus.Shutdown(ctx)
	}()

	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bus.Shutdown(ctx)
}

func TestSubscribeAfterStartPanics(t *testing.T) {
	t.Parallel()

	bus := NewBus(1, 1, quietLogger())
	bus.Start()
	defer drain(t, bus)

	// Registering a handler after workers are reading the slice would be a data
	// race. Panicking makes the invariant enforced rather than merely documented.
	assert.Panics(t, func() { bus.Subscribe(func(context.Context, Event) {}) })
}

func TestStartTwicePanics(t *testing.T) {
	t.Parallel()

	bus := NewBus(1, 1, quietLogger())
	bus.Start()
	defer drain(t, bus)

	assert.Panics(t, bus.Start)
}

func TestNewBusClampsInvalidSizes(t *testing.T) {
	t.Parallel()

	bus := NewBus(0, 0, nil)
	bus.Subscribe(func(context.Context, Event) {})
	bus.Start()
	defer drain(t, bus)

	assert.Equal(t, 1, bus.workers)
	assert.Equal(t, 1, cap(bus.queue))
}

func drain(t *testing.T, bus *Bus) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bus.Shutdown(ctx)
}
