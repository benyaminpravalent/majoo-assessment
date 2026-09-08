//go:build integration

// Package integration exercises the repositories, the migrations and the full
// HTTP stack against a real PostgreSQL instance.
//
// It is behind the `integration` build tag and gated on TEST_DATABASE_URL, so
// `go test ./...` stays fast and hermetic on a machine with no database. Run it
// with:
//
//	docker compose up -d postgres
//	TEST_DATABASE_URL="postgres://blog:blog@localhost:5432/blog_test?sslmode=disable" \
//	  go test -tags=integration -count=1 ./tests/integration/...
//
// These tests cover what pgxmock structurally cannot: that the SQL is valid
// against the real schema, that the CHECK and foreign-key constraints actually
// reject what they claim to, and that the transactional bookkeeping holds under
// a real concurrent write.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/auth"
	"github.com/bpsiregar/majoo-assessment/internal/comment"
	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
	"github.com/bpsiregar/majoo-assessment/internal/platform/postgres"
	"github.com/bpsiregar/majoo-assessment/internal/post"
	"github.com/bpsiregar/majoo-assessment/internal/server"
	"github.com/bpsiregar/majoo-assessment/internal/user"
	"github.com/bpsiregar/majoo-assessment/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// testPool is shared by every test in this package: connecting once and
// truncating between tests is far faster than a pool per test.
var testPool *pgxpool.Pool

// TestMain applies the migrations once for the whole package, so each test
// starts from a schema that is known to match the code.
func TestMain(m *testing.M) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		fmt.Fprintln(os.Stderr,
			"skipping integration tests: TEST_DATABASE_URL is not set")
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := postgres.Connect(ctx, config.DBConfig{
		URL:            url,
		MaxConns:       10,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration tests: cannot reach TEST_DATABASE_URL: %v\n", err)
		os.Exit(1)
	}
	testPool = pool

	logger := logging.New(os.Stderr, "error", "text")
	migrator := postgres.NewMigrator(pool, migrations.FS, logger)

	// Start from a clean schema so a leftover database from an earlier run
	// cannot mask a broken migration.
	if _, err := migrator.Down(ctx, 100); err != nil && !strings.Contains(err.Error(), "does not exist") {
		fmt.Fprintf(os.Stderr, "integration tests: rollback failed: %v\n", err)
	}
	if _, err := migrator.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "integration tests: migrations failed: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	pool.Close()
	os.Exit(code)
}

// truncate empties the tables between tests. TRUNCATE ... CASCADE is much
// faster than deleting rows and resets everything the foreign keys reach.
func truncate(t *testing.T) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`TRUNCATE users, posts, comments, refresh_tokens RESTART IDENTITY CASCADE`)
	require.NoError(t, err)
}

func ctxFor(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newUser(t *testing.T, suffix string) domain.User {
	t.Helper()

	repo := user.NewRepository(testPool)
	hash, err := auth.NewHasher(bcrypt.MinCost).Hash("a-good-enough-password")
	require.NoError(t, err)

	u := domain.User{
		ID:           uuid.New(),
		Email:        "user" + suffix + "@example.com",
		Username:     "user" + suffix,
		DisplayName:  "User " + suffix,
		PasswordHash: hash,
		Role:         domain.RoleUser,
	}
	require.NoError(t, repo.Create(ctxFor(t), &u))
	return u
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestMigrationsAreRecordedAndIdempotent(t *testing.T) {
	ctx := ctxFor(t)
	migrator := postgres.NewMigrator(testPool, migrations.FS, logging.New(os.Stderr, "error", "text"))

	applied, err := migrator.Applied(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, applied, "TestMain should have applied at least one migration")
	assert.Equal(t, int64(1), applied[0].Version)
	assert.NotEmpty(t, applied[0].Checksum)

	// Running Up again must be a no-op, not an error: an init container may run
	// on every deploy.
	second, err := migrator.Up(ctx)
	require.NoError(t, err)
	assert.Empty(t, second)

	pending, err := migrator.Pending(ctx)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

// ---------------------------------------------------------------------------
// Schema constraints
// ---------------------------------------------------------------------------

// TestDatabaseRejectsInvalidState is the point of putting the domain's
// invariants in the schema: each of these would be a silent data bug if only
// the application enforced them.
func TestDatabaseRejectsInvalidState(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "constraints")

	tests := map[string]struct {
		sql  string
		args []any
	}{
		"unknown role": {
			`INSERT INTO users (id, email, username, display_name, password_hash, role)
			 VALUES ($1, 'x@example.com', 'roleuser', 'X', 'aaaaaaaaaaaaaaaaaaaaaa', 'superuser')`,
			[]any{uuid.New()},
		},
		"uppercase email": {
			`INSERT INTO users (id, email, username, display_name, password_hash)
			 VALUES ($1, 'MixedCase@example.com', 'caseuser', 'X', 'aaaaaaaaaaaaaaaaaaaaaa')`,
			[]any{uuid.New()},
		},
		"malformed email": {
			`INSERT INTO users (id, email, username, display_name, password_hash)
			 VALUES ($1, 'not-an-email', 'mailuser', 'X', 'aaaaaaaaaaaaaaaaaaaaaa')`,
			[]any{uuid.New()},
		},
		"username with a space": {
			`INSERT INTO users (id, email, username, display_name, password_hash)
			 VALUES ($1, 'space@example.com', 'has space', 'X', 'aaaaaaaaaaaaaaaaaaaaaa')`,
			[]any{uuid.New()},
		},
		"published post without published_at": {
			`INSERT INTO posts (id, author_id, title, slug, content, status)
			 VALUES ($1, $2, 'A Title', 'a-title', 'body', 'published')`,
			[]any{uuid.New(), author.ID},
		},
		"draft with published_at": {
			`INSERT INTO posts (id, author_id, title, slug, content, status, published_at)
			 VALUES ($1, $2, 'A Title', 'a-title-2', 'body', 'draft', now())`,
			[]any{uuid.New(), author.ID},
		},
		"unknown post status": {
			`INSERT INTO posts (id, author_id, title, slug, content, status)
			 VALUES ($1, $2, 'A Title', 'a-title-3', 'body', 'archived')`,
			[]any{uuid.New(), author.ID},
		},
		"slug with uppercase": {
			`INSERT INTO posts (id, author_id, title, slug, content, status)
			 VALUES ($1, $2, 'A Title', 'Not-A-Slug', 'body', 'draft')`,
			[]any{uuid.New(), author.ID},
		},
		"negative comment count": {
			`INSERT INTO posts (id, author_id, title, slug, content, status, comment_count)
			 VALUES ($1, $2, 'A Title', 'a-title-4', 'body', 'draft', -1)`,
			[]any{uuid.New(), author.ID},
		},
		"post for a non-existent author": {
			`INSERT INTO posts (id, author_id, title, slug, content, status)
			 VALUES ($1, $2, 'A Title', 'a-title-5', 'body', 'draft')`,
			[]any{uuid.New(), uuid.New()},
		},
		"blank comment": {
			`INSERT INTO comments (id, post_id, author_id, content)
			 VALUES ($1, $2, $3, '   ')`,
			[]any{uuid.New(), uuid.New(), author.ID},
		},
		"refresh token digest of the wrong width": {
			`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
			 VALUES ($1, $2, '\x0102'::bytea, now() + interval '1 day')`,
			[]any{uuid.New(), author.ID},
		},
		"refresh token expiring in the past": {
			`INSERT INTO refresh_tokens (id, user_id, token_hash, expires_at)
			 VALUES ($1, $2, decode(repeat('61', 32), 'hex'), now() - interval '1 day')`,
			[]any{uuid.New(), author.ID},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := testPool.Exec(ctx, tc.sql, tc.args...)
			assert.Error(t, err, "the database must reject this row")
		})
	}
}

// TestSlugUniquenessAppliesOnlyToLivePosts proves the partial unique index does
// what a table-level UNIQUE could not: a deleted post releases its slug.
func TestSlugUniquenessAppliesOnlyToLivePosts(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "slugs")
	repo := post.NewRepository(testPool)

	first := &domain.Post{
		ID: uuid.New(), AuthorID: author.ID, Title: "Same Title",
		Slug: "same-title", Content: "body", Status: domain.PostStatusDraft,
	}
	require.NoError(t, repo.Create(ctx, first))

	duplicate := &domain.Post{
		ID: uuid.New(), AuthorID: author.ID, Title: "Same Title",
		Slug: "same-title", Content: "body", Status: domain.PostStatusDraft,
	}
	err := repo.Create(ctx, duplicate)
	require.Error(t, err)
	assert.ErrorIs(t, err, post.ErrSlugTaken)

	require.NoError(t, repo.Delete(ctx, first.ID))

	// The slug is free again now that the original is soft-deleted.
	reused := &domain.Post{
		ID: uuid.New(), AuthorID: author.ID, Title: "Same Title",
		Slug: "same-title", Content: "body", Status: domain.PostStatusDraft,
	}
	assert.NoError(t, repo.Create(ctx, reused))
}

// TestReplyMustLiveOnItsParentsPost exercises the composite foreign key, which
// holds even for writes that never pass through this service.
func TestReplyMustLiveOnItsParentsPost(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "threads")
	postRepo := post.NewRepository(testPool)
	commentRepo := comment.NewRepository(testPool)

	postA := &domain.Post{ID: uuid.New(), AuthorID: author.ID, Title: "Post A", Slug: "post-a", Content: "body", Status: domain.PostStatusDraft}
	postB := &domain.Post{ID: uuid.New(), AuthorID: author.ID, Title: "Post B", Slug: "post-b", Content: "body", Status: domain.PostStatusDraft}
	require.NoError(t, postRepo.Create(ctx, postA))
	require.NoError(t, postRepo.Create(ctx, postB))

	parent := &domain.Comment{ID: uuid.New(), PostID: postA.ID, AuthorID: author.ID, Content: "on A"}
	require.NoError(t, commentRepo.Create(ctx, parent))

	crossPost := &domain.Comment{
		ID: uuid.New(), PostID: postB.ID, AuthorID: author.ID,
		ParentID: &parent.ID, Content: "reply from another post",
	}
	err := commentRepo.Create(ctx, crossPost)

	require.Error(t, err)
	appErr := apierr.From(err)
	assert.Equal(t, apierr.CodeValidation, appErr.Code)
	require.Len(t, appErr.Fields, 1)
	assert.Equal(t, "parent_id", appErr.Fields[0].Field)
}

// ---------------------------------------------------------------------------
// Transactional behaviour
// ---------------------------------------------------------------------------

// TestCommentCounterStaysCorrectUnderConcurrency is the test that a mock cannot
// substitute for: twenty goroutines commenting on the same post at once, with
// the counter maintained inside each transaction.
func TestCommentCounterStaysCorrectUnderConcurrency(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "concurrent")
	postRepo := post.NewRepository(testPool)
	commentRepo := comment.NewRepository(testPool)

	p := &domain.Post{
		ID: uuid.New(), AuthorID: author.ID, Title: "Busy Post",
		Slug: "busy-post", Content: "body", Status: domain.PostStatusDraft,
	}
	require.NoError(t, postRepo.Create(ctx, p))

	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c := &domain.Comment{
				ID: uuid.New(), PostID: p.ID, AuthorID: author.ID,
				Content: fmt.Sprintf("comment %d", n),
			}
			if err := commentRepo.Create(context.Background(), c); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var counter, actual int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT comment_count FROM posts WHERE id = $1`, p.ID).Scan(&counter))
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM comments WHERE post_id = $1 AND deleted_at IS NULL`, p.ID).Scan(&actual))

	assert.Equal(t, writers, counter)
	assert.Equal(t, counter, actual, "the denormalised counter must match reality")
}

// TestCommentOnADeletedPostRollsBack covers the race the transaction closes:
// the post disappears after the visibility check and before the insert.
func TestCommentOnADeletedPostRollsBack(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "deleted")
	postRepo := post.NewRepository(testPool)
	commentRepo := comment.NewRepository(testPool)

	p := &domain.Post{
		ID: uuid.New(), AuthorID: author.ID, Title: "Doomed Post",
		Slug: "doomed-post", Content: "body", Status: domain.PostStatusDraft,
	}
	require.NoError(t, postRepo.Create(ctx, p))
	require.NoError(t, postRepo.Delete(ctx, p.ID))

	err := commentRepo.Create(ctx, &domain.Comment{
		ID: uuid.New(), PostID: p.ID, AuthorID: author.ID, Content: "too late",
	})

	assert.ErrorIs(t, err, comment.ErrPostNotFound)

	var orphans int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM comments WHERE post_id = $1`, p.ID).Scan(&orphans))
	assert.Zero(t, orphans, "the rolled-back transaction must leave nothing behind")
}

// TestDeletingAThreadDecrementsByTheWholeSubtree checks the recursive CTE and
// the counter arithmetic together.
func TestDeletingAThreadDecrementsByTheWholeSubtree(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "subtree")
	postRepo := post.NewRepository(testPool)
	commentRepo := comment.NewRepository(testPool)

	p := &domain.Post{
		ID: uuid.New(), AuthorID: author.ID, Title: "Threaded Post",
		Slug: "threaded-post", Content: "body", Status: domain.PostStatusDraft,
	}
	require.NoError(t, postRepo.Create(ctx, p))

	root := &domain.Comment{ID: uuid.New(), PostID: p.ID, AuthorID: author.ID, Content: "root"}
	require.NoError(t, commentRepo.Create(ctx, root))
	child := &domain.Comment{ID: uuid.New(), PostID: p.ID, AuthorID: author.ID, ParentID: &root.ID, Content: "child"}
	require.NoError(t, commentRepo.Create(ctx, child))
	grandchild := &domain.Comment{ID: uuid.New(), PostID: p.ID, AuthorID: author.ID, ParentID: &child.ID, Content: "grandchild"}
	require.NoError(t, commentRepo.Create(ctx, grandchild))
	sibling := &domain.Comment{ID: uuid.New(), PostID: p.ID, AuthorID: author.ID, Content: "sibling"}
	require.NoError(t, commentRepo.Create(ctx, sibling))

	require.NoError(t, commentRepo.Delete(ctx, root.ID, p.ID))

	var counter int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT comment_count FROM posts WHERE id = $1`, p.ID).Scan(&counter))
	assert.Equal(t, 1, counter, "root, child and grandchild are gone; the sibling remains")

	_, err := commentRepo.ByID(ctx, grandchild.ID)
	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
	_, err = commentRepo.ByID(ctx, sibling.ID)
	assert.NoError(t, err)
}

// TestRefreshTokenRotationIsAtomicUnderConcurrency: two clients racing with the
// same refresh token must produce exactly one winner.
func TestRefreshTokenRotationIsAtomicUnderConcurrency(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	owner := newUser(t, "rotation")
	repo := user.NewSessionRepository(testPool)

	original := &user.Session{
		ID: uuid.New(), UserID: owner.ID,
		TokenHash: auth.HashRefreshToken("original-token"),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, repo.Create(ctx, original))

	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		wins     int
		failures int
	)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			next := &user.Session{
				ID: uuid.New(), UserID: owner.ID,
				TokenHash: auth.HashRefreshToken(fmt.Sprintf("replacement-%d", n)),
				ExpiresAt: time.Now().Add(time.Hour),
			}
			err := repo.Rotate(context.Background(), original.TokenHash, next)

			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else {
				failures++
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, 1, wins, "exactly one rotation may succeed")
	assert.Equal(t, racers-1, failures)

	var live int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT count(*) FROM refresh_tokens WHERE user_id = $1 AND revoked_at IS NULL`, owner.ID).Scan(&live))
	assert.Equal(t, 1, live, "the losers must not have inserted replacements")
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// TestFullTextSearchMatchesAgainstTheGeneratedColumn exercises the
// websearch_to_tsquery path, which no mock can validate.
func TestFullTextSearchMatchesAgainstTheGeneratedColumn(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "search")
	repo := post.NewRepository(testPool)

	publish := func(title, body string) {
		now := time.Now()
		p := &domain.Post{
			ID: uuid.New(), AuthorID: author.ID, Title: title,
			Slug: post.Slugify(title), Content: body,
			Status: domain.PostStatusPublished, PublishedAt: &now,
		}
		require.NoError(t, repo.Create(ctx, p))
	}
	publish("Concurrency in Go", "Goroutines and channels, explained slowly.")
	publish("Database Indexes", "Composite indexes and column ordering.")
	publish("Deploying with Kubernetes", "Rolling updates and readiness probes.")

	published := domain.PostStatusPublished
	page := httpx.PageRequest{Page: 1, Limit: 10}

	found, total, err := repo.List(ctx, post.ListFilter{Search: "goroutines", Status: &published, Page: page})
	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, found, 1)
	assert.Equal(t, "Concurrency in Go", found[0].Title)

	// A hostile search string must be treated as text, not as SQL or as tsquery
	// syntax. websearch_to_tsquery is chosen precisely so this cannot throw.
	_, _, err = repo.List(ctx, post.ListFilter{Search: `'; DROP TABLE posts; --`, Page: page})
	assert.NoError(t, err)

	var stillThere int
	require.NoError(t, testPool.QueryRow(ctx, `SELECT count(*) FROM posts`).Scan(&stillThere))
	assert.Equal(t, 3, stillThere)
}

func TestListPaginationAgainstRealData(t *testing.T) {
	truncate(t)
	ctx := ctxFor(t)
	author := newUser(t, "paging")
	repo := post.NewRepository(testPool)

	for i := 0; i < 25; i++ {
		now := time.Now()
		title := fmt.Sprintf("Post number %02d", i)
		require.NoError(t, repo.Create(ctx, &domain.Post{
			ID: uuid.New(), AuthorID: author.ID, Title: title,
			Slug: post.Slugify(title), Content: "body",
			Status: domain.PostStatusPublished, PublishedAt: &now,
		}))
	}

	seen := map[uuid.UUID]bool{}
	for page := 1; page <= 3; page++ {
		items, total, err := repo.List(ctx, post.ListFilter{
			Page: httpx.PageRequest{Page: page, Limit: 10},
		})
		require.NoError(t, err)
		assert.Equal(t, int64(25), total)

		for _, p := range items {
			assert.False(t, seen[p.ID], "the total ordering must not repeat a row across pages")
			seen[p.ID] = true
		}
	}
	assert.Len(t, seen, 25, "every row must appear exactly once across the pages")
}

// ---------------------------------------------------------------------------
// End-to-end HTTP
// ---------------------------------------------------------------------------

// TestEndToEndBlogFlow is the reviewer walkthrough, executed: register, log in,
// create a post, read it, comment on it, and confirm that another account
// cannot modify either.
func TestEndToEndBlogFlow(t *testing.T) {
	truncate(t)

	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("JWT_SECRET", strings.Repeat("k", 48))
	t.Setenv("BCRYPT_COST", "10")
	t.Setenv("RATE_LIMIT_ENABLED", "false")
	t.Setenv("LOG_LEVEL", "error")

	cfg, err := config.Load()
	require.NoError(t, err)

	srv := server.New(cfg, logging.New(os.Stderr, "error", "json"), testPool, "integration")
	t.Cleanup(srv.Close)
	router := srv.Router()

	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.RemoteAddr = "203.0.113.7:5000"

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	// 1. Register two accounts.
	for _, body := range []string{
		`{"email":"alice@example.com","username":"alice","display_name":"Alice","password":"a-good-enough-password"}`,
		`{"email":"mallory@example.com","username":"mallory","display_name":"Mallory","password":"a-good-enough-password"}`,
	} {
		w := call(http.MethodPost, "/api/v1/auth/register", body, "")
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	}

	// A duplicate registration is a 409, not a 500.
	dup := call(http.MethodPost, "/api/v1/auth/register",
		`{"email":"alice@example.com","username":"alice2","display_name":"Alice","password":"a-good-enough-password"}`, "")
	assert.Equal(t, http.StatusConflict, dup.Code)

	// 2. Log in as each.
	login := func(email string) string {
		w := call(http.MethodPost, "/api/v1/auth/login",
			fmt.Sprintf(`{"email":%q,"password":"a-good-enough-password"}`, email), "")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var body struct {
			Data struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
		require.NotEmpty(t, body.Data.AccessToken)
		return body.Data.AccessToken
	}
	aliceToken := login("alice@example.com")
	malloryToken := login("mallory@example.com")

	// A wrong password is a 401.
	bad := call(http.MethodPost, "/api/v1/auth/login",
		`{"email":"alice@example.com","password":"wrong-password"}`, "")
	assert.Equal(t, http.StatusUnauthorized, bad.Code)

	// 3. Alice creates a post.
	created := call(http.MethodPost, "/api/v1/posts",
		`{"title":"Integration Test Post","content":"Written by the end-to-end test.","status":"published"}`,
		aliceToken)
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())

	var postBody struct {
		Data struct {
			ID   uuid.UUID `json:"id"`
			Slug string    `json:"slug"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &postBody))
	postID := postBody.Data.ID.String()
	assert.Equal(t, "integration-test-post", postBody.Data.Slug)

	// 4. Anyone can read it.
	read := call(http.MethodGet, "/api/v1/posts/"+postID, "", "")
	require.Equal(t, http.StatusOK, read.Code)
	assert.Contains(t, read.Body.String(), "Integration Test Post")

	// 5. Mallory comments on it.
	commented := call(http.MethodPost, "/api/v1/posts/"+postID+"/comments",
		`{"content":"First!"}`, malloryToken)
	require.Equal(t, http.StatusCreated, commented.Code, commented.Body.String())

	var commentBody struct {
		Data struct {
			ID uuid.UUID `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(commented.Body.Bytes(), &commentBody))

	// The denormalised counter moved with the comment.
	listed := call(http.MethodGet, "/api/v1/posts/"+postID, "", "")
	assert.Contains(t, listed.Body.String(), `"comment_count":1`)

	// 6. Mallory may not modify Alice's post.
	hijack := call(http.MethodPatch, "/api/v1/posts/"+postID, `{"title":"Hijacked Title"}`, malloryToken)
	assert.Equal(t, http.StatusForbidden, hijack.Code)

	destroy := call(http.MethodDelete, "/api/v1/posts/"+postID, "", malloryToken)
	assert.Equal(t, http.StatusForbidden, destroy.Code)

	// Nor may Alice edit Mallory's comment.
	editComment := call(http.MethodPatch, "/api/v1/comments/"+commentBody.Data.ID.String(),
		`{"content":"Edited by the post author"}`, aliceToken)
	assert.Equal(t, http.StatusForbidden, editComment.Code)

	// 7. Anonymous writes are rejected.
	anon := call(http.MethodPost, "/api/v1/posts", `{"title":"Anonymous Post","content":"body"}`, "")
	assert.Equal(t, http.StatusUnauthorized, anon.Code)

	// A forged token is rejected too.
	forged := call(http.MethodPost, "/api/v1/posts",
		`{"title":"Forged Post","content":"body"}`, "not.a.real.token")
	assert.Equal(t, http.StatusUnauthorized, forged.Code)

	// 8. Alice deletes her own post; its comment goes with it.
	deleted := call(http.MethodDelete, "/api/v1/posts/"+postID, "", aliceToken)
	require.Equal(t, http.StatusNoContent, deleted.Code)

	gone := call(http.MethodGet, "/api/v1/posts/"+postID, "", "")
	assert.Equal(t, http.StatusNotFound, gone.Code)

	orphan := call(http.MethodGet, "/api/v1/comments/"+commentBody.Data.ID.String(), "", "")
	assert.Equal(t, http.StatusNotFound, orphan.Code,
		"deleting a post must not leave its comments readable")
}

// TestReadinessSucceedsAgainstARealDatabase completes the health story: the
// unit test proved the probe fails when the database is unreachable, and this
// proves it passes when it is.
func TestReadinessSucceedsAgainstARealDatabase(t *testing.T) {
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("JWT_SECRET", strings.Repeat("k", 48))
	t.Setenv("RATE_LIMIT_ENABLED", "false")
	t.Setenv("LOG_LEVEL", "error")

	cfg, err := config.Load()
	require.NoError(t, err)

	srv := server.New(cfg, logging.New(os.Stderr, "error", "json"), testPool, "integration")
	t.Cleanup(srv.Close)

	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"status":"ok"`)
}
