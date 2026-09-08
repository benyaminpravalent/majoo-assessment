// Package domain holds the persistence-independent entities and the business
// rules that are pure functions over them. It deliberately imports nothing from
// the HTTP or database layers: transport DTOs live next to their handlers and
// row-scanning lives in the repositories.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Role enumerates the authorisation roles a user can hold. The same values are
// enforced by a CHECK constraint on users.role, so the database rejects any
// role the application does not know about.
type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

// Valid reports whether r is a role the application recognises.
func (r Role) Valid() bool { return r == RoleUser || r == RoleAdmin }

// User is a registered account. PasswordHash never leaves this layer: the HTTP
// DTOs have no field for it, which makes accidental disclosure a compile error
// rather than a review finding.
type User struct {
	ID           uuid.UUID
	Email        string
	Username     string
	DisplayName  string
	PasswordHash string
	Role         Role
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Summary projects a User onto the subset that is safe to embed in public
// responses such as a post author.
func (u User) Summary() UserSummary {
	return UserSummary{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName}
}

// UserSummary is the public projection of a user, embedded in posts and
// comments so clients do not need a second round trip to render an author.
type UserSummary struct {
	ID          uuid.UUID
	Username    string
	DisplayName string
}

// PostStatus enumerates the publication states of a post. Mirrored by a CHECK
// constraint on posts.status.
type PostStatus string

const (
	PostStatusDraft     PostStatus = "draft"
	PostStatusPublished PostStatus = "published"
)

// Valid reports whether s is a status the application recognises.
func (s PostStatus) Valid() bool { return s == PostStatusDraft || s == PostStatusPublished }

// Post is a blog article. CommentCount is denormalised onto the row and kept
// correct by the same transaction that inserts or soft-deletes a comment; see
// internal/comment/repository.go. It saves a COUNT(*) subquery on every list
// page, which is the hottest read path in the API.
type Post struct {
	ID           uuid.UUID
	AuthorID     uuid.UUID
	Title        string
	Slug         string
	Content      string
	Status       PostStatus
	CommentCount int
	PublishedAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time

	// Author is populated by repository reads that join users. It is nil on
	// write paths, where the caller already knows the author.
	Author *UserSummary
}

// Comment is a reply to a post, optionally nested one or more levels under
// another comment on the same post.
type Comment struct {
	ID        uuid.UUID
	PostID    uuid.UUID
	AuthorID  uuid.UUID
	ParentID  *uuid.UUID
	Content   string
	CreatedAt time.Time
	UpdatedAt time.Time

	Author *UserSummary
}

// Actor is the authenticated principal extracted from a validated access token.
// Handlers and services take an Actor rather than a *User so that no request
// path needs to reload the account just to make an authorisation decision.
type Actor struct {
	UserID uuid.UUID
	Role   Role
}

// IsAdmin reports whether the actor holds the administrator role.
func (a Actor) IsAdmin() bool { return a.Role == RoleAdmin }

// CanModify implements the single ownership rule used across the API: you may
// change your own content, and an administrator may change anyone's.
//
// Keeping this as one exported function rather than repeating the comparison in
// each service means there is exactly one place to audit, and exactly one place
// to change if, say, a moderator role is added later.
func (a Actor) CanModify(ownerID uuid.UUID) bool {
	return a.IsAdmin() || a.UserID == ownerID
}
