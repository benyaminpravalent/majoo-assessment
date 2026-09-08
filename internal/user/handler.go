package user

import (
	"net/http"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/httpx"
	"github.com/bpsiregar/majoo-assessment/internal/platform/validation"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// Handler exposes the account use cases over HTTP.
//
// Handlers here do four things and nothing else: decode, validate, call the
// service, render. Every business rule and every authorisation decision lives
// in the service, so there is no way to reach one of them through a route that
// forgot to check.
type Handler struct {
	svc      *Service
	validate *validation.Validator
}

// NewHandler returns a Handler.
func NewHandler(svc *Service, validate *validation.Validator) *Handler {
	return &Handler{svc: svc, validate: validate}
}

// RegisterCredentialRoutes mounts the endpoints that accept or exchange
// credentials. The router puts them behind a much stricter rate limit than the
// rest of the API, which is why they are a separate group rather than part of
// RegisterPublicRoutes.
func (h *Handler) RegisterCredentialRoutes(r chi.Router) {
	r.Post("/auth/register", h.register)
	r.Post("/auth/login", h.login)
	r.Post("/auth/refresh", h.refresh)
}

// RegisterPublicRoutes mounts the remaining endpoints that need no bearer token.
func (h *Handler) RegisterPublicRoutes(r chi.Router) {
	r.Post("/auth/logout", h.logout)
	r.Get("/users/{userID}", h.getByID)
}

// RegisterProtectedRoutes mounts the endpoints that require a valid token.
func (h *Handler) RegisterProtectedRoutes(r chi.Router) {
	r.Post("/auth/logout-all", h.logoutAll)
	r.Get("/users/me", h.me)
	r.Patch("/users/me", h.updateMe)
}

func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req registerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}
	if err := h.validate.Struct(req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	u, err := h.svc.Register(ctx, RegisterInput{
		Email:       req.Email,
		Username:    req.Username,
		DisplayName: req.DisplayName,
		Password:    req.Password,
	})
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusCreated, newPrivateUser(u))
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req loginRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}
	if err := h.validate.Struct(req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	pair, err := h.svc.Login(ctx, req.Email, req.Password, r.UserAgent())
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newTokenResponse(pair))
}

func (h *Handler) refresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req refreshRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}
	if err := h.validate.Struct(req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	pair, err := h.svc.Refresh(ctx, req.RefreshToken, r.UserAgent())
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newTokenResponse(pair))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req logoutRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}
	if err := h.validate.Struct(req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	// Logout takes the refresh token rather than the access token and needs no
	// authentication: presenting the token is itself the proof, and a client
	// whose access token has already expired must still be able to log out.
	if err := h.svc.Logout(ctx, req.RefreshToken); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteNoContent(w)
}

func (h *Handler) logoutAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	n, err := h.svc.LogoutAll(ctx, actor)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, logoutAllResponse{SessionsRevoked: n})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	u, err := h.svc.ByID(ctx, actor.UserID)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newPrivateUser(u))
}

func (h *Handler) updateMe(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	actor, ok := httpx.Actor(ctx)
	if !ok {
		httpx.WriteError(ctx, w, apierr.Unauthorized("authentication required"))
		return
	}

	var req updateProfileRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}
	if err := h.validate.Struct(req); err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	u, err := h.svc.UpdateProfile(ctx, actor, req.DisplayName)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	httpx.WriteData(ctx, w, http.StatusOK, newPrivateUser(u))
}

func (h *Handler) getByID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		httpx.WriteError(ctx, w, apierr.Validation(apierr.FieldError{
			Field:   "userID",
			Message: "must be a valid UUID",
		}).WithCause(err))
		return
	}

	u, err := h.svc.ByID(ctx, id)
	if err != nil {
		httpx.WriteError(ctx, w, err)
		return
	}

	// Public projection: another user's email address is not the caller's
	// business, even when the caller is authenticated.
	httpx.WriteData(ctx, w, http.StatusOK, newPublicUser(u))
}
