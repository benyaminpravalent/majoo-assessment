package post

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
)

// store is the persistence surface this service needs.
type store interface {
	Create(ctx context.Context, p *domain.Post) error
	ByID(ctx context.Context, id uuid.UUID) (domain.Post, error)
	AuthorOf(ctx context.Context, id uuid.UUID) (uuid.UUID, domain.PostStatus, error)
	List(ctx context.Context, f ListFilter) ([]domain.Post, int64, error)
	Update(ctx context.Context, id uuid.UUID, in UpdateInput) (domain.Post, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

type publisher interface {
	Publish(ctx context.Context, e events.Event)
}

// Service implements the post use cases and owns every authorisation decision
// about a post.
type Service struct {
	posts store
	bus   publisher
	now   func() time.Time
}

// NewService wires the post service.
func NewService(posts store, bus publisher) *Service {
	return &Service{posts: posts, bus: bus, now: time.Now}
}

// slugAttempts bounds the retry loop when a generated slug is already taken.
// Each retry appends fresh randomness, so three attempts is generous; the loop
// exists to survive a collision, not to iterate toward a free name.
const slugAttempts = 3

// CreateInput is the validated input to Create.
type CreateInput struct {
	Title   string
	Content string
	Status  domain.PostStatus
}

// Create stores a new post authored by the actor.
func (s *Service) Create(ctx context.Context, actor domain.Actor, in CreateInput) (domain.Post, error) {
	if !in.Status.Valid() {
		return domain.Post{}, apierr.Validation(apierr.FieldError{
			Field: "status", Message: "must be one of: draft, published",
		})
	}

	title := strings.TrimSpace(in.Title)
	p := domain.Post{
		AuthorID: actor.UserID,
		Title:    title,
		Content:  strings.TrimSpace(in.Content),
		Status:   in.Status,
	}
	if in.Status == domain.PostStatusPublished {
		// Required by posts_published_at_consistent.
		now := s.now().UTC()
		p.PublishedAt = &now
	}

	base := Slugify(title)
	var err error
	for attempt := 0; attempt < slugAttempts; attempt++ {
		// A fresh ID per attempt: reusing the previous one after a rolled-back
		// insert is harmless today but would break if the insert ever becomes
		// part of a larger transaction.
		p.ID = uuid.New()
		p.Slug = base
		if attempt > 0 {
			p.Slug = WithSuffix(base)
		}

		err = s.posts.Create(ctx, &p)
		if err == nil {
			s.publishCreated(ctx, p)
			return p, nil
		}
		if !errors.Is(err, ErrSlugTaken) {
			return domain.Post{}, err
		}
	}

	return domain.Post{}, apierr.Conflict("could not allocate a unique slug for this title").WithCause(err)
}

func (s *Service) publishCreated(ctx context.Context, p domain.Post) {
	name := "post.created"
	if p.Status == domain.PostStatusPublished {
		name = "post.published"
	}
	s.bus.Publish(ctx, events.Event{
		Name:       name,
		ActorID:    p.AuthorID.String(),
		SubjectID:  p.ID.String(),
		Attributes: map[string]any{"slug": p.Slug, "status": string(p.Status)},
	})
}

// ByID returns a post if the actor is allowed to see it.
//
// A draft is invisible to everyone except its author and an administrator, and
// the response is 404 rather than 403. Returning 403 would confirm that a post
// with that ID exists, which is exactly what an unpublished draft should not
// disclose.
func (s *Service) ByID(ctx context.Context, actor *domain.Actor, id uuid.UUID) (domain.Post, error) {
	p, err := s.posts.ByID(ctx, id)
	if err != nil {
		return domain.Post{}, err
	}
	if !s.canView(actor, p) {
		return domain.Post{}, apierr.NotFound("post")
	}
	return p, nil
}

func (s *Service) canView(actor *domain.Actor, p domain.Post) bool {
	if p.Status == domain.PostStatusPublished {
		return true
	}
	return actor != nil && actor.CanModify(p.AuthorID)
}

// ListInput is the request-level description of a listing, before visibility
// rules are applied.
type ListInput struct {
	AuthorID *uuid.UUID
	Status   *domain.PostStatus
	Search   string
	Page     httpx.PageRequest
}

// List returns a page of posts the actor is allowed to see.
//
// The visibility rule is applied by rewriting the filter, not by discarding
// rows after the query. Filtering in Go would make the pagination metadata lie:
// a page of 20 rows could return 12 after filtering, and total_items would
// count posts the caller may not see.
func (s *Service) List(ctx context.Context, actor *domain.Actor, in ListInput) ([]domain.Post, int64, error) {
	f := ListFilter{
		AuthorID: in.AuthorID,
		Status:   in.Status,
		Search:   in.Search,
		Page:     in.Page,
	}

	privileged := actor != nil && (actor.IsAdmin() ||
		(in.AuthorID != nil && *in.AuthorID == actor.UserID))

	if !privileged {
		if in.Status != nil && *in.Status != domain.PostStatusPublished {
			// Be explicit rather than silently returning an empty page: the caller
			// asked for something they are not entitled to, and a 403 is easier to
			// debug than an inexplicably empty list.
			return nil, 0, apierr.Forbidden("only your own drafts can be listed; omit the status filter or set author_id to your own id")
		}
		published := domain.PostStatusPublished
		f.Status = &published
	}

	return s.posts.List(ctx, f)
}

// UpdateRequest carries the partial change requested by a client.
type UpdateRequest struct {
	Title   *string
	Content *string
	Status  *domain.PostStatus
}

// Update applies a partial change after checking ownership.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id uuid.UUID, req UpdateRequest) (domain.Post, error) {
	authorID, currentStatus, err := s.posts.AuthorOf(ctx, id)
	if err != nil {
		return domain.Post{}, err
	}
	if err := s.authorize(actor, authorID, currentStatus); err != nil {
		return domain.Post{}, err
	}

	var in UpdateInput

	if req.Title != nil {
		title := strings.TrimSpace(*req.Title)
		in.Title = &title
		// The slug follows the title. Existing links to the old slug are not
		// preserved; the README lists slug history as a known limitation, since
		// doing it properly needs a redirect table rather than a bigger UPDATE.
		slug := Slugify(title)
		in.Slug = &slug
	}
	if req.Content != nil {
		content := strings.TrimSpace(*req.Content)
		in.Content = &content
	}

	if req.Status != nil {
		if !req.Status.Valid() {
			return domain.Post{}, apierr.Validation(apierr.FieldError{
				Field: "status", Message: "must be one of: draft, published",
			})
		}
		in.Status = req.Status
		switch {
		case *req.Status == domain.PostStatusPublished && currentStatus != domain.PostStatusPublished:
			now := s.now().UTC()
			in.PublishedAt = &now
		case *req.Status == domain.PostStatusDraft:
			// Required by posts_published_at_consistent when unpublishing.
			in.ClearPublishedAt = true
		}
	}

	updated, err := s.updateWithSlugRetry(ctx, id, in)
	if err != nil {
		return domain.Post{}, err
	}

	if req.Status != nil && *req.Status == domain.PostStatusPublished && currentStatus != domain.PostStatusPublished {
		s.bus.Publish(ctx, events.Event{
			Name:      "post.published",
			ActorID:   actor.UserID.String(),
			SubjectID: id.String(),
		})
	} else {
		s.bus.Publish(ctx, events.Event{
			Name:      "post.updated",
			ActorID:   actor.UserID.String(),
			SubjectID: id.String(),
		})
	}

	return updated, nil
}

func (s *Service) updateWithSlugRetry(ctx context.Context, id uuid.UUID, in UpdateInput) (domain.Post, error) {
	if in.Slug == nil {
		return s.posts.Update(ctx, id, in)
	}

	base := *in.Slug
	var err error
	for attempt := 0; attempt < slugAttempts; attempt++ {
		candidate := base
		if attempt > 0 {
			candidate = WithSuffix(base)
		}
		in.Slug = &candidate

		var p domain.Post
		p, err = s.posts.Update(ctx, id, in)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, ErrSlugTaken) {
			return domain.Post{}, err
		}
	}
	return domain.Post{}, apierr.Conflict("could not allocate a unique slug for this title").WithCause(err)
}

// Delete soft-deletes a post after checking ownership.
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id uuid.UUID) error {
	authorID, status, err := s.posts.AuthorOf(ctx, id)
	if err != nil {
		return err
	}
	if err := s.authorize(actor, authorID, status); err != nil {
		return err
	}

	if err := s.posts.Delete(ctx, id); err != nil {
		return err
	}

	s.bus.Publish(ctx, events.Event{
		Name:      "post.deleted",
		ActorID:   actor.UserID.String(),
		SubjectID: id.String(),
	})
	return nil
}

// authorize applies the write rule for a post: the author or an administrator.
//
// A non-owner attempting to write an unpublished draft gets 404, matching what
// a read would return, so the two responses do not combine into an existence
// oracle. A non-owner attempting to write a published post gets 403, because
// the post's existence is already public.
func (s *Service) authorize(actor domain.Actor, authorID uuid.UUID, status domain.PostStatus) error {
	if actor.CanModify(authorID) {
		return nil
	}
	if status != domain.PostStatusPublished {
		return apierr.NotFound("post")
	}
	return apierr.Forbidden("you may only modify your own posts")
}
