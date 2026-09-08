package httpx

import (
	"context"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
)

type (
	requestIDKey struct{}
	actorKey     struct{}
)

// WithRequestID returns a context carrying the correlation ID for a request.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the correlation ID attached by the RequestID middleware, or
// the empty string outside a request.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// WithActor returns a context carrying the authenticated principal. Only the
// authentication middleware calls this, after a token has been fully verified.
func WithActor(ctx context.Context, a domain.Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// Actor returns the authenticated principal and whether one is present.
// Handlers behind RequireAuth can rely on ok being true; handlers on optionally
// authenticated routes use ok to vary their behaviour.
func Actor(ctx context.Context) (domain.Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(domain.Actor)
	return a, ok
}

// ActorPtr returns the authenticated principal, or nil on an anonymous request.
//
// Read paths that vary by caller take a pointer because "nobody is logged in"
// is a meaningful state there, unlike write paths where authentication is
// mandatory and a value type is the safer signature.
func ActorPtr(ctx context.Context) *domain.Actor {
	a, ok := Actor(ctx)
	if !ok {
		return nil
	}
	return &a
}
