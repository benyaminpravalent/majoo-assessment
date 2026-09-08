// Package post owns blog articles: storage, business rules and HTTP transport.
package post

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// constraintSlugUnique is the partial unique index on live posts.
const constraintSlugUnique = "posts_slug_active_unique"

// ErrSlugTaken reports that a generated slug collides with a live post. The
// service catches it and retries with a suffix, so it normally never reaches a
// client.
//
// It is a plain sentinel wrapped inside the *apierr.Error rather than being one
// itself: apierr constructors return fresh values, so errors.Is against an
// *apierr.Error would compare two different pointers and always be false.
var ErrSlugTaken = errors.New("post: slug already in use")

// slugConflict is the client-facing error carrying ErrSlugTaken as its cause.
func slugConflict(cause error) error {
	return apierr.Conflict("a post with this slug already exists").
		WithCause(fmt.Errorf("%w: %w", ErrSlugTaken, cause))
}

// Repository reads and writes posts.
//
// It holds a Store rather than an Executor because Delete spans two statements
// that must commit together.
type Repository struct {
	db postgres.Store
}

// NewRepository returns a Repository backed by db.
func NewRepository(db postgres.Store) *Repository {
	return &Repository{db: db}
}

// selectColumns is the projection shared by every read. The author is joined in
// rather than fetched separately: a list of 20 posts would otherwise be 21
// queries, and the join is a primary-key lookup per row.
const selectColumns = `
	p.id, p.author_id, p.title, p.slug, p.content, p.status,
	p.comment_count, p.published_at, p.created_at, p.updated_at,
	u.id, u.username, u.display_name`

const fromClause = `
	FROM posts p
	JOIN users u ON u.id = p.author_id`

// Create inserts a post.
func (r *Repository) Create(ctx context.Context, p *domain.Post) error {
	const q = `
		INSERT INTO posts (id, author_id, title, slug, content, status, published_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING comment_count, created_at, updated_at`

	err := r.db.QueryRow(ctx, q, p.ID, p.AuthorID, p.Title, p.Slug, p.Content, p.Status, p.PublishedAt).
		Scan(&p.CommentCount, &p.CreatedAt, &p.UpdatedAt)
	if err == nil {
		return nil
	}

	switch {
	case postgres.IsUniqueViolation(err, constraintSlugUnique):
		return slugConflict(err)
	case postgres.IsForeignKeyViolation(err, ""):
		// The author was deleted between authentication and this insert.
		return apierr.Unauthorized("the authenticated account no longer exists").WithCause(err)
	default:
		return apierr.Internal(fmt.Errorf("insert post: %w", err))
	}
}

// ByID returns a live post with its author.
func (r *Repository) ByID(ctx context.Context, id uuid.UUID) (domain.Post, error) {
	q := `SELECT ` + selectColumns + fromClause + `
		WHERE p.id = $1 AND p.deleted_at IS NULL`

	p, err := scanPost(r.db.QueryRow(ctx, q, id))
	if err != nil {
		return domain.Post{}, postgres.NotFoundOr(fmt.Errorf("select post: %w", err), "post")
	}
	return p, nil
}

// Exists reports whether a live post with the given id exists, without paying
// for the full row. Used by the comment service to validate its parent.
func (r *Repository) Exists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM posts WHERE id = $1 AND deleted_at IS NULL)`, id).Scan(&exists)
	if err != nil {
		return false, apierr.Internal(fmt.Errorf("check post existence: %w", err))
	}
	return exists, nil
}

// AuthorOf returns the author and status of a live post. It is the minimal read
// needed for an authorisation decision, so update and delete do not have to
// fetch and discard the whole body.
func (r *Repository) AuthorOf(ctx context.Context, id uuid.UUID) (authorID uuid.UUID, status domain.PostStatus, err error) {
	err = r.db.QueryRow(ctx,
		`SELECT author_id, status FROM posts WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&authorID, &status)
	if err != nil {
		return uuid.Nil, "", postgres.NotFoundOr(fmt.Errorf("select post owner: %w", err), "post")
	}
	return authorID, status, nil
}

// ListFilter describes one page of a post listing.
type ListFilter struct {
	AuthorID *uuid.UUID
	Status   *domain.PostStatus
	Search   string
	Page     httpx.PageRequest
}

// List returns one page of posts and the total number of matches.
//
// The total comes from a COUNT(*) OVER () window in the same statement rather
// than a second query. That costs one extra pass over the matched rows, and
// buys two things: one round trip instead of two, and a count that is
// guaranteed consistent with the page — a separate COUNT can be taken against a
// different snapshot and report a total that contradicts the rows returned.
func (r *Repository) List(ctx context.Context, f ListFilter) ([]domain.Post, int64, error) {
	var (
		args   []any
		where  = []string{"p.deleted_at IS NULL"}
		order  = "p.created_at DESC, p.id DESC"
		search = strings.TrimSpace(f.Search)
	)

	// Placeholders are always $n over a positional args slice. No filter value
	// is ever interpolated into the SQL text, which is what makes this safe from
	// injection regardless of what a client sends.
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	if f.AuthorID != nil {
		where = append(where, "p.author_id = "+next(*f.AuthorID))
	}
	if f.Status != nil {
		where = append(where, "p.status = "+next(string(*f.Status)))
	}
	if search != "" {
		// websearch_to_tsquery accepts human input ("go AND -rust", quoted
		// phrases) without throwing on syntax the way to_tsquery does, so a
		// stray operator in a search box cannot produce a 500.
		ph := next(search)
		where = append(where, "p.search_vector @@ websearch_to_tsquery('english', "+ph+")")
		// With a search term, relevance is a better primary sort than recency;
		// created_at and id stay as tiebreakers so the ordering remains total.
		order = "ts_rank_cd(p.search_vector, websearch_to_tsquery('english', " + ph + ")) DESC, " + order
	}

	limitPh := next(f.Page.Limit)
	offsetPh := next(f.Page.Offset())

	q := `SELECT ` + selectColumns + `, COUNT(*) OVER () AS total_count` + fromClause + `
		WHERE ` + strings.Join(where, "\n\t\t  AND ") + `
		ORDER BY ` + order + `
		LIMIT ` + limitPh + ` OFFSET ` + offsetPh

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, 0, apierr.Internal(fmt.Errorf("list posts: %w", err))
	}
	defer rows.Close()

	// Non-nil so an empty page serialises as [] rather than null.
	posts := make([]domain.Post, 0, f.Page.Limit)
	var total int64
	for rows.Next() {
		var (
			p      domain.Post
			author domain.UserSummary
		)
		err := rows.Scan(
			&p.ID, &p.AuthorID, &p.Title, &p.Slug, &p.Content, &p.Status,
			&p.CommentCount, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt,
			&author.ID, &author.Username, &author.DisplayName,
			&total,
		)
		if err != nil {
			return nil, 0, apierr.Internal(fmt.Errorf("scan post row: %w", err))
		}
		p.Author = &author
		posts = append(posts, p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, apierr.Internal(fmt.Errorf("iterate post rows: %w", err))
	}

	return posts, total, nil
}

// UpdateInput carries the fields a caller may change. A nil pointer means
// "leave unchanged", which is what distinguishes PATCH from PUT and lets a
// client clear nothing by accident.
type UpdateInput struct {
	Title   *string
	Slug    *string
	Content *string
	Status  *domain.PostStatus
	// PublishedAt is set by the service when a draft transitions to published.
	PublishedAt *time.Time
	// ClearPublishedAt unpublishes, which the posts_published_at_consistent
	// constraint requires when moving back to draft.
	ClearPublishedAt bool
}

// Update applies a partial change and returns the updated post.
//
// COALESCE is not used here. It cannot express "set this column to NULL", and
// it hides which columns a statement actually touches. Building the SET list
// from the non-nil fields keeps both explicit.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (domain.Post, error) {
	var (
		sets []string
		args []any
	)
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	if in.Title != nil {
		sets = append(sets, "title = "+next(*in.Title))
	}
	if in.Slug != nil {
		sets = append(sets, "slug = "+next(*in.Slug))
	}
	if in.Content != nil {
		sets = append(sets, "content = "+next(*in.Content))
	}
	if in.Status != nil {
		sets = append(sets, "status = "+next(string(*in.Status)))
	}
	switch {
	case in.ClearPublishedAt:
		sets = append(sets, "published_at = NULL")
	case in.PublishedAt != nil:
		sets = append(sets, "published_at = "+next(*in.PublishedAt))
	}

	if len(sets) == 0 {
		// Nothing to change. Returning the current row keeps PATCH idempotent
		// and avoids issuing an UPDATE with an empty SET, which is a syntax error.
		return r.ByID(ctx, id)
	}

	idPh := next(id)
	q := `
		WITH updated AS (
			UPDATE posts
			   SET ` + strings.Join(sets, ",\n\t\t\t       ") + `
			 WHERE id = ` + idPh + ` AND deleted_at IS NULL
			RETURNING *
		)
		SELECT ` + selectColumns + `
		  FROM updated p
		  JOIN users u ON u.id = p.author_id`

	p, err := scanPost(r.db.QueryRow(ctx, q, args...))
	if err != nil {
		if postgres.IsUniqueViolation(err, constraintSlugUnique) {
			return domain.Post{}, slugConflict(err)
		}
		return domain.Post{}, postgres.NotFoundOr(fmt.Errorf("update post: %w", err), "post")
	}
	return p, nil
}

// Delete soft-deletes a post and every comment on it, atomically.
//
// Both statements must land together: a post marked deleted while its comments
// stay live would leave orphaned rows visible to the comment endpoints, and
// comments deleted without the post would empty a thread that still renders.
// This is the clearest transaction boundary in the service.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	return postgres.InTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE posts SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
		if err != nil {
			return apierr.Internal(fmt.Errorf("soft-delete post: %w", err))
		}
		if tag.RowsAffected() == 0 {
			// Already deleted, or never existed. Rolling back here is what makes
			// a double delete return 404 rather than silently succeeding.
			return apierr.NotFound("post")
		}

		if _, err := tx.Exec(ctx,
			`UPDATE comments SET deleted_at = now() WHERE post_id = $1 AND deleted_at IS NULL`, id); err != nil {
			return apierr.Internal(fmt.Errorf("soft-delete comments of post: %w", err))
		}
		return nil
	})
}

// SlugExists reports whether a live post already uses the slug.
func (r *Repository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM posts WHERE slug = $1 AND deleted_at IS NULL)`, slug).Scan(&exists)
	if err != nil {
		return false, apierr.Internal(fmt.Errorf("check slug availability: %w", err))
	}
	return exists, nil
}

func scanPost(row pgx.Row) (domain.Post, error) {
	var (
		p      domain.Post
		author domain.UserSummary
	)
	err := row.Scan(
		&p.ID, &p.AuthorID, &p.Title, &p.Slug, &p.Content, &p.Status,
		&p.CommentCount, &p.PublishedAt, &p.CreatedAt, &p.UpdatedAt,
		&author.ID, &author.Username, &author.DisplayName,
	)
	if err != nil {
		return domain.Post{}, err
	}
	p.Author = &author
	return p, nil
}
