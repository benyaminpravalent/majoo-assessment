package post

import (
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/google/uuid"
)

type createRequest struct {
	Title   string `json:"title"   validate:"required,min=3,max=200"`
	Content string `json:"content" validate:"required,min=1,max=50000"`
	// Optional. Absent means draft, so a client cannot publish by accident.
	Status string `json:"status,omitempty" validate:"omitempty,oneof=draft published"`
}

// updateRequest uses pointers so that "field absent" and "field set to its zero
// value" are distinguishable. With plain strings, omitting title and sending
// title:"" would be identical, and PATCH could not be implemented correctly.
type updateRequest struct {
	Title   *string `json:"title,omitempty"   validate:"omitempty,min=3,max=200"`
	Content *string `json:"content,omitempty" validate:"omitempty,min=1,max=50000"`
	Status  *string `json:"status,omitempty"  validate:"omitempty,oneof=draft published"`
}

type authorResponse struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
}

type postResponse struct {
	ID           uuid.UUID         `json:"id"`
	Title        string            `json:"title"`
	Slug         string            `json:"slug"`
	Content      string            `json:"content"`
	Status       domain.PostStatus `json:"status"`
	CommentCount int               `json:"comment_count"`
	Author       *authorResponse   `json:"author,omitempty"`
	AuthorID     uuid.UUID         `json:"author_id"`
	PublishedAt  *time.Time        `json:"published_at"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

func newPostResponse(p domain.Post) postResponse {
	resp := postResponse{
		ID:           p.ID,
		Title:        p.Title,
		Slug:         p.Slug,
		Content:      p.Content,
		Status:       p.Status,
		CommentCount: p.CommentCount,
		AuthorID:     p.AuthorID,
		PublishedAt:  p.PublishedAt,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
	}
	if p.Author != nil {
		resp.Author = &authorResponse{
			ID:          p.Author.ID,
			Username:    p.Author.Username,
			DisplayName: p.Author.DisplayName,
		}
	}
	return resp
}

func newPostResponses(posts []domain.Post) []postResponse {
	out := make([]postResponse, 0, len(posts))
	for _, p := range posts {
		out = append(out, newPostResponse(p))
	}
	return out
}
