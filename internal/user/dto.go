package user

import (
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/google/uuid"
)

// The request and response types below are the API's contract. They are
// separate from domain.User on purpose: domain.User carries PasswordHash, and
// the only reliable way to guarantee that never appears in a response is for
// the response type not to have a field for it.

type registerRequest struct {
	Email       string `json:"email"        validate:"required,email,max=254"`
	Username    string `json:"username"     validate:"required,username"`
	DisplayName string `json:"display_name" validate:"required,min=1,max=80"`
	// The maximum is auth.MaxPasswordBytes, bcrypt's input limit. Enforcing it
	// here turns an internal hashing error into a field validation message.
	Password string `json:"password" validate:"required,min=8,max=72"`
}

type loginRequest struct {
	Email string `json:"email" validate:"required,email,max=254"`
	// No min= here. Length rules belong on registration; applying them at login
	// tells an attacker something about the stored password.
	Password string `json:"password" validate:"required,max=72"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required,min=16,max=256"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required,min=16,max=256"`
}

type updateProfileRequest struct {
	DisplayName string `json:"display_name" validate:"required,min=1,max=80"`
}

// publicUser is the projection returned for someone else's profile and embedded
// as a post or comment author. It has no email field.
type publicUser struct {
	ID          uuid.UUID `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

// privateUser is the projection returned to the account owner. It adds the
// email address and the role.
type privateUser struct {
	ID          uuid.UUID   `json:"id"`
	Email       string      `json:"email"`
	Username    string      `json:"username"`
	DisplayName string      `json:"display_name"`
	Role        domain.Role `json:"role"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

type tokenResponse struct {
	AccessToken  string      `json:"access_token"`
	RefreshToken string      `json:"refresh_token"`
	TokenType    string      `json:"token_type"`
	ExpiresIn    int64       `json:"expires_in"`
	ExpiresAt    time.Time   `json:"expires_at"`
	User         privateUser `json:"user"`
}

type logoutAllResponse struct {
	SessionsRevoked int64 `json:"sessions_revoked"`
}

func newPublicUser(u domain.User) publicUser {
	return publicUser{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		CreatedAt:   u.CreatedAt,
	}
}

func newPrivateUser(u domain.User) privateUser {
	return privateUser{
		ID:          u.ID,
		Email:       u.Email,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        u.Role,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}

func newTokenResponse(p TokenPair) tokenResponse {
	return tokenResponse{
		AccessToken:  p.AccessToken,
		RefreshToken: p.RefreshToken,
		TokenType:    "Bearer",
		ExpiresIn:    p.ExpiresIn,
		ExpiresAt:    p.ExpiresAt,
		User:         newPrivateUser(p.User),
	}
}
