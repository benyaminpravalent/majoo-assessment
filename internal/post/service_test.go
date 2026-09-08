package post

import (
	"context"
	"testing"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type fixture struct {
	svc    *Service
	store  *fakeStore
	bus    *recordingBus
	author domain.Actor
	other  domain.Actor
	admin  domain.Actor
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	store := newFakeStore()
	bus := &recordingBus{}
	svc := NewService(store, bus)
	svc.now = func() time.Time { return testNow }

	f := &fixture{
		svc:    svc,
		store:  store,
		bus:    bus,
		author: domain.Actor{UserID: uuid.New(), Role: domain.RoleUser},
		other:  domain.Actor{UserID: uuid.New(), Role: domain.RoleUser},
		admin:  domain.Actor{UserID: uuid.New(), Role: domain.RoleAdmin},
	}
	store.authors[f.author.UserID] = domain.UserSummary{ID: f.author.UserID, Username: "author", DisplayName: "Author"}
	store.authors[f.other.UserID] = domain.UserSummary{ID: f.other.UserID, Username: "other", DisplayName: "Other"}
	return f
}

func (f *fixture) create(t *testing.T, actor domain.Actor, title string, status domain.PostStatus) domain.Post {
	t.Helper()
	p, err := f.svc.Create(context.Background(), actor, CreateInput{
		Title: title, Content: "Body of " + title, Status: status,
	})
	require.NoError(t, err)
	return p
}

func TestCreateDraftDoesNotSetPublishedAt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	p := f.create(t, f.author, "My First Post", domain.PostStatusDraft)

	assert.Equal(t, domain.PostStatusDraft, p.Status)
	assert.Nil(t, p.PublishedAt,
		"posts_published_at_consistent requires a draft to have no publication time")
	assert.Equal(t, "my-first-post", p.Slug)
	assert.Contains(t, f.bus.names(), "post.created")
}

func TestCreatePublishedSetsPublishedAt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	p := f.create(t, f.author, "Live Post", domain.PostStatusPublished)

	require.NotNil(t, p.PublishedAt)
	assert.Equal(t, testNow, *p.PublishedAt)
	assert.Contains(t, f.bus.names(), "post.published")
}

func TestCreateTrimsWhitespace(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	p, err := f.svc.Create(context.Background(), f.author, CreateInput{
		Title: "   Spaced Title   ", Content: "  body  ", Status: domain.PostStatusDraft,
	})

	require.NoError(t, err)
	assert.Equal(t, "Spaced Title", p.Title)
	assert.Equal(t, "body", p.Content)
}

func TestCreateRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	f := newFixture(t)

	_, err := f.svc.Create(context.Background(), f.author, CreateInput{
		Title: "Title", Content: "body", Status: domain.PostStatus("archived"),
	})

	require.Error(t, err)
	assert.Equal(t, apierr.CodeValidation, apierr.From(err).Code)
}

// TestCreateRetriesWithASuffixOnSlugCollision proves the collision path is
// handled without a read-then-write race: the second attempt simply carries new
// randomness.
func TestCreateRetriesWithASuffixOnSlugCollision(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	first := f.create(t, f.author, "Duplicate Title", domain.PostStatusDraft)

	second := f.create(t, f.author, "Duplicate Title", domain.PostStatusDraft)

	assert.Equal(t, "duplicate-title", first.Slug)
	assert.NotEqual(t, first.Slug, second.Slug)
	assert.Regexp(t, `^duplicate-title-[a-z2-7]{6}$`, second.Slug)

	attempts := f.store.attempts()
	require.Len(t, attempts, 3, "the second create should try the bare slug, then a suffixed one")
	assert.Equal(t, "duplicate-title", attempts[1])
}

func TestCreateGivesUpAfterBoundedSlugAttempts(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	// Every insert collides: the retry loop must terminate rather than spin.
	f.store.createErr = slugConflict(nil)

	_, err := f.svc.Create(context.Background(), f.author, CreateInput{
		Title: "Always Taken", Content: "body", Status: domain.PostStatusDraft,
	})

	require.Error(t, err)
	assert.Equal(t, apierr.CodeConflict, apierr.From(err).Code)
	assert.Len(t, f.store.attempts(), slugAttempts)
}

func TestByIDReturnsAPublishedPostToAnyone(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Public", domain.PostStatusPublished)

	got, err := f.svc.ByID(context.Background(), nil, p.ID)

	require.NoError(t, err)
	assert.Equal(t, p.ID, got.ID)
}

// TestByIDHidesDraftsAsNotFound is a deliberate choice: 403 would confirm that
// a post with that ID exists, which is exactly what an unpublished draft must
// not disclose.
func TestByIDHidesDraftsAsNotFound(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	draft := f.create(t, f.author, "Secret Draft", domain.PostStatusDraft)

	for name, actor := range map[string]*domain.Actor{
		"anonymous":  nil,
		"other user": &f.other,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.svc.ByID(context.Background(), actor, draft.ID)

			require.Error(t, err)
			appErr := apierr.From(err)
			assert.Equal(t, apierr.CodeNotFound, appErr.Code,
				"a draft must be indistinguishable from a non-existent post")
			assert.Equal(t, 404, appErr.Status)
		})
	}
}

func TestByIDShowsDraftsToOwnerAndAdmin(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	draft := f.create(t, f.author, "Secret Draft", domain.PostStatusDraft)

	for name, actor := range map[string]domain.Actor{
		"owner": f.author,
		"admin": f.admin,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := f.svc.ByID(context.Background(), &actor, draft.ID)
			require.NoError(t, err)
			assert.Equal(t, draft.ID, got.ID)
		})
	}
}

func TestListShowsOnlyPublishedPostsToAnonymousCallers(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.create(t, f.author, "Published One", domain.PostStatusPublished)
	f.create(t, f.author, "A Draft", domain.PostStatusDraft)

	got, total, err := f.svc.List(context.Background(), nil, ListInput{
		Page: httpx.PageRequest{Page: 1, Limit: 10},
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1), total, "the total must count only visible posts, or pagination lies")
	require.Len(t, got, 1)
	assert.Equal(t, "Published One", got[0].Title)
}

func TestListShowsOwnDraftsWhenFilteringByOwnAuthorID(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.create(t, f.author, "Published One", domain.PostStatusPublished)
	f.create(t, f.author, "A Draft", domain.PostStatusDraft)

	got, total, err := f.svc.List(context.Background(), &f.author, ListInput{
		AuthorID: &f.author.UserID,
		Page:     httpx.PageRequest{Page: 1, Limit: 10},
	})

	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	assert.Len(t, got, 2)
}

func TestListRefusesToShowAnotherAuthorsDrafts(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.create(t, f.author, "A Draft", domain.PostStatusDraft)

	draft := domain.PostStatusDraft
	_, _, err := f.svc.List(context.Background(), &f.other, ListInput{
		AuthorID: &f.author.UserID,
		Status:   &draft,
		Page:     httpx.PageRequest{Page: 1, Limit: 10},
	})

	require.Error(t, err)
	assert.Equal(t, apierr.CodeForbidden, apierr.From(err).Code,
		"an explicit request for someone else's drafts is refused, not silently emptied")
}

func TestListLetsAnAdminSeeEverything(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.create(t, f.author, "Published One", domain.PostStatusPublished)
	f.create(t, f.author, "A Draft", domain.PostStatusDraft)

	draft := domain.PostStatusDraft
	got, total, err := f.svc.List(context.Background(), &f.admin, ListInput{
		Status: &draft,
		Page:   httpx.PageRequest{Page: 1, Limit: 10},
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, got, 1)
	assert.Equal(t, "A Draft", got[0].Title)
}

func TestListPaginates(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	for _, title := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} {
		f.create(t, f.author, title, domain.PostStatusPublished)
	}

	page2, total, err := f.svc.List(context.Background(), nil, ListInput{
		Page: httpx.PageRequest{Page: 2, Limit: 2},
	})

	require.NoError(t, err)
	assert.Equal(t, int64(5), total)
	require.Len(t, page2, 2)
	assert.Equal(t, "Charlie", page2[0].Title)

	// A page past the end is empty, not an error: clients paginate blindly.
	beyond, total, err := f.svc.List(context.Background(), nil, ListInput{
		Page: httpx.PageRequest{Page: 9, Limit: 2},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5), total)
	assert.Empty(t, beyond)
}

func TestListSearches(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.create(t, f.author, "Concurrency in Go", domain.PostStatusPublished)
	f.create(t, f.author, "Database Indexes", domain.PostStatusPublished)

	got, total, err := f.svc.List(context.Background(), nil, ListInput{
		Search: "concurrency",
		Page:   httpx.PageRequest{Page: 1, Limit: 10},
	})

	require.NoError(t, err)
	assert.Equal(t, int64(1), total)
	require.Len(t, got, 1)
	assert.Equal(t, "Concurrency in Go", got[0].Title)
}

func TestUpdateByOwner(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Original Title", domain.PostStatusDraft)

	newTitle := "Updated Title"
	got, err := f.svc.Update(context.Background(), f.author, p.ID, UpdateRequest{Title: &newTitle})

	require.NoError(t, err)
	assert.Equal(t, "Updated Title", got.Title)
	assert.Equal(t, "updated-title", got.Slug, "the slug follows the title")
	assert.Contains(t, f.bus.names(), "post.updated")
}

func TestUpdateByAdmin(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Original", domain.PostStatusPublished)

	newContent := "moderated"
	_, err := f.svc.Update(context.Background(), f.admin, p.ID, UpdateRequest{Content: &newContent})

	assert.NoError(t, err)
}

func TestUpdateByAnotherUserIsRejected(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	published := f.create(t, f.author, "Published", domain.PostStatusPublished)
	draft := f.create(t, f.author, "Draft Post", domain.PostStatusDraft)

	newTitle := "Hijacked"

	_, err := f.svc.Update(context.Background(), f.other, published.ID, UpdateRequest{Title: &newTitle})
	assert.Equal(t, apierr.CodeForbidden, apierr.From(err).Code,
		"a published post's existence is already public, so 403 is right")

	_, err = f.svc.Update(context.Background(), f.other, draft.ID, UpdateRequest{Title: &newTitle})
	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code,
		"a draft must answer the same way to a write as it does to a read")
}

func TestUpdateOnMissingPostReturns404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	title := "x"

	_, err := f.svc.Update(context.Background(), f.author, uuid.New(), UpdateRequest{Title: &title})

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}

// TestPublishingADraftSetsPublishedAt and its counterpart below cover the
// transitions that posts_published_at_consistent constrains.
func TestPublishingADraftSetsPublishedAt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "To Publish", domain.PostStatusDraft)
	require.Nil(t, p.PublishedAt)

	published := domain.PostStatusPublished
	got, err := f.svc.Update(context.Background(), f.author, p.ID, UpdateRequest{Status: &published})

	require.NoError(t, err)
	require.NotNil(t, got.PublishedAt)
	assert.Equal(t, testNow, *got.PublishedAt)
	assert.Contains(t, f.bus.names(), "post.published")
}

func TestUnpublishingClearsPublishedAt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "To Unpublish", domain.PostStatusPublished)
	require.NotNil(t, p.PublishedAt)

	draft := domain.PostStatusDraft
	got, err := f.svc.Update(context.Background(), f.author, p.ID, UpdateRequest{Status: &draft})

	require.NoError(t, err)
	assert.Nil(t, got.PublishedAt,
		"leaving published_at set on a draft would violate the database constraint")
}

// TestRepublishingDoesNotMovePublishedAt: an already-published post updated
// again keeps its original publication time.
func TestRepublishingDoesNotMovePublishedAt(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Already Live", domain.PostStatusPublished)
	originalTime := *p.PublishedAt

	published := domain.PostStatusPublished
	got, err := f.svc.Update(context.Background(), f.author, p.ID, UpdateRequest{Status: &published})

	require.NoError(t, err)
	require.NotNil(t, got.PublishedAt)
	assert.Equal(t, originalTime, *got.PublishedAt)
}

func TestUpdateRejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Post", domain.PostStatusDraft)

	bad := domain.PostStatus("archived")
	_, err := f.svc.Update(context.Background(), f.author, p.ID, UpdateRequest{Status: &bad})

	assert.Equal(t, apierr.CodeValidation, apierr.From(err).Code)
}

func TestUpdateRetriesWhenTheNewSlugIsTaken(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.create(t, f.author, "Taken Title", domain.PostStatusPublished)
	p := f.create(t, f.author, "Other Title", domain.PostStatusPublished)

	collide := "Taken Title"
	got, err := f.svc.Update(context.Background(), f.author, p.ID, UpdateRequest{Title: &collide})

	require.NoError(t, err)
	assert.Regexp(t, `^taken-title-[a-z2-7]{6}$`, got.Slug)
}

func TestDeleteByOwner(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Doomed", domain.PostStatusPublished)

	require.NoError(t, f.svc.Delete(context.Background(), f.author, p.ID))

	_, err := f.svc.ByID(context.Background(), &f.author, p.ID)
	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
	assert.Contains(t, f.bus.names(), "post.deleted")
}

func TestDeleteByAnotherUserIsRejected(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Protected", domain.PostStatusPublished)

	err := f.svc.Delete(context.Background(), f.other, p.ID)

	assert.Equal(t, apierr.CodeForbidden, apierr.From(err).Code)

	_, err = f.svc.ByID(context.Background(), nil, p.ID)
	assert.NoError(t, err, "a rejected delete must leave the post intact")
}

func TestDeleteTwiceReturns404(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	p := f.create(t, f.author, "Doomed", domain.PostStatusPublished)
	require.NoError(t, f.svc.Delete(context.Background(), f.author, p.ID))

	err := f.svc.Delete(context.Background(), f.author, p.ID)

	assert.Equal(t, apierr.CodeNotFound, apierr.From(err).Code)
}
