package post

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

type httpFixture struct {
	*fixture
	router chi.Router
}

func newHTTPFixture(t *testing.T) *httpFixture {
	t.Helper()

	f := newFixture(t)
	h := NewHandler(f.svc, validation.New())

	r := chi.NewRouter()
	h.RegisterPublicRoutes(r)
	h.RegisterProtectedRoutes(r)

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

func decodeData[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var body struct {
		Data T `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body was: %s", w.Body.String())
	return body.Data
}

func decodeList(t *testing.T, w *httptest.ResponseRecorder) ([]postResponse, httpx.Pagination) {
	t.Helper()
	var body struct {
		Data       []postResponse   `json:"data"`
		Pagination httpx.Pagination `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body was: %s", w.Body.String())
	return body.Data, body.Pagination
}

func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), "body was: %s", w.Body.String())
	return body.Error.Code
}

func TestCreatePostEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/posts",
		`{"title":"A New Post","content":"Some content","status":"published"}`, &f.author)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	got := decodeData[postResponse](t, w)
	assert.Equal(t, "A New Post", got.Title)
	assert.Equal(t, domain.PostStatusPublished, got.Status)
	assert.NotNil(t, got.PublishedAt)
	assert.Equal(t, "/api/v1/posts/"+got.ID.String(), w.Header().Get("Location"),
		"a 201 should tell the client where the resource lives")
}

func TestCreatePostDefaultsToDraft(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/posts", `{"title":"Quiet Post","content":"body"}`, &f.author)

	require.Equal(t, http.StatusCreated, w.Code)
	got := decodeData[postResponse](t, w)
	assert.Equal(t, domain.PostStatusDraft, got.Status,
		"omitting status must not publish by accident")
	assert.Nil(t, got.PublishedAt)
}

func TestCreatePostRequiresAuthentication(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/posts", `{"title":"Anonymous","content":"body"}`, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreatePostValidatesInput(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"missing title":   `{"content":"body"}`,
		"missing content": `{"title":"A Title"}`,
		"short title":     `{"title":"ab","content":"body"}`,
		"empty content":   `{"title":"A Title","content":""}`,
		"unknown status":  `{"title":"A Title","content":"body","status":"archived"}`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newHTTPFixture(t)
			w := f.do(t, http.MethodPost, "/posts", body, &f.author)

			require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
			assert.Equal(t, "validation_failed", errorCode(t, w))
		})
	}
}

func TestGetPostEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Readable", domain.PostStatusPublished)

	w := f.do(t, http.MethodGet, "/posts/"+created.ID.String(), "", nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeData[postResponse](t, w)
	assert.Equal(t, created.ID, got.ID)
	require.NotNil(t, got.Author, "the author must be embedded so clients need no second call")
	assert.Equal(t, "author", got.Author.Username)
}

func TestGetPostRejectsMalformedID(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodGet, "/posts/not-a-uuid", "", nil)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestGetPostReturns404ForUnknownID(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodGet, "/posts/"+uuid.New().String(), "", nil)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetDraftReturns404ToStrangersAnd200ToItsAuthor(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	draft := f.create(t, f.author, "Private Draft", domain.PostStatusDraft)

	anonymous := f.do(t, http.MethodGet, "/posts/"+draft.ID.String(), "", nil)
	assert.Equal(t, http.StatusNotFound, anonymous.Code)

	stranger := f.do(t, http.MethodGet, "/posts/"+draft.ID.String(), "", &f.other)
	assert.Equal(t, http.StatusNotFound, stranger.Code)

	owner := f.do(t, http.MethodGet, "/posts/"+draft.ID.String(), "", &f.author)
	assert.Equal(t, http.StatusOK, owner.Code)
}

func TestListPostsEndpointReturnsPaginationMetadata(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	for _, title := range []string{"Alpha", "Bravo", "Charlie"} {
		f.create(t, f.author, title, domain.PostStatusPublished)
	}

	w := f.do(t, http.MethodGet, "/posts?page=1&limit=2", "", nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	items, pagination := decodeList(t, w)
	assert.Len(t, items, 2)
	assert.Equal(t, int64(3), pagination.TotalItems)
	assert.Equal(t, 2, pagination.TotalPages)
	assert.True(t, pagination.HasNext)
	assert.False(t, pagination.HasPrev)
}

func TestListPostsReturnsAnEmptyArrayNotNull(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodGet, "/posts", "", nil)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"data":[]`)
}

func TestListPostsValidatesQueryParameters(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"page zero":       "/posts?page=0",
		"limit too large": "/posts?limit=1000",
		"bad author id":   "/posts?author_id=nope",
		"bad status":      "/posts?status=archived",
		"search too long": "/posts?q=" + strings.Repeat("a", 201),
	}

	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newHTTPFixture(t)
			w := f.do(t, http.MethodGet, path, "", nil)

			require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
			assert.Equal(t, "validation_failed", errorCode(t, w))
		})
	}
}

func TestListPostsRefusesToLeakOtherAuthorsDrafts(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	f.create(t, f.author, "Hidden", domain.PostStatusDraft)

	w := f.do(t, http.MethodGet, "/posts?status=draft&author_id="+f.author.UserID.String(), "", &f.other)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdatePostEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Before", domain.PostStatusDraft)

	w := f.do(t, http.MethodPatch, "/posts/"+created.ID.String(),
		`{"title":"After Update"}`, &f.author)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "After Update", decodeData[postResponse](t, w).Title)
}

// TestUpdatePostRequiresAtLeastOneChange stops an empty PATCH from being a
// silent no-op that a client mistakes for success.
func TestUpdatePostRequiresAtLeastOneChange(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Unchanged", domain.PostStatusDraft)

	w := f.do(t, http.MethodPatch, "/posts/"+created.ID.String(), `{}`, &f.author)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdatePostByAnotherUserIsForbidden(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Protected", domain.PostStatusPublished)

	w := f.do(t, http.MethodPatch, "/posts/"+created.ID.String(),
		`{"title":"Hijacked Title"}`, &f.other)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "forbidden", errorCode(t, w))
}

func TestUpdatePostRequiresAuthentication(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Protected", domain.PostStatusPublished)

	w := f.do(t, http.MethodPatch, "/posts/"+created.ID.String(), `{"title":"Anonymous Edit"}`, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestDeletePostEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Doomed", domain.PostStatusPublished)

	w := f.do(t, http.MethodDelete, "/posts/"+created.ID.String(), "", &f.author)

	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Body.String())

	after := f.do(t, http.MethodGet, "/posts/"+created.ID.String(), "", nil)
	assert.Equal(t, http.StatusNotFound, after.Code)
}

func TestDeletePostByAnotherUserIsForbidden(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Protected", domain.PostStatusPublished)

	w := f.do(t, http.MethodDelete, "/posts/"+created.ID.String(), "", &f.other)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeletePostByAdminIsAllowed(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	created := f.create(t, f.author, "Moderated", domain.PostStatusPublished)

	w := f.do(t, http.MethodDelete, "/posts/"+created.ID.String(), "", &f.admin)

	assert.Equal(t, http.StatusNoContent, w.Code)
}
