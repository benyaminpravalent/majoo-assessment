package comment

import (
	"net/http"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/validation"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the comment use cases over HTTP.
type Handler struct {
	svc      *Service
	validate *validation.Validator
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, validate *validation.Validator) *Handler {
	return &Handler{svc: svc, validate: validate}
}

// RegisterPublicRoutes mounts the read endpoints.
//
// A comment thread is nested under its post, because a comment has no meaning
// outside one. The single-comment route is flat, because a client following a
// permalink has the comment ID and should not need the post ID as well.
func (h *Handler) RegisterPublicRoutes(r chi.Router) {
	r.Get("/posts/{postID}/comments", h.listByPost)
	r.Get("/comments/{commentID}", h.get)
}

// RegisterProtectedRoutes mounts the write endpoints.
func (h *Handler) RegisterProtectedRoutes(r chi.Router) {
	r.Post("/posts/{postID}/comments", h.create)
	r.Patch("/comments/{commentID}", h.update)
	r.Delete("/comments/{commentID}", h.delete)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	postID, err := parseUUIDParam(r, "postID")
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	var req createRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}
	if err := h.validate.Struct(req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	c, err := h.svc.Create(ctx, actor, CreateInput{
		PostID:   postID,
		ParentID: req.ParentID,
		Content:  req.Content,
	})
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	w.Header().Set("Location", "/api/v1/comments/"+c.ID.String())
	httpx.WriteData(ctx, w, http.StatusCreated, newCommentResponse(c))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := parseUUIDParam(r, "commentID")
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	c, err := h.svc.ByID(ctx, httpx.ActorPtr(ctx), id)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newCommentResponse(c))
}

func (h *Handler) listByPost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	postID, err := parseUUIDParam(r, "postID")
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	page, err := httpx.ParsePageRequest(r)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	comments, total, err := h.svc.ListByPost(ctx, httpx.ActorPtr(ctx), postID, page)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteList(ctx, w, newCommentResponses(comments), httpx.NewPagination(page, total))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	id, err := parseUUIDParam(r, "commentID")
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	var req updateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}
	if err := h.validate.Struct(req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	c, err := h.svc.Update(ctx, actor, id, req.Content)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newCommentResponse(c))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	id, err := parseUUIDParam(r, "commentID")
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	if err := h.svc.Delete(ctx, actor, id); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteNoContent(w)
}

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, apierr.Validation(apierr.FieldError{
			Field: name, Message: "must be a valid UUID",
		}).WithCause(err)
	}
	return id, nil
}
