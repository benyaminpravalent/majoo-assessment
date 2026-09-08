package post

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/google/uuid"
)

// fakeStore is an in-memory post store.
//
// It reproduces the two behaviours the service actually depends on: the partial
// unique index on live slugs, and the filtering the List query performs. That
// makes the visibility tests meaningful — the service's job is to rewrite the
// filter, and the fake proves the rewritten filter selects the right rows.
type fakeStore struct {
	mu      sync.Mutex
	posts   map[uuid.UUID]domain.Post
	slugs   map[string]uuid.UUID
	authors map[uuid.UUID]domain.UserSummary

	createErr error
	updateErr error
	deleteErr error

	// createCalls records every slug the service attempted, so the retry
	// behaviour can be asserted directly.
	createCalls []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		posts:   map[uuid.UUID]domain.Post{},
		slugs:   map[string]uuid.UUID{},
		authors: map[uuid.UUID]domain.UserSummary{},
	}
}

func (f *fakeStore) Create(_ context.Context, p *domain.Post) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.createCalls = append(f.createCalls, p.Slug)
	if f.createErr != nil {
		return f.createErr
	}
	if _, taken := f.slugs[p.Slug]; taken {
		return slugConflict(nil)
	}

	author := f.authors[p.AuthorID]
	p.Author = &author
	f.posts[p.ID] = *p
	f.slugs[p.Slug] = p.ID
	return nil
}

func (f *fakeStore) ByID(_ context.Context, id uuid.UUID) (domain.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.posts[id]
	if !ok {
		return domain.Post{}, apierr.NotFound("post")
	}
	return p, nil
}

func (f *fakeStore) AuthorOf(_ context.Context, id uuid.UUID) (uuid.UUID, domain.PostStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	p, ok := f.posts[id]
	if !ok {
		return uuid.Nil, "", apierr.NotFound("post")
	}
	return p.AuthorID, p.Status, nil
}

func (f *fakeStore) List(_ context.Context, filter ListFilter) ([]domain.Post, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	matched := make([]domain.Post, 0, len(f.posts))
	for _, p := range f.posts {
		if filter.AuthorID != nil && p.AuthorID != *filter.AuthorID {
			continue
		}
		if filter.Status != nil && p.Status != *filter.Status {
			continue
		}
		if q := strings.TrimSpace(filter.Search); q != "" &&
			!strings.Contains(strings.ToLower(p.Title+" "+p.Content), strings.ToLower(q)) {
			continue
		}
		matched = append(matched, p)
	}

	// Deterministic order so pagination assertions are stable.
	sort.Slice(matched, func(i, j int) bool { return matched[i].Title < matched[j].Title })

	total := int64(len(matched))
	start := filter.Page.Offset()
	if start > len(matched) {
		start = len(matched)
	}
	end := start + filter.Page.Limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[start:end], total, nil
}

func (f *fakeStore) Update(_ context.Context, id uuid.UUID, in UpdateInput) (domain.Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updateErr != nil {
		return domain.Post{}, f.updateErr
	}
	p, ok := f.posts[id]
	if !ok {
		return domain.Post{}, apierr.NotFound("post")
	}

	if in.Slug != nil && *in.Slug != p.Slug {
		if _, taken := f.slugs[*in.Slug]; taken {
			return domain.Post{}, slugConflict(nil)
		}
		delete(f.slugs, p.Slug)
		p.Slug = *in.Slug
		f.slugs[p.Slug] = id
	}
	if in.Title != nil {
		p.Title = *in.Title
	}
	if in.Content != nil {
		p.Content = *in.Content
	}
	if in.Status != nil {
		p.Status = *in.Status
	}
	switch {
	case in.ClearPublishedAt:
		p.PublishedAt = nil
	case in.PublishedAt != nil:
		p.PublishedAt = in.PublishedAt
	}

	f.posts[id] = p
	return p, nil
}

func (f *fakeStore) Delete(_ context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.deleteErr != nil {
		return f.deleteErr
	}
	p, ok := f.posts[id]
	if !ok {
		return apierr.NotFound("post")
	}
	delete(f.posts, id)
	delete(f.slugs, p.Slug)
	return nil
}

func (f *fakeStore) attempts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.createCalls...)
}

// recordingBus captures published events for assertions.
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
