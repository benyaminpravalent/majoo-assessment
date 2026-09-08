package comment

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

func TestCreateCommentEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/posts/"+f.publishedPost.ID.String()+"/comments",
		`{"content":"Great write-up"}`, &f.other)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	got := decodeData[commentResponse](t, w)
	assert.Equal(t, "Great write-up", got.Content)
	assert.Equal(t, f.publishedPost.ID, got.PostID)
	assert.Nil(t, got.ParentID)
	assert.Equal(t, "/api/v1/comments/"+got.ID.String(), w.Header().Get("Location"))
}

func TestCreateCommentRequiresAuthentication(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/posts/"+f.publishedPost.ID.String()+"/comments",
		`{"content":"Anonymous"}`, nil)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestCreateCommentValidatesContent(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"missing content": `{}`,
		"empty content":   `{"content":""}`,
		"too long":        `{"content":"` + strings.Repeat("x", 5001) + `"}`,
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newHTTPFixture(t)
			w := f.do(t, http.MethodPost, "/posts/"+f.publishedPost.ID.String()+"/comments", body, &f.other)

			require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
			assert.Equal(t, "validation_failed", errorCode(t, w))
		})
	}
}

func TestCreateCommentRejectsMalformedPostID(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/posts/not-a-uuid/comments", `{"content":"x"}`, &f.other)

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestCreateCommentOnMissingPostReturns404(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodPost, "/posts/"+uuid.New().String()+"/comments",
		`{"content":"x"}`, &f.other)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestCreateReplyEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	parent := f.comment(t, f.other, f.publishedPost.ID, "Parent", nil)

	w := f.do(t, http.MethodPost, "/posts/"+f.publishedPost.ID.String()+"/comments",
		`{"content":"A reply","parent_id":"`+parent.ID.String()+`"}`, &f.author)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	got := decodeData[commentResponse](t, w)
	require.NotNil(t, got.ParentID)
	assert.Equal(t, parent.ID, *got.ParentID)
}

func TestListCommentsEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	for _, body := range []string{"a", "b", "c"} {
		f.comment(t, f.other, f.publishedPost.ID, body, nil)
	}

	w := f.do(t, http.MethodGet, "/posts/"+f.publishedPost.ID.String()+"/comments?limit=2", "", nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var body struct {
		Data       []commentResponse `json:"data"`
		Pagination struct {
			TotalItems int64 `json:"total_items"`
			HasNext    bool  `json:"has_next"`
		} `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body.Data, 2)
	assert.Equal(t, int64(3), body.Pagination.TotalItems)
	assert.True(t, body.Pagination.HasNext)
}

func TestListCommentsOnAnInvisibleDraftReturns404(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)

	w := f.do(t, http.MethodGet, "/posts/"+f.draftPost.ID.String()+"/comments", "", &f.other)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestGetCommentEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Readable", nil)

	w := f.do(t, http.MethodGet, "/comments/"+c.ID.String(), "", nil)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeData[commentResponse](t, w)
	assert.Equal(t, c.ID, got.ID)
	require.NotNil(t, got.Author)
	assert.Equal(t, "commenter", got.Author.Username)
}

func TestUpdateCommentEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Before", nil)

	w := f.do(t, http.MethodPatch, "/comments/"+c.ID.String(), `{"content":"After"}`, &f.other)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "After", decodeData[commentResponse](t, w).Content)
}

func TestUpdateCommentByAnotherUserIsForbidden(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Protected", nil)

	w := f.do(t, http.MethodPatch, "/comments/"+c.ID.String(), `{"content":"Hijacked"}`, &f.author)

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, "forbidden", errorCode(t, w))
}

func TestDeleteCommentEndpoint(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Doomed", nil)

	w := f.do(t, http.MethodDelete, "/comments/"+c.ID.String(), "", &f.other)

	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, http.StatusNotFound, f.do(t, http.MethodGet, "/comments/"+c.ID.String(), "", nil).Code)
}

func TestDeleteCommentByAdminIsAllowed(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Moderated", nil)

	w := f.do(t, http.MethodDelete, "/comments/"+c.ID.String(), "", &f.admin)

	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestDeleteCommentByAnotherUserIsForbidden(t *testing.T) {
	t.Parallel()

	f := newHTTPFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Protected", nil)

	w := f.do(t, http.MethodDelete, "/comments/"+c.ID.String(), "", &f.author)

	assert.Equal(t, http.StatusForbidden, w.Code)
}
