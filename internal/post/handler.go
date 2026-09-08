package post

import (
	"net/http"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/validation"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the post use cases over HTTP.
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
// These are mounted behind OptionalAuth rather than no auth at all: an
// authenticated author browsing the list must see their own drafts, so the
// handler needs the actor when one is present without requiring one.
func (h *Handler) RegisterPublicRoutes(r chi.Router) {
	r.Get("/posts", h.list)
	r.Get("/posts/{postID}", h.get)
}

// RegisterProtectedRoutes mounts the write endpoints.
func (h *Handler) RegisterProtectedRoutes(r chi.Router) {
	r.Post("/posts", h.create)
	r.Patch("/posts/{postID}", h.update)
	r.Delete("/posts/{postID}", h.delete)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
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

	status := domain.PostStatusDraft
	if req.Status != "" {
		status = domain.PostStatus(req.Status)
	}

	p, err := h.svc.Create(ctx, actor, CreateInput{
		Title:   req.Title,
		Content: req.Content,
		Status:  status,
	})
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	w.Header().Set("Location", "/api/v1/posts/"+p.ID.String())
	httpx.WriteData(ctx, w, http.StatusCreated, newPostResponse(p))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := parsePostID(r)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	p, err := h.svc.ByID(ctx, httpx.ActorPtr(ctx), id)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newPostResponse(p))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	page, err := httpx.ParsePageRequest(r)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	in := ListInput{Page: page, Search: r.URL.Query().Get("q")}
	var fields []apierr.FieldError

	if raw := r.URL.Query().Get("author_id"); raw != "" {
		authorID, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			fields = append(fields, apierr.FieldError{Field: "author_id", Message: "must be a valid UUID"})
		} else {
			in.AuthorID = &authorID
		}
	}

	if raw := r.URL.Query().Get("status"); raw != "" {
		status := domain.PostStatus(raw)
		if !status.Valid() {
			fields = append(fields, apierr.FieldError{Field: "status", Message: "must be one of: draft, published"})
		} else {
			in.Status = &status
		}
	}

	if len(in.Search) > 200 {
		fields = append(fields, apierr.FieldError{Field: "q", Message: "must be at most 200 characters long"})
	}

	if len(fields) > 0 {
		httpx.WriteError(ctx, w, apierr.Validation(fields...))
		return
	}

	posts, total, err := h.svc.List(ctx, httpx.ActorPtr(ctx), in)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteList(ctx, w, newPostResponses(posts), httpx.NewPagination(page, total))
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	id, err := parsePostID(r)
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
	if req.Title == nil && req.Content == nil && req.Status == nil {
		httpx.WriteError(ctx, w, apierr.BadRequest("request must change at least one of: title, content, status"))
		return
	}

	upd := UpdateRequest{Title: req.Title, Content: req.Content}
	if req.Status != nil {
		status := domain.PostStatus(*req.Status)
		upd.Status = &status
	}

	p, err := h.svc.Update(ctx, actor, id, upd)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newPostResponse(p))
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	id, err := parsePostID(r)
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

// parsePostID reads and validates the {postID} path parameter.
func parsePostID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "postID"))
	if err != nil {
		return uuid.Nil, apierr.Validation(apierr.FieldError{
			Field: "postID", Message: "must be a valid UUID",
		}).WithCause(err)
	}
	return id, nil
}
