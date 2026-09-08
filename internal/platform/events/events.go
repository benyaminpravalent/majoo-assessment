// Package events provides a small in-process publisher for domain events.
//
// Why this exists at all: several actions in this API have follow-up work that
// the caller should not wait for — writing an audit trail, warming a cache,
// eventually notifying a post's author. Doing that inline lengthens the request;
// doing it in a bare `go func()` loses the work on shutdown and gives an
// attacker an unbounded goroutine factory.
//
// So this is a bounded worker pool with explicit backpressure:
//
//   - a fixed number of workers, so concurrency does not scale with traffic;
//   - a buffered queue with a fixed depth, so memory is bounded;
//   - a non-blocking Publish that drops and records when the queue is full,
//     because delaying a user's HTTP response to make room for an audit log is
//     the wrong trade;
//   - Shutdown drains the queue within a deadline, so in-flight work survives a
//     rolling deploy.
//
// It is explicitly not a message broker, and the docs say so: at-most-once,
// in-process, lost if the process is killed. docs/architecture-decisions.md
// (ADR-007) describes the transactional-outbox upgrade path to Kafka, which is
// what the e-commerce design in docs/ecommerce-architecture.md assumes.
package events

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
)

// Event is one thing that happened, described in past tense.
type Event struct {
	// Name identifies the event type, e.g. "post.published".
	Name string
	// ActorID is the user who caused it, if any.
	ActorID string
	// SubjectID is the resource it happened to.
	SubjectID string
	// RequestID correlates the event with the HTTP request that produced it.
	RequestID string
	// OccurredAt is set by Publish if the caller left it zero.
	OccurredAt time.Time
	// Attributes carries event-specific detail. Values are logged, so they must
	// not contain secrets; the logging package redacts by key as a backstop.
	Attributes map[string]any
}

// Handler processes one event. Handlers must be safe for concurrent use and
// should respect ctx, which is cancelled when the drain deadline passes.
type Handler func(ctx context.Context, e Event)

// Bus fans events out to registered handlers on a fixed worker pool.
type Bus struct {
	queue   chan Event
	logger  *slog.Logger
	workers int

	// handlers is written only before Start and read only after, so no lock is
	// needed on the hot path. Subscribe panics if called after Start to keep
	// that invariant honest rather than merely documented.
	handlers []Handler
	started  atomic.Bool

	wg sync.WaitGroup

	// closed guards the queue against a send on a closed channel once Shutdown
	// has run. It is checked before every publish and set before the close.
	closed    atomic.Bool
	closeOnce sync.Once

	published atomic.Int64
	dropped   atomic.Int64
}

// NewBus returns a Bus with the given worker count and queue depth. Both must
// be positive; config.Load validates them.
func NewBus(workers, buffer int, logger *slog.Logger) *Bus {
	if workers < 1 {
		workers = 1
	}
	if buffer < 1 {
		buffer = 1
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Bus{
		queue:   make(chan Event, buffer),
		logger:  logger,
		workers: workers,
	}
}

// Subscribe registers a handler. It must be called before Start.
func (b *Bus) Subscribe(h Handler) {
	if b.started.Load() {
		panic("events: Subscribe called after Start")
	}
	b.handlers = append(b.handlers, h)
}

// Start launches the worker goroutines. Calling it twice is a programming
// error and panics, because a second pool would silently double every handler.
func (b *Bus) Start() {
	if !b.started.CompareAndSwap(false, true) {
		panic("events: Start called twice")
	}
	for i := 0; i < b.workers; i++ {
		b.wg.Add(1)
		go b.work()
	}
}

func (b *Bus) work() {
	defer b.wg.Done()
	for e := range b.queue {
		b.dispatch(e)
	}
}

// dispatch runs every handler for one event, isolating panics so that a bug in
// one handler cannot take down a worker — and with it, every later event.
func (b *Bus) dispatch(e Event) {
	// Handlers get their own deadline: an event handler that blocks forever
	// would otherwise hold a worker for the life of the process.
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	ctx = logging.WithLogger(ctx, b.logger.With(
		slog.String("event", e.Name),
		slog.String("request_id", e.RequestID),
	))

	for _, h := range b.handlers {
		b.safeInvoke(ctx, h, e)
	}
}

const handlerTimeout = 10 * time.Second

func (b *Bus) safeInvoke(ctx context.Context, h Handler, e Event) {
	defer func() {
		if p := recover(); p != nil {
			b.logger.Error("event handler panicked",
				slog.String("event", e.Name),
				slog.Any("panic", p))
		}
	}()
	h(ctx, e)
}

// Publish enqueues an event without blocking.
//
// If the queue is full the event is dropped and counted. That is a deliberate
// choice: this queue carries best-effort side work, and the alternative —
// blocking the HTTP handler until a worker frees up — turns a slow handler into
// a site-wide latency spike. The drop is logged at warn level so the condition
// is visible rather than silent.
func (b *Bus) Publish(ctx context.Context, e Event) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.RequestID == "" {
		e.RequestID = httpx.RequestID(ctx)
	}

	if b.closed.Load() {
		b.dropped.Add(1)
		return
	}
	// Shutdown can close the queue between the check above and the send below.
	// Recovering from that narrow race is cheaper than serialising every publish
	// behind a mutex, and the outcome is identical: the event is dropped.
	defer func() {
		if recover() != nil {
			b.dropped.Add(1)
		}
	}()

	select {
	case b.queue <- e:
		b.published.Add(1)
	default:
		n := b.dropped.Add(1)
		b.logger.Warn("event dropped: queue full",
			slog.String("event", e.Name),
			slog.Int("queue_capacity", cap(b.queue)),
			slog.Int64("dropped_total", n))
	}
}

// Stats reports counters suitable for exporting as metrics.
func (b *Bus) Stats() (published, dropped int64, queued int) {
	return b.published.Load(), b.dropped.Load(), len(b.queue)
}

// Shutdown stops accepting new events and waits for the queue to drain, or for
// ctx to be cancelled — whichever happens first. It reports whether the drain
// completed, so the caller can log a clean stop versus a truncated one.
//
// Publish after Shutdown is safe: it drops the event rather than panicking.
func (b *Bus) Shutdown(ctx context.Context) bool {
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.queue)
	})

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		remaining := len(b.queue)
		b.logger.Warn("event bus shutdown timed out before draining",
			slog.Int("events_remaining", remaining))
		return false
	}
}
