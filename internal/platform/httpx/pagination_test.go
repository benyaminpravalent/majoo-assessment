package httpx

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePageRequestDefaults(t *testing.T) {
	t.Parallel()

	got, err := ParsePageRequest(httptest.NewRequest(http.MethodGet, "/posts", nil))

	require.NoError(t, err)
	assert.Equal(t, PageRequest{Page: 1, Limit: DefaultPageSize}, got)
	assert.Equal(t, 0, got.Offset())
}

func TestParsePageRequestReadsValidValues(t *testing.T) {
	t.Parallel()

	got, err := ParsePageRequest(httptest.NewRequest(http.MethodGet, "/posts?page=3&limit=25", nil))

	require.NoError(t, err)
	assert.Equal(t, 3, got.Page)
	assert.Equal(t, 25, got.Limit)
	assert.Equal(t, 50, got.Offset(), "offset must skip the two preceding pages")
}

func TestParsePageRequestAcceptsMaxLimit(t *testing.T) {
	t.Parallel()

	got, err := ParsePageRequest(httptest.NewRequest(http.MethodGet,
		"/posts?limit="+strconv.Itoa(MaxPageSize), nil))

	require.NoError(t, err)
	assert.Equal(t, MaxPageSize, got.Limit)
}

// TestParsePageRequestRejectsInvalidValues covers the pagination edge cases the
// assessment calls out. Note that these are errors rather than silent
// corrections: a client sending limit=abc has a bug, and quietly returning the
// default hides it.
func TestParsePageRequestRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		query      string
		wantFields []string
	}{
		"page zero":          {"page=0", []string{"page"}},
		"page negative":      {"page=-1", []string{"page"}},
		"page not a number":  {"page=abc", []string{"page"}},
		"page overflows int": {"page=99999999999999999999", []string{"page"}},
		"limit zero":         {"limit=0", []string{"limit"}},
		"limit negative":     {"limit=-5", []string{"limit"}},
		"limit not a number": {"limit=many", []string{"limit"}},
		"limit above max":    {"limit=" + strconv.Itoa(MaxPageSize+1), []string{"limit"}},
		"both invalid":       {"page=0&limit=0", []string{"page", "limit"}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := ParsePageRequest(httptest.NewRequest(http.MethodGet, "/posts?"+tc.query, nil))

			require.Error(t, err)
			appErr := apierr.From(err)
			assert.Equal(t, apierr.CodeValidation, appErr.Code)
			assert.Equal(t, http.StatusUnprocessableEntity, appErr.Status)

			gotFields := make([]string, 0, len(appErr.Fields))
			for _, f := range appErr.Fields {
				gotFields = append(gotFields, f.Field)
			}
			assert.ElementsMatch(t, tc.wantFields, gotFields,
				"the response must name every invalid field, not just the first")
		})
	}
}

func TestParsePageRequestTreatsEmptyParameterAsAbsent(t *testing.T) {
	t.Parallel()

	got, err := ParsePageRequest(httptest.NewRequest(http.MethodGet, "/posts?page=&limit=", nil))

	require.NoError(t, err)
	assert.Equal(t, PageRequest{Page: 1, Limit: DefaultPageSize}, got)
}

func TestNewPagination(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		req   PageRequest
		total int64
		want  Pagination
	}{
		"first page of three": {
			req:   PageRequest{Page: 1, Limit: 10},
			total: 25,
			want:  Pagination{Page: 1, Limit: 10, TotalItems: 25, TotalPages: 3, HasNext: true, HasPrev: false},
		},
		"middle page": {
			req:   PageRequest{Page: 2, Limit: 10},
			total: 25,
			want:  Pagination{Page: 2, Limit: 10, TotalItems: 25, TotalPages: 3, HasNext: true, HasPrev: true},
		},
		"last page": {
			req:   PageRequest{Page: 3, Limit: 10},
			total: 25,
			want:  Pagination{Page: 3, Limit: 10, TotalItems: 25, TotalPages: 3, HasNext: false, HasPrev: true},
		},
		"exactly one full page": {
			req:   PageRequest{Page: 1, Limit: 10},
			total: 10,
			want:  Pagination{Page: 1, Limit: 10, TotalItems: 10, TotalPages: 1, HasNext: false, HasPrev: false},
		},
		"no results": {
			req:   PageRequest{Page: 1, Limit: 10},
			total: 0,
			want:  Pagination{Page: 1, Limit: 10, TotalItems: 0, TotalPages: 0, HasNext: false, HasPrev: false},
		},
		"page beyond the end": {
			req:   PageRequest{Page: 9, Limit: 10},
			total: 25,
			want:  Pagination{Page: 9, Limit: 10, TotalItems: 25, TotalPages: 3, HasNext: false, HasPrev: true},
		},
		"no results on a deep page reports no previous": {
			req:   PageRequest{Page: 5, Limit: 10},
			total: 0,
			want:  Pagination{Page: 5, Limit: 10, TotalItems: 0, TotalPages: 0, HasNext: false, HasPrev: false},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, NewPagination(tc.req, tc.total))
		})
	}
}

func TestPageRequestOffset(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 0, PageRequest{Page: 1, Limit: 20}.Offset())
	assert.Equal(t, 20, PageRequest{Page: 2, Limit: 20}.Offset())
	assert.Equal(t, 990, PageRequest{Page: 100, Limit: 10}.Offset())
}
