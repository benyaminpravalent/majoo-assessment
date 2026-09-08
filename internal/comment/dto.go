package comment

import (
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/google/uuid"
)

type createRequest struct {
	Content string `json:"content" validate:"required,min=1,max=5000"`
	// Optional. When present it must name a comment on the same post; the
	// comments_parent_same_post foreign key enforces that in the database.
	ParentID *uuid.UUID `json:"parent_id,omitempty" validate:"omitempty"`
}

type updateRequest struct {
	Content string `json:"content" validate:"required,min=1,max=5000"`
}

type authorResponse struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
}

type commentResponse struct {
	ID        uuid.UUID       `json:"id"`
	PostID    uuid.UUID       `json:"post_id"`
	ParentID  *uuid.UUID      `json:"parent_id"`
	Content   string          `json:"content"`
	Author    *authorResponse `json:"author,omitempty"`
	AuthorID  uuid.UUID       `json:"author_id"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func newCommentResponse(c domain.Comment) commentResponse {
	resp := commentResponse{
		ID:        c.ID,
		PostID:    c.PostID,
		ParentID:  c.ParentID,
		Content:   c.Content,
		AuthorID:  c.AuthorID,
		CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt,
	}
	if c.Author != nil {
		resp.Author = &authorResponse{
			ID:          c.Author.ID,
			Username:    c.Author.Username,
			DisplayName: c.Author.DisplayName,
		}
	}
	return resp
}

func newCommentResponses(comments []domain.Comment) []commentResponse {
	out := make([]commentResponse, 0, len(comments))
	for _, c := range comments {
		out = append(out, newCommentResponse(c))
	}
	return out
}
