package middleware

import (
	"net/http"
	"strings"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
)

// tokenParser is the piece of auth.TokenService this package needs. Declaring
// it here keeps the middleware testable with a two-line stub.
type tokenParser interface {
	ParseAccessToken(raw string) (domain.Actor, error)
}

// Authenticator turns bearer tokens into actors on the request context.
type Authenticator struct {
	tokens tokenParser
}

// NewAuthenticator returns an Authenticator using the given token parser.
func NewAuthenticator(tokens tokenParser) *Authenticator {
	return &Authenticator{tokens: tokens}
}

// Require rejects any request without a valid bearer token.
//
// The WWW-Authenticate header is set on rejection because RFC 9110 requires it
// for a 401, and because it tells a client library which scheme to retry with.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		raw, err := bearerToken(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			httpx.WriteError(ctx, w, err)
			return
		}

		actor, err := a.tokens.ParseAccessToken(raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
			httpx.WriteError(ctx, w, err)
			return
		}

		next.ServeHTTP(w, r.WithContext(httpx.WithActor(ctx, actor)))
	})
}

// Optional attaches an actor when a valid token is present and otherwise lets
// the request through anonymously.
//
// An invalid token is still rejected rather than ignored. Silently treating a
// malformed or expired token as "anonymous" would show an author the public
// view of their own drafts with no indication that their session had lapsed,
// which is a confusing failure to debug from the client side.
func (a *Authenticator) Optional(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if header == "" {
			next.ServeHTTP(w, r)
			return
		}

		raw, err := bearerToken(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			httpx.WriteError(ctx, w, err)
			return
		}

		actor, err := a.tokens.ParseAccessToken(raw)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api", error="invalid_token"`)
			httpx.WriteError(ctx, w, err)
			return
		}

		next.ServeHTTP(w, r.WithContext(httpx.WithActor(ctx, actor)))
	})
}

// RequireAdmin rejects an authenticated caller who is not an administrator. It
// must be composed after Require.
//
// No route uses it today — post and comment ownership rules already grant
// administrators access through domain.Actor.CanModify — but it is the correct
// place for a future admin-only endpoint, and having it here keeps that
// decision out of the handlers.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		actor, ok := httpx.Actor(ctx)
		if !ok {
			httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
			return
		}
		if !actor.IsAdmin() {
			httpx.WriteError(ctx, w, apierr.Forbidden("this endpoint requires the admin role"))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the credential from an Authorization header.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", apierr.Unauthorized("authorization header is required")
	}

	scheme, token, found := strings.Cut(header, " ")
	// The scheme is case-insensitive per RFC 9110; the token is not.
	if !found || !strings.EqualFold(scheme, "bearer") || strings.TrimSpace(token) == "" {
		return "", apierr.Unauthorized("authorization header must be of the form: Bearer <token>")
	}

	return strings.TrimSpace(token), nil
}
