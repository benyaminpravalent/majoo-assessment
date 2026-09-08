package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubParser accepts exactly one token value and rejects everything else.
type stubParser struct {
	validToken string
	actor      domain.Actor
}

func (s stubParser) ParseAccessToken(raw string) (domain.Actor, error) {
	if raw == s.validToken {
		return s.actor, nil
	}
	return domain.Actor{}, apierr.Unauthorized("invalid or expired access token").
		WithCause(errors.New("stub rejected the token"))
}

func newTestAuthenticator() (*Authenticator, domain.Actor) {
	actor := domain.Actor{UserID: uuid.New(), Role: domain.RoleUser}
	return NewAuthenticator(stubParser{validToken: "good-token", actor: actor}), actor
}

// captureActor records whether the handler ran and with which principal.
func captureActor(ran *bool, got *domain.Actor, present *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		a, ok := httpx.Actor(r.Context())
		*got, *present = a, ok
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequireAcceptsValidBearerToken(t *testing.T) {
	t.Parallel()

	auth, want := newTestAuthenticator()
	var ran, present bool
	var got domain.Actor

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer good-token")
	w := httptest.NewRecorder()

	auth.Require(captureActor(&ran, &got, &present)).ServeHTTP(w, req)

	assert.True(t, ran)
	assert.True(t, present)
	assert.Equal(t, want, got)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireAcceptsCaseInsensitiveScheme(t *testing.T) {
	t.Parallel()

	auth, _ := newTestAuthenticator()
	var ran, present bool
	var got domain.Actor

	// RFC 9110 makes the auth scheme case-insensitive; some clients send "bearer".
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "bearer good-token")

	auth.Require(captureActor(&ran, &got, &present)).ServeHTTP(httptest.NewRecorder(), req)

	assert.True(t, ran)
}

func TestRequireRejectsMissingOrMalformedHeader(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"absent":               "",
		"no scheme":            "good-token",
		"wrong scheme":         "Basic dXNlcjpwYXNz",
		"scheme without token": "Bearer",
		"empty token":          "Bearer    ",
	}

	auth, _ := newTestAuthenticator()
	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var ran, present bool
			var got domain.Actor

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			w := httptest.NewRecorder()

			auth.Require(captureActor(&ran, &got, &present)).ServeHTTP(w, req)

			assert.False(t, ran, "the handler must not run without valid credentials")
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Contains(t, w.Header().Get("WWW-Authenticate"), "Bearer",
				"a 401 must advertise the accepted scheme")
		})
	}
}

func TestRequireRejectsInvalidToken(t *testing.T) {
	t.Parallel()

	auth, _ := newTestAuthenticator()
	var ran, present bool
	var got domain.Actor

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer forged-token")
	w := httptest.NewRecorder()

	auth.Require(captureActor(&ran, &got, &present)).ServeHTTP(w, req)

	assert.False(t, ran)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Header().Get("WWW-Authenticate"), "invalid_token")
	assert.Contains(t, w.Body.String(), `"code":"unauthorized"`)
}

func TestOptionalAllowsAnonymousRequests(t *testing.T) {
	t.Parallel()

	auth, _ := newTestAuthenticator()
	var ran, present bool
	var got domain.Actor

	w := httptest.NewRecorder()
	auth.Optional(captureActor(&ran, &got, &present)).
		ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	assert.True(t, ran, "an anonymous request must still reach the handler")
	assert.False(t, present, "no actor should be attached")
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestOptionalAttachesActorWhenTokenIsValid(t *testing.T) {
	t.Parallel()

	auth, want := newTestAuthenticator()
	var ran, present bool
	var got domain.Actor

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer good-token")

	auth.Optional(captureActor(&ran, &got, &present)).ServeHTTP(httptest.NewRecorder(), req)

	assert.True(t, present)
	assert.Equal(t, want, got)
}

// TestOptionalStillRejectsAnInvalidToken: treating an expired token as
// "anonymous" would silently show an author the public view of their own
// drafts, with no signal that their session had lapsed.
func TestOptionalStillRejectsAnInvalidToken(t *testing.T) {
	t.Parallel()

	auth, _ := newTestAuthenticator()
	var ran, present bool
	var got domain.Actor

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer expired-token")
	w := httptest.NewRecorder()

	auth.Optional(captureActor(&ran, &got, &present)).ServeHTTP(w, req)

	assert.False(t, ran)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireAdmin(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		actor      *domain.Actor
		wantStatus int
		wantRun    bool
	}{
		"admin passes": {
			actor:      &domain.Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
			wantStatus: http.StatusOK,
			wantRun:    true,
		},
		"ordinary user is forbidden": {
			actor:      &domain.Actor{UserID: uuid.New(), Role: domain.RoleUser},
			wantStatus: http.StatusForbidden,
		},
		"anonymous is unauthorized": {
			actor:      nil,
			wantStatus: http.StatusUnauthorized,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var ran, present bool
			var got domain.Actor

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.actor != nil {
				req = req.WithContext(httpx.WithActor(req.Context(), *tc.actor))
			}
			w := httptest.NewRecorder()

			RequireAdmin(captureActor(&ran, &got, &present)).ServeHTTP(w, req)

			assert.Equal(t, tc.wantStatus, w.Code)
			assert.Equal(t, tc.wantRun, ran)
		})
	}
}

func TestBearerTokenExtraction(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer  padded-token  ")

	token, err := bearerToken(req)

	require.NoError(t, err)
	assert.Equal(t, "padded-token", token)
}
