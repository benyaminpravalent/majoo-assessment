package httpx

import (
	"net/http"
	"strconv"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
)

// Pagination bounds for list endpoints.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
)

// PageRequest is a validated page/limit pair.
//
// This API uses offset pagination deliberately. A blog's list endpoints are
// bounded (tens of thousands of posts, not billions of feed rows) and reviewers
// and UIs both benefit from a total count and jump-to-page. The cost — OFFSET
// scanning grows linearly with page depth, and concurrent inserts can shift
// rows between pages — is documented in the README, and the social-media
// assessment in docs/social-media-database-design.md shows the keyset design
// used when that cost stops being acceptable.
type PageRequest struct {
	Page  int
	Limit int
}

// Offset returns the SQL OFFSET for the requested page.
func (p PageRequest) Offset() int { return (p.Page - 1) * p.Limit }

// Pagination is the metadata returned alongside a page of results.
type Pagination struct {
	Page       int   `json:"page"`
	Limit      int   `json:"limit"`
	TotalItems int64 `json:"total_items"`
	TotalPages int   `json:"total_pages"`
	HasNext    bool  `json:"has_next"`
	HasPrev    bool  `json:"has_prev"`
}

// NewPagination computes the response metadata for a page of total items.
func NewPagination(req PageRequest, total int64) Pagination {
	totalPages := 0
	if total > 0 {
		totalPages = int((total + int64(req.Limit) - 1) / int64(req.Limit))
	}
	return Pagination{
		Page:       req.Page,
		Limit:      req.Limit,
		TotalItems: total,
		TotalPages: totalPages,
		HasNext:    req.Page < totalPages,
		HasPrev:    req.Page > 1 && total > 0,
	}
}

// ParsePageRequest reads page and limit from the query string.
//
// Absent parameters take defaults. Present-but-invalid parameters are an error
// rather than silently corrected: a client sending limit=abc has a bug, and
// quietly returning 20 rows hides it.
func ParsePageRequest(r *http.Request) (PageRequest, error) {
	q := r.URL.Query()
	req := PageRequest{Page: 1, Limit: DefaultPageSize}
	var fields []apierr.FieldError

	if raw := q.Get("page"); raw != "" {
		v, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			fields = append(fields, apierr.FieldError{Field: "page", Message: "must be an integer"})
		case v < 1:
			fields = append(fields, apierr.FieldError{Field: "page", Message: "must be greater than or equal to 1"})
		default:
			req.Page = v
		}
	}

	if raw := q.Get("limit"); raw != "" {
		v, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			fields = append(fields, apierr.FieldError{Field: "limit", Message: "must be an integer"})
		case v < 1:
			fields = append(fields, apierr.FieldError{Field: "limit", Message: "must be greater than or equal to 1"})
		case v > MaxPageSize:
			// An upper bound is a availability control, not a nicety: without it
			// one client can ask for a million rows and exhaust the pool.
			fields = append(fields, apierr.FieldError{
				Field:   "limit",
				Message: "must be less than or equal to " + strconv.Itoa(MaxPageSize),
			})
		default:
			req.Limit = v
		}
	}

	if len(fields) > 0 {
		return PageRequest{}, apierr.Validation(fields...)
	}
	return req, nil
}
