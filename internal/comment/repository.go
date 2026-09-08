// Package comment owns replies to posts.
//
// Its most interesting property is that two of its three write paths are
// transactional: creating and deleting a comment must also adjust the
// denormalised posts.comment_count. That counter is the reason the transaction
// exists, and the reason a CHECK (comment_count >= 0) sits on the column.
package comment

import (
	"context"
	"errors"
	"fmt"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Constraint names from migrations/0001_init.up.sql.
const (
	constraintParentSamePost = "comments_parent_same_post"
	constraintPostFK         = "comments_post_id_fkey"
)

// ErrPostNotFound reports that the target post does not exist or is deleted.
// The service turns it into a 404 naming the post rather than the comment.
var ErrPostNotFound = errors.New("comment: post not found")

// Repository reads and writes comments.
//
// It holds a Store because Create and Delete each span two statements that must
// commit together.
type Repository struct {
	db postgres.Store
}

// NewRepository returns a Repository backed by db.
func NewRepository(db postgres.Store) *Repository {
	return &Repository{db: db}
}

const selectColumns = `
	c.id, c.post_id, c.author_id, c.parent_id, c.content, c.created_at, c.updated_at,
	u.id, u.username, u.display_name`

const fromClause = `
	FROM comments c
	JOIN users u ON u.id = c.author_id`

// Create inserts a comment and increments the post's comment counter, in one
// transaction.
//
// The counter UPDATE runs first, and deliberately so. It takes a row lock on
// the post, and its `deleted_at IS NULL` predicate is what proves the post is
// still live: if it matches no rows, the post was deleted — possibly by a
// concurrent request that has already committed — and the whole transaction
// rolls back with a 404. Checking existence with a separate SELECT before the
// INSERT would leave a window in which the post disappears between the check
// and the write.
func (r *Repository) Create(ctx context.Context, c *domain.Comment) error {
	return postgres.InTx(ctx, r.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE posts SET comment_count = comment_count + 1 WHERE id = $1 AND deleted_at IS NULL`,
			c.PostID)
		if err != nil {
			return apierr.Internal(fmt.Errorf("increment post comment count: %w", err))
		}
		if tag.RowsAffected() == 0 {
			return ErrPostNotFound
		}

		const insert = `
			INSERT INTO comments (id, post_id, author_id, parent_id, content)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING created_at, updated_at`

		err = tx.QueryRow(ctx, insert, c.ID, c.PostID, c.AuthorID, c.ParentID, c.Content).
			Scan(&c.CreatedAt, &c.UpdatedAt)
		if err == nil {
			return nil
		}

		switch {
		case postgres.IsForeignKeyViolation(err, constraintParentSamePost):
			// The composite (parent_id, post_id) foreign key rejected a reply
			// whose parent lives on a different post — or does not exist.
			return apierr.Validation(apierr.FieldError{
				Field:   "parent_id",
				Message: "must reference an existing comment on the same post",
			}).WithCause(err)
		case postgres.IsForeignKeyViolation(err, constraintPostFK):
			return ErrPostNotFound
		case postgres.IsForeignKeyViolation(err, ""):
			return apierr.Unauthorized("the authenticated account no longer exists").WithCause(err)
		default:
			return apierr.Internal(fmt.Errorf("insert comment: %w", err))
		}
	})
}

// ByID returns a live comment with its author.
func (r *Repository) ByID(ctx context.Context, id uuid.UUID) (domain.Comment, error) {
	q := `SELECT ` + selectColumns + fromClause + `
		WHERE c.id = $1 AND c.deleted_at IS NULL`

	c, err := scanComment(r.db.QueryRow(ctx, q, id))
	if err != nil {
		return domain.Comment{}, postgres.NotFoundOr(fmt.Errorf("select comment: %w", err), "comment")
	}
	return c, nil
}

// AuthorAndPostOf returns the owner and parent post of a live comment: the
// minimal read an authorisation check needs.
func (r *Repository) AuthorAndPostOf(ctx context.Context, id uuid.UUID) (authorID, postID uuid.UUID, err error) {
	err = r.db.QueryRow(ctx,
		`SELECT author_id, post_id FROM comments WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&authorID, &postID)
	if err != nil {
		return uuid.Nil, uuid.Nil, postgres.NotFoundOr(fmt.Errorf("select comment owner: %w", err), "comment")
	}
	return authorID, postID, nil
}

// ListByPost returns one page of a post's comments, oldest first, together with
// the total. Reading order is chronological, which is why this ordering differs
// from the newest-first post listing.
func (r *Repository) ListByPost(ctx context.Context, postID uuid.UUID, page httpx.PageRequest) ([]domain.Comment, int64, error) {
	q := `SELECT ` + selectColumns + `, COUNT(*) OVER () AS total_count` + fromClause + `
		WHERE c.post_id = $1 AND c.deleted_at IS NULL
		ORDER BY c.created_at ASC, c.id ASC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.Query(ctx, q, postID, page.Limit, page.Offset())
	if err != nil {
		return nil, 0, apierr.Internal(fmt.Errorf("list comments: %w", err))
	}
	defer rows.Close()

	comments := make([]domain.Comment, 0, page.Limit)
	var total int64
	for rows.Next() {
		var (
			c      domain.Comment
			author domain.UserSummary
		)
		err := rows.Scan(
			&c.ID, &c.PostID, &c.AuthorID, &c.ParentID, &c.Content, &c.CreatedAt, &c.UpdatedAt,
			&author.ID, &author.Username, &author.DisplayName,
			&total,
		)
		if err != nil {
			return nil, 0, apierr.Internal(fmt.Errorf("scan comment row: %w", err))
		}
		c.Author = &author
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, apierr.Internal(fmt.Errorf("iterate comment rows: %w", err))
	}

	return comments, total, nil
}

// Update changes a comment's body.
//
// No transaction is needed: editing text does not move the counter. This is
// worth stating, because "every write is a transaction" is a habit that costs
// throughput without buying correctness.
func (r *Repository) Update(ctx context.Context, id uuid.UUID, content string) (domain.Comment, error) {
	const q = `
		WITH updated AS (
			UPDATE comments
			   SET content = $2
			 WHERE id = $1 AND deleted_at IS NULL
			RETURNING *
		)
		SELECT ` + selectColumns + `
		  FROM updated c
		  JOIN users u ON u.id = c.author_id`

	c, err := scanComment(r.db.QueryRow(ctx, q, id, content))
	if err != nil {
		return domain.Comment{}, postgres.NotFoundOr(fmt.Errorf("update comment: %w", err), "comment")
	}
	return c, nil
}

// Delete soft-deletes a comment, its replies, and decrements the post counter
// by the number of rows actually removed — all in one transaction.
//
// Deleting a parent also removes its replies, because a reply to a deleted
// comment renders as an orphan. The counter is decremented by the true count
// rather than by one, which is why the UPDATE returns its row count.
func (r *Repository) Delete(ctx context.Context, id, postID uuid.UUID) error {
	return postgres.InTx(ctx, r.db, func(tx pgx.Tx) error {
		// A recursive CTE collects the comment and every descendant, so a
		// three-level thread is removed in a single statement rather than by an
		// application-side tree walk issuing one UPDATE per node.
		const softDelete = `
			WITH RECURSIVE thread AS (
				SELECT id FROM comments WHERE id = $1 AND deleted_at IS NULL
				UNION ALL
				SELECT c.id
				  FROM comments c
				  JOIN thread t ON c.parent_id = t.id
				 WHERE c.deleted_at IS NULL
			)
			UPDATE comments
			   SET deleted_at = now()
			 WHERE id IN (SELECT id FROM thread)`

		tag, err := tx.Exec(ctx, softDelete, id)
		if err != nil {
			return apierr.Internal(fmt.Errorf("soft-delete comment thread: %w", err))
		}
		removed := tag.RowsAffected()
		if removed == 0 {
			return apierr.NotFound("comment")
		}

		// posts_comment_count_non_negative turns any arithmetic mistake here into
		// a failed transaction rather than a silently corrupt counter.
		if _, err := tx.Exec(ctx,
			`UPDATE posts SET comment_count = comment_count - $2 WHERE id = $1`,
			postID, removed); err != nil {
			return apierr.Internal(fmt.Errorf("decrement post comment count by %d: %w", removed, err))
		}
		return nil
	})
}

func scanComment(row pgx.Row) (domain.Comment, error) {
	var (
		c      domain.Comment
		author domain.UserSummary
	)
	err := row.Scan(
		&c.ID, &c.PostID, &c.AuthorID, &c.ParentID, &c.Content, &c.CreatedAt, &c.UpdatedAt,
		&author.ID, &author.Username, &author.DisplayName,
	)
	if err != nil {
		return domain.Comment{}, err
	}
	c.Author = &author
	return c, nil
}
