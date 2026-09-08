package comment

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore is an in-memory comment store. It reproduces the two rules the
// database enforces and the service relies on: a reply must live on its
// parent's post, and deleting a comment removes its descendants.
type fakeStore struct {
	mu       sync.Mutex
	comments map[uuid.UUID]domain.Comment
	// counters mirrors posts.comment_count so the transactional bookkeeping is
	// observable from a test.
	counters map[uuid.UUID]int

	createErr error
	deleteErr error
	livePosts map[uuid.UUID]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		comments:  map[uuid.UUID]domain.Comment{},
		counters:  map[uuid.UUID]int{},
		livePosts: map[uuid.UUID]bool{},
	}
}

func (f *fakeStore) Create(_ context.Context, c *domain.Comment) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return f.createErr
	}
	if !f.livePosts[c.PostID] {
		return ErrPostNotFound
	}
	if c.ParentID != nil {
		parent, ok := f.comments[*c.ParentID]
		if !ok || parent.PostID != c.PostID {
			// What the comments_parent_same_post composite foreign key enforces.
			return apierr.Validation(apierr.FieldError{
				Field:   "parent_id",
				Message: "must reference an existing comment on the same post",
			})
		}
	}

	c.CreatedAt = time.Now().UTC()
	c.UpdatedAt = c.CreatedAt
	c.Author = &domain.UserSummary{ID: c.AuthorID, Username: "commenter", DisplayName: "Commenter"}
	f.comments[c.ID] = *c
	f.counters[c.PostID]++
	return nil
}

func (f *fakeStore) ByID(_ context.Context, id uuid.UUID) (domain.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.comments[id]
	if !ok {
		return domain.Comment{}, apierr.NotFound("comment")
	}
	return c, nil
}

func (f *fakeStore) AuthorAndPostOf(_ context.Context, id uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.comments[id]
	if !ok {
		return uuid.Nil, uuid.Nil, apierr.NotFound("comment")
	}
	return c.AuthorID, c.PostID, nil
}

func (f *fakeStore) ListByPost(_ context.Context, postID uuid.UUID, page httpx.PageRequest) ([]domain.Comment, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	matched := make([]domain.Comment, 0, len(f.comments))
	for _, c := range f.comments {
		if c.PostID == postID {
			matched = append(matched, c)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Content < matched[j].Content })

	total := int64(len(matched))
	start := page.Offset()
	if start > len(matched) {
		start = len(matched)
	}
	end := start + page.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[start:end], total, nil
}

func (f *fakeStore) Update(_ context.Context, id uuid.UUID, content string) (domain.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.comments[id]
	if !ok {
		return domain.Comment{}, apierr.NotFound("comment")
	}
	c.Content = content
	c.UpdatedAt = time.Now().UTC()
	f.comments[id] = c
	return c, nil
}

func (f *fakeStore) Delete(_ context.Context, id, postID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.comments[id]; !ok {
		return apierr.NotFound("comment")
	}

	// Remove the comment and everything below it, as the recursive CTE does.
	removed := 0
	queue := []uuid.UUID{id}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, ok := f.comments[current]; !ok {
			continue
		}
		delete(f.comments, current)
		removed++
		for childID, child := range f.comments {
			if child.ParentID != nil && *child.ParentID == current {
				queue = append(queue, childID)
			}
		}
	}
	f.counters[postID] -= removed
	return nil
}

// fakePostReader stands in for the post service.
type fakePostReader struct {
	mu      sync.Mutex
	posts   map[uuid.UUID]domain.Post
	lastErr error
}

func newFakePostReader() *fakePostReader {
	return &fakePostReader{posts: map[uuid.UUID]domain.Post{}}
}

func (f *fakePostReader) ByID(_ context.Context, actor *domain.Actor, id uuid.UUID) (domain.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.lastErr != nil {
		return domain.Post{}, f.lastErr
	}
	p, ok := f.posts[id]
	if !ok {
		return domain.Post{}, apierr.NotFound("post")
	}
	// The same visibility rule the real post service applies.
	if p.Status != domain.PostStatusPublished {
		if actor == nil || !actor.CanModify(p.AuthorID) {
			return domain.Post{}, apierr.NotFound("post")
		}
	}
	return p, nil
}

type recordingBus struct {
	mu     sync.Mutex
	events []events.Event
}

func (b *recordingBus) Publish(_ context.Context, e events.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
}

func (b *recordingBus) names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.events))
	for _, e := range b.events {
		out = append(out, e.Name)
	}
	return out
}

type fixture struct {
	svc    *Service
	store  *fakeStore
	posts  *fakePostReader
	bus    *recordingBus
	author domain.Actor
	other  domain.Actor
	admin  domain.Actor

	publishedPost domain.Post
	draftPost     domain.Post
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := newFakeStore()
	posts := newFakePostReader()
	bus := &recordingBus{}

	f := &fixture{
		svc:    NewService(store, posts, bus),
		store:  store,
		posts:  posts,
		bus:    bus,
		author: domain.Actor{UserID: uuid.New(), Role: domain.RoleUser},
		other:  domain.Actor{UserID: uuid.New(), Role: domain.RoleUser},
		admin:  domain.Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
	}

	f.publishedPost = domain.Post{ID: uuid.New(), AuthorID: f.author.UserID, Status: domain.PostStatusPublished}
	f.draftPost = domain.Post{ID: uuid.New(), AuthorID: f.author.UserID, Status: domain.PostStatusDraft}
	posts.posts[f.publishedPost.ID] = f.publishedPost
	posts.posts[f.draftPost.ID] = f.draftPost
	store.livePosts[f.publishedPost.ID] = true
	store.livePosts[f.draftPost.ID] = true

	return f
}

func (f *fixture) comment(t *testing.T, actor domain.Actor, postID uuid.UUID, content string, parent *uuid.UUID) domain.Comment {
	t.Helper()
	c, err := f.svc.Create(context.Background(), actor, CreateInput{
		PostID: postID, ParentID: parent, Content: content,
	})
	require.NoError(t, err)
	return c
}

func TestCreateCommentOnAPublishedPost(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	c := f.comment(t, f.other, f.publishedPost.ID, "  Nice post  ", nil)

	assert.Equal(t, "Nice post", c.Content, "content is trimmed")
	assert.Equal(t, f.other.UserID, c.AuthorID)
	assert.Equal(t, 1, f.store.counters[f.publishedPost.ID],
		"the post's denormalised counter must move with the comment")
	assert.Contains(t, f.bus.names(), "comment.created")
}

func TestCreateReply(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	parent := f.comment(t, f.other, f.publishedPost.ID, "Parent", nil)

	reply := f.comment(t, f.author, f.publishedPost.ID, "Reply", &parent.ID)

	require.NotNil(t, reply.ParentID)
	assert.Equal(t, parent.ID, *reply.ParentID)
	assert.Equal(t, 2, f.store.counters[f.publishedPost.ID])
}

// TestCreateRejectsAParentOnAnotherPost is enforced by a composite foreign key
// in the schema; the service must surface it as a field error, not a 500.
func TestCreateRejectsAParentOnAnotherPost(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	otherPost := domain.Post{ID: uuid.New(), AuthorID: f.author.UserID, Status: domain.PostStatusPublished}
	f.posts.posts[otherPost.ID] = otherPost
	f.store.livePosts[otherPost.ID] = true
	parent := f.comment(t, f.other, otherPost.ID, "Elsewhere", nil)

	_, err := f.svc.Create(context.Background(), f.other, CreateInput{
		PostID: f.publishedPost.ID, ParentID: &parent.ID, Content: "Cross-post reply",
	})

	require.Error(t, err)
	appErr := apierr.From(err)
	assert.Equal(t, apierr.CodeValidation, appErr.Code)
	require.Len(t, appErr.Fields, 1)
	assert.Equal(t, "parent_id", appErr.Fields[0].Field)
}

func TestCreateOnAMissingPostReturns404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.svc.Create(context.Background(), f.other, CreateInput{
		PostID: uuid.New(), Content: "Into the void",
	})

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

// TestCreateOnSomeoneElsesDraftReturns404: commenting must not be a way to
// discover draft IDs that a read already refuses to confirm.
func TestCreateOnSomeoneElsesDraftReturns404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.svc.Create(context.Background(), f.other, CreateInput{
		PostID: f.draftPost.ID, Content: "Sneaky",
	})

	appErr := apierr.From(err)
	assert.Equal(t, apierr.CodeNotFound, appErr.Code)
	assert.Zero(t, f.store.counters[f.draftPost.ID], "nothing must have been written")
}

func TestAuthorCanCommentOnTheirOwnDraft(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.svc.Create(context.Background(), f.author, CreateInput{
		PostID: f.draftPost.ID, Content: "Note to self",
	})

	assert.NoError(t, err)
}

// TestCreateHandlesAPostDeletedMidFlight covers the race the transaction
// closes: the visibility check passed, then the post disappeared before the
// insert. The transaction rolls back and the caller gets a 404.
func TestCreateHandlesAPostDeletedMidFlight(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.store.createErr = ErrPostNotFound

	_, err := f.svc.Create(context.Background(), f.other, CreateInput{
		PostID: f.publishedPost.ID, Content: "Too late",
	})

	require.Error(t, err)
	appErr := apierr.From(err)
	assert.Equal(t, apierr.CodeNotFound, appErr.Code)
	assert.Equal(t, "post not found", appErr.Message, "the missing resource is the post, not the comment")
}

func TestListByPostPaginates(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	for _, body := range []string{"a", "b", "c", "d", "e"} {
		f.comment(t, f.other, f.publishedPost.ID, body, nil)
	}

	got, total, err := f.svc.ListByPost(context.Background(), nil, f.publishedPost.ID,
		httpx.PageRequest{Page: 2, Limit: 2})

	require.NoError(t, err)
	assert.Equal(t, int64(5), total)
	require.Len(t, got, 2)
	assert.Equal(t, "c", got[0].Content)
}

func TestListByPostRefusesAnInvisiblePost(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.comment(t, f.author, f.draftPost.ID, "hidden", nil)

	_, _, err := f.svc.ListByPost(context.Background(), &f.other, f.draftPost.ID,
		httpx.PageRequest{Page: 1, Limit: 10})

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

func TestByIDHidesCommentsOnInvisiblePosts(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c := f.comment(t, f.author, f.draftPost.ID, "hidden", nil)

	_, err := f.svc.ByID(context.Background(), &f.other, c.ID)

	appErr := apierr.From(err)
	assert.Equal(t, apierr.CodeNotFound, appErr.Code)
	assert.Equal(t, "comment not found", appErr.Message)
}

func TestUpdateByOwner(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Original", nil)

	got, err := f.svc.Update(context.Background(), f.other, c.ID, "  Edited  ")

	require.NoError(t, err)
	assert.Equal(t, "Edited", got.Content)
	assert.Contains(t, f.bus.names(), "comment.updated")
}

func TestUpdateByAnotherUserIsForbidden(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Original", nil)

	_, err := f.svc.Update(context.Background(), f.author, c.ID, "Hijacked")

	assert.Equal(t, apierr.CodeForbidden, apierr.From(err).Code)

	unchanged, err := f.svc.ByID(context.Background(), &f.author, c.ID)
	require.NoError(t, err)
	assert.Equal(t, "Original", unchanged.Content)
}

// A post's author is not currently granted moderation rights over comments on
// their post; only the comment's author and an administrator may edit or
// delete. This is recorded as a known limitation in the README.
func TestPostAuthorCannotEditSomeoneElsesComment(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Visitor comment", nil)

	_, err := f.svc.Update(context.Background(), f.author, c.ID, "Edited by post author")

	assert.Equal(t, apierr.CodeForbidden, apierr.From(err).Code)
}

func TestUpdateByAdminIsAllowed(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Original", nil)

	_, err := f.svc.Update(context.Background(), f.admin, c.ID, "Moderated")

	assert.NoError(t, err)
}

func TestUpdateMissingCommentReturns404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.svc.Update(context.Background(), f.author, uuid.New(), "x")

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

func TestDeleteByOwnerRemovesRepliesAndAdjustsTheCounter(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	parent := f.comment(t, f.other, f.publishedPost.ID, "Parent", nil)
	f.comment(t, f.author, f.publishedPost.ID, "Reply one", &parent.ID)
	f.comment(t, f.author, f.publishedPost.ID, "Reply two", &parent.ID)
	require.Equal(t, 3, f.store.counters[f.publishedPost.ID])

	require.NoError(t, f.svc.Delete(context.Background(), f.other, parent.ID))

	assert.Zero(t, f.store.counters[f.publishedPost.ID],
		"the counter must be reduced by the number of rows actually removed, not by one")
	assert.Contains(t, f.bus.names(), "comment.deleted")
}

func TestDeleteByAnotherUserIsForbidden(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	c := f.comment(t, f.other, f.publishedPost.ID, "Protected", nil)

	err := f.svc.Delete(context.Background(), f.author, c.ID)

	assert.Equal(t, apierr.CodeForbidden, apierr.From(err).Code)
	assert.Equal(t, 1, f.store.counters[f.publishedPost.ID], "nothing must have been removed")
}

func TestDeleteMissingCommentReturns404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	err := f.svc.Delete(context.Background(), f.author, uuid.New())

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

func TestLongContentIsPreservedExactly(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	body := strings.Repeat("x", 5000)

	c := f.comment(t, f.other, f.publishedPost.ID, body, nil)

	assert.Len(t, c.Content, 5000, "content at the CHECK limit must not be silently truncated")
}
