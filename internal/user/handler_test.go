package user

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/validation"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httpFixture wires the real handler over the real service and the in-memory
// fakes, so these tests exercise decoding, validation, the service rules and
// the response envelope together — the parts a unit test of any single layer
// would miss.
type httpFixture struct {
	*fixture
	router chi.Router
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()

	f := newFixture(t)
	h := NewHandler(f.svc, validation.New())

	r := chi.NewRouter()
	r.Group(func(pub chi.Router) {
		h.RegisterCredentialRoutes(pub)
		h.RegisterPublicRoutes(pub)
	})
	// Protected routes are mounted behind a stub that injects whichever actor the
	// test put in the request context, standing in for the auth middleware.
	r.Group(func(protected chi.Router) {
		h.RegisterProtectedRoutes(protected)
	})

	return &httpFixture{fixture: f, router: r}
}

func (f *httpFixture) do(t *testing.T, method, path, body string, actor *domain.Actor) *httptest.ResponseRecorder {
	t.Helper()

	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != nil {
		req = req.WithContext(httpx.WithActor(req.Context(), *actor))
	}

	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	return w
}

// decodeData unwraps the success envelope.
func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var body struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body was: %s", w.Body.String())
	return body.Data
}

// decodeError unwraps the error envelope.
func decodeError(t *testing.T, w *httptest.ResponseRecorder) struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fields  []struct {
		Field   string `json:"field"`
		Message string `json:"message"`
	} `json:"fields"`
} {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Fields  []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
			} `json:"fields"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body was: %s", w.Body.String())
	return body.Error
}

const registerBody = `{
	"email": "ben@example.com",
	"username": "ben_siregar",
	"display_name": "Ben Siregar",
	"password": "a-good-enough-password"
}`

func TestRegisterEndpointReturns201(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/register", registerBody, nil)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	got := decodeData[privateUser](t, w)
	assert.Equal(t, "ben@example.com", got.Email)
	assert.Equal(t, domain.RoleUser, got.Role)
	assert.NotEqual(t, uuid.Nil, got.ID)
}

// TestRegisterEndpointNeverReturnsThePasswordHash is checked on the raw bytes,
// because that is the only way to catch a field added to the DTO later.
func TestRegisterEndpointNeverReturnsThePasswordHash(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/register", registerBody, nil)

	require.Equal(t, http.StatusCreated, w.Code)
	body := w.Body.String()
	assert.NotContains(t, body, "password")
	assert.NotContains(t, body, "$2a$")
	assert.NotContains(t, body, "a-good-enough-password")
}

func TestRegisterEndpointRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body       string
		wantFields []string
	}{
		"missing everything":  {`{}`, []string{"email", "username", "display_name", "password"}},
		"bad email":           {`{"email":"nope","username":"ben_x","display_name":"B","password":"longenough1"}`, []string{"email"}},
		"short password":      {`{"email":"a@b.co","username":"ben_x","display_name":"B","password":"short"}`, []string{"password"}},
		"username with space": {`{"email":"a@b.co","username":"ben x","display_name":"B","password":"longenough1"}`, []string{"username"}},
		"blank display name":  {`{"email":"a@b.co","username":"ben_x","display_name":"","password":"longenough1"}`, []string{"display_name"}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newHTTPFixture(t)
			w := f.do(t, http.MethodPost, "/auth/register", tc.body, nil)

			require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
			errBody := decodeError(t, w)
			assert.Equal(t, "validation_failed", errBody.Code)

			got := make([]string, 0, len(errBody.Fields))
			for _, fe := range errBody.Fields {
				got = append(got, fe.Field)
			}
			for _, want := range tc.wantFields {
				assert.Contains(t, got, want)
			}
		})
	}
}

func TestRegisterEndpointRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/register", `{"email":`, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "bad_request", decodeError(t, w).Code)
}

func TestRegisterEndpointReturns409OnDuplicate(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	require.Equal(t, http.StatusCreated, f.do(t, http.MethodPost, "/auth/register", registerBody, nil).Code)

	w := f.do(t, http.MethodPost, "/auth/register", registerBody, nil)

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, "conflict", decodeError(t, w).Code)
}

func TestLoginEndpointReturnsTokens(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	require.Equal(t, http.StatusCreated, f.do(t, http.MethodPost, "/auth/register", registerBody, nil).Code)

	w := f.do(t, http.MethodPost, "/auth/login",
		`{"email":"ben@example.com","password":"a-good-enough-password"}`, nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeData[tokenResponse](t, w)
	assert.Equal(t, "Bearer", got.TokenType)
	assert.NotEmpty(t, got.AccessToken)
	assert.NotEmpty(t, got.RefreshToken)
	assert.Positive(t, got.ExpiresIn)
}

func TestLoginEndpointReturns401ForBadCredentials(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	require.Equal(t, http.StatusCreated, f.do(t, http.MethodPost, "/auth/register", registerBody, nil).Code)

	for name, body := range map[string]string{
		"wrong password": `{"email":"ben@example.com","password":"wrong-password"}`,
		"unknown email":  `{"email":"nobody@example.com","password":"a-good-enough-password"}`,
	} {
		t.Run(name, func(t *testing.T) {
			w := f.do(t, http.MethodPost, "/auth/login", body, nil)

			require.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Equal(t, "invalid email or password", decodeError(t, w).Message,
				"both failures must be reported identically")
		})
	}
}

func TestRefreshEndpointRotatesTokens(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	require.Equal(t, http.StatusCreated, f.do(t, http.MethodPost, "/auth/register", registerBody, nil).Code)
	login := f.do(t, http.MethodPost, "/auth/login",
		`{"email":"ben@example.com","password":"a-good-enough-password"}`, nil)
	tokens := decodeData[tokenResponse](t, login)

	w := f.do(t, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+tokens.RefreshToken+`"}`, nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	refreshed := decodeData[tokenResponse](t, w)
	assert.NotEqual(t, tokens.RefreshToken, refreshed.RefreshToken)

	replay := f.do(t, http.MethodPost, "/auth/refresh",
		`{"refresh_token":"`+tokens.RefreshToken+`"}`, nil)
	assert.Equal(t, http.StatusUnauthorized, replay.Code, "a rotated token must not work twice")
}

func TestLogoutEndpointReturns204AndIsIdempotent(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	require.Equal(t, http.StatusCreated, f.do(t, http.MethodPost, "/auth/register", registerBody, nil).Code)
	login := f.do(t, http.MethodPost, "/auth/login",
		`{"email":"ben@example.com","password":"a-good-enough-password"}`, nil)
	tokens := decodeData[tokenResponse](t, login)

	body := `{"refresh_token":"` + tokens.RefreshToken + `"}`
	assert.Equal(t, http.StatusNoContent, f.do(t, http.MethodPost, "/auth/logout", body, nil).Code)
	assert.Equal(t, http.StatusNoContent, f.do(t, http.MethodPost, "/auth/logout", body, nil).Code)
}

func TestMeEndpointRequiresAnActor(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	// The route is normally behind Require, but the handler must not assume it:
	// a routing mistake should fail closed with a 401, not panic on a zero actor.
	w := f.do(t, http.MethodGet, "/users/me", "", nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMeEndpointReturnsThePrivateProjection(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := decodeData[privateUser](t, f.do(t, http.MethodPost, "/auth/register", registerBody, nil))

	w := f.do(t, http.MethodGet, "/users/me", "",
		&domain.Actor{UserID: created.ID, Role: domain.RoleUser})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeData[privateUser](t, w)
	assert.Equal(t, created.ID, got.ID)
	assert.Equal(t, "ben@example.com", got.Email, "the owner may see their own address")
}

// TestGetUserByIDReturnsThePublicProjection: another user's email address is
// not the caller's business, authenticated or not.
func TestGetUserByIDReturnsThePublicProjection(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := decodeData[privateUser](t, f.do(t, http.MethodPost, "/auth/register", registerBody, nil))

	w := f.do(t, http.MethodGet, "/users/"+created.ID.String(), "", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "ben@example.com")
	assert.NotContains(t, w.Body.String(), `"role"`)
	got := decodeData[publicUser](t, w)
	assert.Equal(t, "ben_siregar", got.Username)
}

func TestGetUserByIDRejectsAMalformedID(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodGet, "/users/not-a-uuid", "", nil)

	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	errBody := decodeError(t, w)
	require.NotEmpty(t, errBody.Fields)
	assert.Equal(t, "userID", errBody.Fields[0].Field)
}

func TestGetUserByIDReturns404ForUnknownUser(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodGet, "/users/"+uuid.New().String(), "", nil)

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "not_found", decodeError(t, w).Code)
}

func TestUpdateMeEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := decodeData[privateUser](t, f.do(t, http.MethodPost, "/auth/register", registerBody, nil))
	actor := &domain.Actor{UserID: created.ID, Role: domain.RoleUser}

	w := f.do(t, http.MethodPatch, "/users/me", `{"display_name":"Renamed"}`, actor)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "Renamed", decodeData[privateUser](t, w).DisplayName)
}

func TestUpdateMeEndpointValidatesInput(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := decodeData[privateUser](t, f.do(t, http.MethodPost, "/auth/register", registerBody, nil))
	actor := &domain.Actor{UserID: created.ID, Role: domain.RoleUser}

	w := f.do(t, http.MethodPatch, "/users/me", `{"display_name":""}`, actor)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

// TestUnknownFieldIsRejected keeps a client typo from silently doing nothing.
func TestUnknownFieldIsRejected(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/login",
		`{"email":"a@b.co","password":"secret","remember_me":true}`, nil)

	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	errBody := decodeError(t, w)
	require.NotEmpty(t, errBody.Fields)
	assert.Equal(t, "remember_me", errBody.Fields[0].Field)
}

func TestLogoutAllEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := decodeData[privateUser](t, f.do(t, http.MethodPost, "/auth/register", registerBody, nil))
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, f.do(t, http.MethodPost, "/auth/login",
			`{"email":"ben@example.com","password":"a-good-enough-password"}`, nil).Code)
	}

	w := f.do(t, http.MethodPost, "/auth/logout-all", "",
		&domain.Actor{UserID: created.ID, Role: domain.RoleUser})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, int64(2), decodeData[logoutAllResponse](t, w).SessionsRevoked)
}

// The session endpoints' rejection paths. The brief asks specifically for
// negative coverage of malformed authentication and invalid input, and these
// are the paths a client hits when its stored token is truncated or absent.

func TestRefreshEndpointRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/refresh", `{"refresh_token":`, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.NotEmpty(t, decodeError(t, w).Code)
}

func TestRefreshEndpointRejectsAnUnusableToken(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/refresh", `{"refresh_token":"tiny"}`, nil)

	// A token too short to be one of ours is rejected before any database work,
	// so a flood of junk costs nothing.
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "refresh_token", decodeError(t, w).Fields[0].Field)
}

func TestLogoutEndpointRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/logout", `not json`, nil)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLogoutEndpointRejectsAnUnusableToken(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/logout", `{"refresh_token":"tiny"}`, nil)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, "refresh_token", decodeError(t, w).Fields[0].Field)
}

// LogoutAll and UpdateMe read the actor from the context. If the middleware were
// ever unmounted the handler must refuse rather than act on a zero-valued actor
// — which would be "log out user 00000000-...".
func TestLogoutAllEndpointRequiresAnActor(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/auth/logout-all", "", nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestUpdateMeEndpointRequiresAnActor(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPatch, "/users/me", `{"display_name":"Ben"}`, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestUpdateMeEndpointRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	actor := &domain.Actor{UserID: uuid.New(), Role: domain.RoleUser}

	w := f.do(t, http.MethodPatch, "/users/me", `{"display_name":`, actor)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}
