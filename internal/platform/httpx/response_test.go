package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteDataUsesTheSuccessEnvelope(t *testing.T) {
	t.Parallel()

	ctx := WithRequestID(context.Background(), "req-123")
	w := httptest.NewRecorder()

	WriteData(ctx, w, http.StatusCreated, map[string]string{"id": "abc"})

	assert.Equal(t, http.StatusCreated, w.Code)
	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.JSONEq(t, `{"data":{"id":"abc"},"request_id":"req-123"}`, w.Body.String())
}

func TestWriteListIncludesPaginationMetadata(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	page := PageRequest{Page: 2, Limit: 10}

	WriteList(context.Background(), w, []string{"a", "b"}, NewPagination(page, 25))

	var body struct {
		Data       []string    `json:"data"`
		Pagination *Pagination `json:"pagination"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	assert.Equal(t, []string{"a", "b"}, body.Data)
	require.NotNil(t, body.Pagination)
	assert.Equal(t, 3, body.Pagination.TotalPages)
	assert.True(t, body.Pagination.HasNext)
	assert.True(t, body.Pagination.HasPrev)
}

// TestWriteListRendersEmptySliceAsArray guards a JSON detail clients trip over:
// a nil slice marshals to null, which breaks `for (const p of body.data)`.
func TestWriteListRendersEmptySliceAsArray(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()

	WriteList(context.Background(), w, []string{}, NewPagination(PageRequest{Page: 1, Limit: 10}, 0))

	assert.Contains(t, w.Body.String(), `"data":[]`)
	assert.NotContains(t, w.Body.String(), `"data":null`)
}

func TestWriteErrorRendersTheErrorEnvelope(t *testing.T) {
	t.Parallel()

	ctx := WithRequestID(context.Background(), "req-abc")
	w := httptest.NewRecorder()

	WriteError(ctx, w, apierr.NotFound("post"))

	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.JSONEq(t,
		`{"error":{"code":"not_found","message":"post not found"},"request_id":"req-abc"}`,
		w.Body.String())
}

func TestWriteErrorIncludesFieldErrors(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()

	WriteError(context.Background(), w, apierr.Validation(
		apierr.FieldError{Field: "email", Message: "must be a valid email address"},
	))

	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), `"fields":[{"field":"email"`)
}

// TestWriteErrorNeverLeaksTheInternalCause is the property that makes the whole
// error design worth its extra type.
func TestWriteErrorNeverLeaksTheInternalCause(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	secret := errors.New(`pq: relation "users" does not exist at 10.0.0.9:5432`)

	WriteError(context.Background(), w, apierr.Internal(secret))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, "an internal error occurred")
	assert.NotContains(t, body, "relation")
	assert.NotContains(t, body, "10.0.0.9")
}

func TestWriteErrorClassifiesUnknownErrorsAs500(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()

	WriteError(context.Background(), w, errors.New("nobody classified me"))

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.NotContains(t, w.Body.String(), "nobody classified me")
}

func TestWriteErrorIgnoresNil(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()

	WriteError(context.Background(), w, nil)

	assert.Empty(t, w.Body.String())
}

// TestWriteJSONFallsBackWhenMarshallingFails checks that an unserialisable
// payload still produces a coherent 500 rather than a truncated 200.
func TestWriteJSONFallsBackWhenMarshallingFails(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()

	// NaN cannot be represented in JSON.
	WriteJSON(context.Background(), w, http.StatusOK, map[string]float64{"value": math.NaN()})

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.JSONEq(t,
		`{"error":{"code":"internal_error","message":"an internal error occurred"}}`,
		w.Body.String())
}

func TestWriteNoContent(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()

	WriteNoContent(w)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, w.Body.String())
}

func TestRequestIDAndActorContextHelpers(t *testing.T) {
	t.Parallel()

	assert.Empty(t, RequestID(context.Background()))
	assert.Equal(t, "abc", RequestID(WithRequestID(context.Background(), "abc")))

	_, ok := Actor(context.Background())
	assert.False(t, ok)
	assert.Nil(t, ActorPtr(context.Background()))
}
