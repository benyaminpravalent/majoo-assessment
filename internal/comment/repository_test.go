package comment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, mock.ExpectationsWereMet())
		mock.Close()
	})
	return mock
}

func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

func commentColumns() []string {
	return []string{
		"id", "post_id", "author_id", "parent_id", "content", "created_at", "updated_at",
		"author_id2", "username", "display_name",
	}
}

func commentRow(id, postID, authorID uuid.UUID, content string) []any {
	now := time.Now().UTC().Truncate(time.Second)
	return []any{id, postID, authorID, nil, content, now, now, authorID, "commenter", "Commenter"}
}

// TestCreateIncrementsTheCounterBeforeInserting documents the statement order
// that closes the delete race: the counter UPDATE takes the post's row lock and
// its "deleted_at IS NULL" predicate proves the post is still live. pgxmock
// enforces ordering by default, so this test fails if the two are swapped.
func TestCreateIncrementsTheCounterBeforeInserting(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	c := &domain.Comment{ID: uuid.New(), PostID: uuid.New(), AuthorID: uuid.New(), Content: "hello"}
	now := time.Now()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE posts SET comment_count = comment_count \+ 1`).
		WithArgs(c.PostID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(`INSERT INTO comments`).
		WithArgs(c.ID, c.PostID, c.AuthorID, c.ParentID, c.Content).
		WillReturnRows(pgxmock.NewRows([]string{"created_at", "updated_at"}).AddRow(now, now))
	mock.ExpectCommit()

	require.NoError(t, repo.Create(context.Background(), c))
	assert.Equal(t, now, c.CreatedAt)
}

// TestCreateRollsBackWhenThePostIsGone: zero rows from the counter UPDATE means
// the post was deleted, so the comment must never be inserted.
func TestCreateRollsBackWhenThePostIsGone(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	c := &domain.Comment{ID: uuid.New(), PostID: uuid.New(), AuthorID: uuid.New(), Content: "hello"}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE posts SET comment_count`).
		WithArgs(c.PostID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err := repo.Create(context.Background(), c)

	assert.ErrorIs(t, err, ErrPostNotFound)
}

// TestCreateRollsBackAndReportsACrossPostParent: the composite foreign key
// rejects the insert, the counter increment is undone, and the client gets a
// field error rather than a 500.
func TestCreateRollsBackAndReportsACrossPostParent(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	parentID := uuid.New()
	c := &domain.Comment{
		ID: uuid.New(), PostID: uuid.New(), AuthorID: uuid.New(),
		ParentID: &parentID, Content: "reply",
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE posts SET comment_count`).
		WithArgs(c.PostID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(`INSERT INTO comments`).
		WithArgs(anyArgs(5)...).
		WillReturnError(&pgconn.PgError{Code: "23503", ConstraintName: constraintParentSamePost})
	mock.ExpectRollback()

	err := repo.Create(context.Background(), c)

	appErr := apierr.From(err)
	require.Equal(t, apierr.CodeValidation, appErr.Code)
	require.Len(t, appErr.Fields, 1)
	assert.Equal(t, "parent_id", appErr.Fields[0].Field)
}

func TestCreateRollsBackOnAnUnexpectedInsertFailure(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	c := &domain.Comment{ID: uuid.New(), PostID: uuid.New(), AuthorID: uuid.New(), Content: "x"}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE posts SET comment_count`).
		WithArgs(c.PostID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(`INSERT INTO comments`).
		WithArgs(anyArgs(5)...).
		WillReturnError(errors.New("connection reset by peer"))
	mock.ExpectRollback()

	err := repo.Create(context.Background(), c)

	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

// TestDeleteDecrementsByTheNumberOfRowsRemoved is the arithmetic the
// posts_comment_count_non_negative CHECK backstops: a thread of four rows must
// reduce the counter by four, not by one.
func TestDeleteDecrementsByTheNumberOfRowsRemoved(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, postID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectExec(`WITH RECURSIVE thread`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 4))
	mock.ExpectExec(`UPDATE posts SET comment_count = comment_count - \$2`).
		WithArgs(postID, int64(4)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	assert.NoError(t, repo.Delete(context.Background(), id, postID))
}

func TestDeleteRollsBackWhenTheCommentIsAlreadyGone(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, postID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectExec(`WITH RECURSIVE thread`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err := repo.Delete(context.Background(), id, postID)

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

// TestDeleteRollsBackWhenTheCounterUpdateFails is the case the CHECK constraint
// is there to catch: if the arithmetic would drive the counter negative, the
// database rejects it and the soft delete is undone rather than leaving a
// corrupt count.
func TestDeleteRollsBackWhenTheCounterUpdateFails(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, postID := uuid.New(), uuid.New()

	mock.ExpectBegin()
	mock.ExpectExec(`WITH RECURSIVE thread`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectExec(`UPDATE posts SET comment_count`).
		WithArgs(postID, int64(2)).
		WillReturnError(&pgconn.PgError{Code: "23514", ConstraintName: "posts_comment_count_non_negative"})
	mock.ExpectRollback()

	err := repo.Delete(context.Background(), id, postID)

	require.Error(t, err)
	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

func TestByIDMapsNoRowsTo404(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	mock.ExpectQuery(`SELECT .* FROM comments c`).
		WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows(commentColumns()))

	_, err := repo.ByID(context.Background(), uuid.New())

	appErr := apierr.From(err)
	assert.Equal(t, apierr.CodeNotFound, appErr.Code)
	assert.Equal(t, "comment not found", appErr.Message)
}

// Comment threads read oldest-first, which is the opposite of the post listing.
// The composite index (post_id, created_at, id) is built for exactly this.
func TestListByPostOrdersOldestFirstAndReturnsTheWindowTotal(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	postID, authorID := uuid.New(), uuid.New()
	mock.ExpectQuery(`ORDER BY c.created_at ASC, c.id ASC`).
		WithArgs(postID, 10, 20).
		WillReturnRows(pgxmock.NewRows(append(commentColumns(), "total_count")).
			AddRow(append(commentRow(uuid.New(), postID, authorID, "first"), int64(31))...))

	got, total, err := repo.ListByPost(context.Background(), postID, httpx.PageRequest{Page: 3, Limit: 10})

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(31), total)
	require.NotNil(t, got[0].Author)
	assert.Equal(t, "commenter", got[0].Author.Username)
}

func TestListByPostReturnsAnEmptySliceNotNil(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	mock.ExpectQuery(`FROM comments c`).
		WithArgs(anyArgs(3)...).
		WillReturnRows(pgxmock.NewRows(append(commentColumns(), "total_count")))

	got, total, err := repo.ListByPost(context.Background(), uuid.New(), httpx.PageRequest{Page: 1, Limit: 10})

	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
	assert.Zero(t, total)
}

// TestUpdateUsesNoTransaction records a deliberate decision: editing text does
// not move the counter, so wrapping it in a transaction would cost throughput
// without buying correctness. pgxmock fails the test if a Begin is issued.
func TestUpdateUsesNoTransaction(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, postID, authorID := uuid.New(), uuid.New(), uuid.New()
	mock.ExpectQuery(`UPDATE comments`).
		WithArgs(id, "edited").
		WillReturnRows(pgxmock.NewRows(commentColumns()).
			AddRow(commentRow(id, postID, authorID, "edited")...))

	got, err := repo.Update(context.Background(), id, "edited")

	require.NoError(t, err)
	assert.Equal(t, "edited", got.Content)
}

func TestAuthorAndPostOf(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, postID, authorID := uuid.New(), uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT author_id, post_id FROM comments`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"author_id", "post_id"}).AddRow(authorID, postID))

	gotAuthor, gotPost, err := repo.AuthorAndPostOf(context.Background(), id)

	require.NoError(t, err)
	assert.Equal(t, authorID, gotAuthor)
	assert.Equal(t, postID, gotPost)
}
