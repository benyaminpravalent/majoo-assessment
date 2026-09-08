package comment

import (
	"context"
	"errors"
	"strings"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/google/uuid"
)

// store is the persistence surface this service needs.
type store interface {
	Create(ctx context.Context, c *domain.Comment) error
	ByID(ctx context.Context, id uuid.UUID) (domain.Comment, error)
	AuthorAndPostOf(ctx context.Context, id uuid.UUID) (uuid.UUID, uuid.UUID, error)
	ListByPost(ctx context.Context, postID uuid.UUID, page httpx.PageRequest) ([]domain.Comment, int64, error)
	Update(ctx context.Context, id uuid.UUID, content string) (domain.Comment, error)
	Delete(ctx context.Context, id, postID uuid.UUID) error
}

// postReader is the slice of the post service this one depends on.
//
// Comments need to know whether a post is visible to the caller before
// exposing its thread, and asking the post service keeps that rule in one
// place. The dependency points comment -> post and never back.
type postReader interface {
	ByID(ctx context.Context, actor *domain.Actor, id uuid.UUID) (domain.Post, error)
}

type publisher interface {
	Publish(ctx context.Context, e events.Event)
}

// Service implements the comment use cases.
type Service struct {
	comments store
	posts    postReader
	bus      publisher
}

// NewService wires the comment service.
func NewService(comments store, posts postReader, bus publisher) *Service {
	return &Service{comments: comments, posts: posts, bus: bus}
}

// CreateInput is the validated input to Create.
type CreateInput struct {
	PostID   uuid.UUID
	ParentID *uuid.UUID
	Content  string
}

// Create adds a comment to a post.
//
// Visibility is checked before the write: commenting on someone else's
// unpublished draft must fail, and it must fail with the same 404 a read would
// give, so the endpoint does not become a way to probe for draft IDs.
func (s *Service) Create(ctx context.Context, actor domain.Actor, in CreateInput) (domain.Comment, error) {
	if _, err := s.posts.ByID(ctx, &actor, in.PostID); err != nil {
		return domain.Comment{}, err
	}

	c := domain.Comment{
		ID:       uuid.New(),
		PostID:   in.PostID,
		AuthorID: actor.UserID,
		ParentID: in.ParentID,
		Content:  strings.TrimSpace(in.Content),
	}

	if err := s.comments.Create(ctx, &c); err != nil {
		if errors.Is(err, ErrPostNotFound) {
			// The post was deleted between the visibility check above and the
			// transaction. The transaction rolled back, so nothing was written.
			return domain.Comment{}, apierr.NotFound("post")
		}
		return domain.Comment{}, err
	}

	s.bus.Publish(ctx, events.Event{
		Name:       "comment.created",
		ActorID:    actor.UserID.String(),
		SubjectID:  c.ID.String(),
		Attributes: map[string]any{"post_id": c.PostID.String()},
	})

	return c, nil
}

// ByID returns a comment if its post is visible to the actor.
func (s *Service) ByID(ctx context.Context, actor *domain.Actor, id uuid.UUID) (domain.Comment, error) {
	c, err := s.comments.ByID(ctx, id)
	if err != nil {
		return domain.Comment{}, err
	}
	if _, err := s.posts.ByID(ctx, actor, c.PostID); err != nil {
		// The post is a draft the caller may not see, so neither is its thread.
		return domain.Comment{}, apierr.NotFound("comment")
	}
	return c, nil
}

// ListByPost returns a page of a post's comments.
func (s *Service) ListByPost(ctx context.Context, actor *domain.Actor, postID uuid.UUID, page httpx.PageRequest) ([]domain.Comment, int64, error) {
	if _, err := s.posts.ByID(ctx, actor, postID); err != nil {
		return nil, 0, err
	}
	return s.comments.ListByPost(ctx, postID, page)
}

// Update edits a comment the actor owns.
func (s *Service) Update(ctx context.Context, actor domain.Actor, id uuid.UUID, content string) (domain.Comment, error) {
	authorID, _, err := s.comments.AuthorAndPostOf(ctx, id)
	if err != nil {
		return domain.Comment{}, err
	}
	if !actor.CanModify(authorID) {
		return domain.Comment{}, apierr.Forbidden("you may only modify your own comments")
	}

	c, err := s.comments.Update(ctx, id, strings.TrimSpace(content))
	if err != nil {
		return domain.Comment{}, err
	}

	s.bus.Publish(ctx, events.Event{
		Name:      "comment.updated",
		ActorID:   actor.UserID.String(),
		SubjectID: id.String(),
	})
	return c, nil
}

// Delete removes a comment the actor owns, along with its replies.
func (s *Service) Delete(ctx context.Context, actor domain.Actor, id uuid.UUID) error {
	authorID, postID, err := s.comments.AuthorAndPostOf(ctx, id)
	if err != nil {
		return err
	}
	if !actor.CanModify(authorID) {
		return apierr.Forbidden("you may only delete your own comments")
	}

	if err := s.comments.Delete(ctx, id, postID); err != nil {
		return err
	}

	s.bus.Publish(ctx, events.Event{
		Name:       "comment.deleted",
		ActorID:    actor.UserID.String(),
		SubjectID:  id.String(),
		Attributes: map[string]any{"post_id": postID.String()},
	})
	return nil
}
