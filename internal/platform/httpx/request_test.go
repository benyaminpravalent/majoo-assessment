package httpx

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type sample struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func jsonRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestDecodeJSONHappyPath(t *testing.T) {
	t.Parallel()

	var got sample
	err := DecodeJSON(jsonRequest(`{"name":"widget","count":3}`), &got)

	require.NoError(t, err)
	assert.Equal(t, sample{Name: "widget", Count: 3}, got)
}

func TestDecodeJSONAcceptsContentTypeWithCharset(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"x"}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	var got sample
	assert.NoError(t, DecodeJSON(r, &got))
}

func TestDecodeJSONRejectsWrongContentType(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"x"}`))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var got sample
	err := DecodeJSON(r, &got)

	requireCode(t, err, apierr.CodeUnsupportedType, http.StatusUnsupportedMediaType)
}

// TestDecodeJSONRejectsUnknownFields catches client typos loudly. Silently
// ignoring an unknown key means a caller who sends "displayname" instead of
// "display_name" gets a 201 and wonders why nothing changed.
func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	var got sample
	err := DecodeJSON(jsonRequest(`{"name":"x","nmae":"typo"}`), &got)

	appErr := requireCode(t, err, apierr.CodeValidation, http.StatusUnprocessableEntity)
	require.Len(t, appErr.Fields, 1)
	assert.Equal(t, "nmae", appErr.Fields[0].Field)
	assert.Equal(t, "unknown field", appErr.Fields[0].Message)
}

func TestDecodeJSONReportsTypeMismatchAsFieldError(t *testing.T) {
	t.Parallel()

	var got sample
	err := DecodeJSON(jsonRequest(`{"name":"x","count":"three"}`), &got)

	appErr := requireCode(t, err, apierr.CodeValidation, http.StatusUnprocessableEntity)
	require.Len(t, appErr.Fields, 1)
	assert.Equal(t, "count", appErr.Fields[0].Field)
}

func TestDecodeJSONRejectsEmptyBody(t *testing.T) {
	t.Parallel()

	var got sample
	err := DecodeJSON(jsonRequest(``), &got)

	appErr := requireCode(t, err, apierr.CodeBadRequest, http.StatusBadRequest)
	assert.Contains(t, appErr.Message, "must not be empty")
}

func TestDecodeJSONRejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"unclosed object": `{"name":"x"`,
		"trailing comma":  `{"name":"x",}`,
		"bare word":       `not json`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var got sample
			err := DecodeJSON(jsonRequest(body), &got)
			require.Error(t, err)
			assert.Equal(t, http.StatusBadRequest, apierr.From(err).Status)
		})
	}
}

// TestDecodeJSONRejectsTrailingContent stops a request body carrying two
// concatenated objects from being half-applied.
func TestDecodeJSONRejectsTrailingContent(t *testing.T) {
	t.Parallel()

	var got sample
	err := DecodeJSON(jsonRequest(`{"name":"first"}{"name":"second"}`), &got)

	appErr := requireCode(t, err, apierr.CodeBadRequest, http.StatusBadRequest)
	assert.Contains(t, appErr.Message, "exactly one JSON object")
}

// TestDecodeJSONMapsBodyLimitTo413 checks the interaction with the BodyLimit
// middleware: an over-sized body must be a 413, not a generic 400.
func TestDecodeJSONMapsBodyLimitTo413(t *testing.T) {
	t.Parallel()

	oversized := `{"name":"` + strings.Repeat("a", 500) + `"}`
	r := jsonRequest(oversized)
	w := httptest.NewRecorder()
	r.Body = http.MaxBytesReader(w, r.Body, 64)

	var got sample
	err := DecodeJSON(r, &got)

	appErr := requireCode(t, err, apierr.CodePayloadTooLarge, http.StatusRequestEntityTooLarge)
	assert.Contains(t, appErr.Message, "64 bytes")
}

func TestDecodeJSONTreatsNilDestinationAsServerBug(t *testing.T) {
	t.Parallel()

	// Passing a non-pointer is a programming error and must surface as 500, so
	// it shows up in alerting rather than being blamed on the caller.
	err := DecodeJSON(jsonRequest(`{"name":"x"}`), sample{})

	requireCode(t, err, apierr.CodeInternal, http.StatusInternalServerError)
}

func TestDecodeJSONAllowsMissingContentTypeHeader(t *testing.T) {
	t.Parallel()

	// Some clients omit the header on a body-bearing request. Rejecting those
	// would break curl one-liners for no security benefit, since the body is
	// still parsed strictly as JSON.
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"name":"x"}`))
	r.Header.Del("Content-Type")

	var got sample
	assert.NoError(t, DecodeJSON(r, &got))
}

func requireCode(t *testing.T, err error, code apierr.Code, status int) *apierr.Error {
	t.Helper()
	require.Error(t, err)
	appErr := apierr.From(err)
	require.NotNil(t, appErr)
	assert.Equal(t, code, appErr.Code)
	assert.Equal(t, status, appErr.Status)
	return appErr
}
