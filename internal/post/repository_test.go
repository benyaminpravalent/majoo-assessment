package post

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

func postColumns() []string {
	return []string{
		"id", "author_id", "title", "slug", "content", "status",
		"comment_count", "published_at", "created_at", "updated_at",
		"author_id2", "username", "display_name",
	}
}

func postRow(id, authorID uuid.UUID, title, slug string, status domain.PostStatus) []any {
	now := time.Now().UTC().Truncate(time.Second)
	return []any{
		id, authorID, title, slug, "content", status,
		3, nil, now, now,
		authorID, "author", "Author",
	}
}

func TestRepositoryCreateMapsSlugCollisionToTheRetrySentinel(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	mock.ExpectQuery(`INSERT INTO posts`).
		WithArgs(anyArgs(7)...).
		WillReturnError(&pgconn.PgError{Code: "23505", ConstraintName: constraintSlugUnique})

	err := repo.Create(context.Background(), &domain.Post{ID: uuid.New()})

	// The service retries on this sentinel, so it must survive being wrapped in
	// the client-facing conflict error.
	assert.ErrorIs(t, err, ErrSlugTaken)
	assert.Equal(t, apierr.CodeConflict, apierr.From(err).Code)
}

// A foreign-key failure on author_id means the authenticated account was
// deleted between the token check and the insert; 401 is more accurate than a
// 500, and tells the client to re-authenticate.
func TestRepositoryCreateMapsAuthorForeignKeyTo401(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	mock.ExpectQuery(`INSERT INTO posts`).
		WithArgs(anyArgs(7)...).
		WillReturnError(&pgconn.PgError{Code: "23503", ConstraintName: "posts_author_id_fkey"})

	err := repo.Create(context.Background(), &domain.Post{ID: uuid.New()})

	assert.Equal(t, apierr.CodeUnauthorized, apierr.From(err).Code)
}

func TestRepositoryByIDJoinsTheAuthor(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, authorID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .* FROM posts p\s+JOIN users u`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows(postColumns()).
			AddRow(postRow(id, authorID, "Title", "title", domain.PostStatusPublished)...))

	got, err := repo.ByID(context.Background(), id)

	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	require.NotNil(t, got.Author)
	assert.Equal(t, "author", got.Author.Username)
	assert.Equal(t, 3, got.CommentCount)
}

func TestRepositoryByIDMapsNoRowsTo404(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	mock.ExpectQuery(`SELECT .* FROM posts`).
		WithArgs(anyArgs(1)...).
		WillReturnRows(pgxmock.NewRows(postColumns()))

	_, err := repo.ByID(context.Background(), uuid.New())

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

// TestRepositoryListBuildsPlaceholdersForEveryFilter is the injection-safety
// test: no filter value is ever interpolated into the SQL text, so whatever a
// client sends arrives as a bound parameter.
func TestRepositoryListBuildsPlaceholdersForEveryFilter(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	authorID := uuid.New()
	status := domain.PostStatusPublished
	// A search term containing SQL metacharacters: if it were interpolated, this
	// would break the statement.
	const hostileSearch = `'; DROP TABLE posts; --`

	mock.ExpectQuery(`SELECT .*COUNT\(\*\) OVER \(\).* FROM posts`).
		WithArgs(authorID, string(status), hostileSearch, 10, 20).
		WillReturnRows(pgxmock.NewRows(append(postColumns(), "total_count")))

	_, _, err := repo.List(context.Background(), ListFilter{
		AuthorID: &authorID,
		Status:   &status,
		Search:   hostileSearch,
		Page:     httpx.PageRequest{Page: 3, Limit: 10},
	})

	require.NoError(t, err)
}

func TestRepositoryListWithNoFiltersBindsOnlyPagination(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, authorID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT .* FROM posts`).
		WithArgs(20, 0).
		WillReturnRows(pgxmock.NewRows(append(postColumns(), "total_count")).
			AddRow(append(postRow(id, authorID, "Title", "title", domain.PostStatusPublished), int64(42))...))

	posts, total, err := repo.List(context.Background(), ListFilter{
		Page: httpx.PageRequest{Page: 1, Limit: 20},
	})

	require.NoError(t, err)
	require.Len(t, posts, 1)
	assert.Equal(t, int64(42), total, "the total comes from the window function in the same statement")
}

func TestRepositoryListReturnsAnEmptySliceNotNil(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	mock.ExpectQuery(`SELECT .* FROM posts`).
		WithArgs(anyArgs(2)...).
		WillReturnRows(pgxmock.NewRows(append(postColumns(), "total_count")))

	posts, total, err := repo.List(context.Background(), ListFilter{
		Page: httpx.PageRequest{Page: 1, Limit: 20},
	})

	require.NoError(t, err)
	assert.NotNil(t, posts, "an empty page must serialise as [] rather than null")
	assert.Empty(t, posts)
	assert.Zero(t, total)
}

func TestRepositoryUpdateWithNoChangesFallsBackToARead(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, authorID := uuid.New(), uuid.New()
	// An UPDATE with an empty SET list is a syntax error, so the repository must
	// issue a plain read instead.
	mock.ExpectQuery(`SELECT .* FROM posts p\s+JOIN users u`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows(postColumns()).
			AddRow(postRow(id, authorID, "Unchanged", "unchanged", domain.PostStatusDraft)...))

	got, err := repo.Update(context.Background(), id, UpdateInput{})

	require.NoError(t, err)
	assert.Equal(t, "Unchanged", got.Title)
}

func TestRepositoryUpdateBuildsOnlyTheRequestedColumns(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, authorID := uuid.New(), uuid.New()
	title, content := "New Title", "new content"

	// Two SET values plus the id: COALESCE is deliberately not used, so exactly
	// the requested columns are bound.
	mock.ExpectQuery(`UPDATE posts`).
		WithArgs(title, content, id).
		WillReturnRows(pgxmock.NewRows(postColumns()).
			AddRow(postRow(id, authorID, title, "new-title", domain.PostStatusDraft)...))

	got, err := repo.Update(context.Background(), id, UpdateInput{Title: &title, Content: &content})

	require.NoError(t, err)
	assert.Equal(t, title, got.Title)
}

func TestRepositoryUpdateClearsPublishedAtWithoutAPlaceholder(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, authorID := uuid.New(), uuid.New()
	draft := domain.PostStatusDraft

	// published_at = NULL is written literally, so only status and id are bound.
	mock.ExpectQuery(`UPDATE posts`).
		WithArgs(string(draft), id).
		WillReturnRows(pgxmock.NewRows(postColumns()).
			AddRow(postRow(id, authorID, "T", "t", draft)...))

	_, err := repo.Update(context.Background(), id, UpdateInput{Status: &draft, ClearPublishedAt: true})

	require.NoError(t, err)
}

// TestRepositoryDeleteCommitsBothSoftDeletes covers the transaction that keeps
// a deleted post's comments from being left visible.
func TestRepositoryDeleteCommitsBothSoftDeletes(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE posts SET deleted_at`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE comments SET deleted_at`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 7))
	mock.ExpectCommit()

	assert.NoError(t, repo.Delete(context.Background(), id))
}

// TestRepositoryDeleteRollsBackWhenThePostIsAlreadyGone: rolling back here is
// what makes a double delete return 404 instead of silently succeeding.
func TestRepositoryDeleteRollsBackWhenThePostIsAlreadyGone(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE posts SET deleted_at`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectRollback()

	err := repo.Delete(context.Background(), id)

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

func TestRepositoryDeleteRollsBackWhenTheCommentUpdateFails(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id := uuid.New()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE posts SET deleted_at`).
		WithArgs(id).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE comments SET deleted_at`).
		WithArgs(id).
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	err := repo.Delete(context.Background(), id)

	require.Error(t, err)
	assert.Equal(t, apierr.CodeInternal, apierr.From(err).Code)
}

func TestRepositoryExistsAndSlugExists(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id := uuid.New()
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectQuery(`SELECT EXISTS`).
		WithArgs("taken-slug").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(false))

	exists, err := repo.Exists(context.Background(), id)
	require.NoError(t, err)
	assert.True(t, exists)

	taken, err := repo.SlugExists(context.Background(), "taken-slug")
	require.NoError(t, err)
	assert.False(t, taken)
}

func TestRepositoryAuthorOfReturnsTheMinimalProjection(t *testing.T) {
	t.Parallel()

	mock := newMockPool(t)
	repo := NewRepository(mock)

	id, authorID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT author_id, status FROM posts`).
		WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"author_id", "status"}).
			AddRow(authorID, domain.PostStatusDraft))

	gotAuthor, gotStatus, err := repo.AuthorOf(context.Background(), id)

	require.NoError(t, err)
	assert.Equal(t, authorID, gotAuthor)
	assert.Equal(t, domain.PostStatusDraft, gotStatus)
}
