package user

import (
	"context"
	"encoding/hex"
	"sync"

	"github.com/bpsiregar/majoo-assessment/internal/domain"
	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/bpsiregar/majoo-assessment/internal/platform/events"
	"github.com/google/uuid"
)

// fakeUserStore is an in-memory userStore.
//
// A hand-written fake is used rather than a generated mock because these tests
// assert on behaviour — "a duplicate email is rejected", "the stored hash is not
// the plaintext" — and a fake that actually enforces the uniqueness constraint
// tests that behaviour, whereas a mock would only record that a method was
// called.
type fakeUserStore struct {
	mu       sync.Mutex
	byID     map[uuid.UUID]domain.User
	byEmail  map[string]uuid.UUID
	byName   map[string]uuid.UUID
	createFn func(*domain.User) error
	getErr   error
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		byID:    map[uuid.UUID]domain.User{},
		byEmail: map[string]uuid.UUID{},
		byName:  map[string]uuid.UUID{},
	}
}

func (f *fakeUserStore) Create(_ context.Context, u *domain.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createFn != nil {
		return f.createFn(u)
	}
	if _, taken := f.byEmail[u.Email]; taken {
		return apierr.Conflict("an account with this email address already exists")
	}
	if _, taken := f.byName[u.Username]; taken {
		return apierr.Conflict("this username is already taken")
	}

	f.byID[u.ID] = *u
	f.byEmail[u.Email] = u.ID
	f.byName[u.Username] = u.ID
	return nil
}

func (f *fakeUserStore) ByID(_ context.Context, id uuid.UUID) (domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.getErr != nil {
		return domain.User{}, f.getErr
	}
	u, ok := f.byID[id]
	if !ok {
		return domain.User{}, apierr.NotFound("user")
	}
	return u, nil
}

func (f *fakeUserStore) ByEmail(_ context.Context, email string) (domain.User, error) {
	f.mu.Lock()
	id, ok := f.byEmail[email]
	getErr := f.getErr
	f.mu.Unlock()

	if getErr != nil {
		return domain.User{}, getErr
	}
	if !ok {
		return domain.User{}, apierr.NotFound("user")
	}
	return f.ByID(context.Background(), id)
}

func (f *fakeUserStore) UpdateProfile(_ context.Context, id uuid.UUID, displayName string) (domain.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	u, ok := f.byID[id]
	if !ok {
		return domain.User{}, apierr.NotFound("user")
	}
	u.DisplayName = displayName
	f.byID[id] = u
	return u, nil
}

// fakeSessionStore is an in-memory sessionStore that reproduces the one
// behaviour that matters most: Rotate only succeeds against a live session, so
// a replayed refresh token is rejected exactly as the SQL predicate would
// reject it.
type fakeSessionStore struct {
	mu         sync.Mutex
	byHash     map[string]*Session
	rotateErr  error
	createErr  error
	revokeAllN int64
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{byHash: map[string]*Session{}}
}

func key(hash []byte) string { return hex.EncodeToString(hash) }

func (f *fakeSessionStore) Create(_ context.Context, s *Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return f.createErr
	}
	cp := *s
	f.byHash[key(s.TokenHash)] = &cp
	return nil
}

func (f *fakeSessionStore) ByHash(_ context.Context, hash []byte) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	s, ok := f.byHash[key(hash)]
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	return *s, nil
}

func (f *fakeSessionStore) Rotate(_ context.Context, presented []byte, next *Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.rotateErr != nil {
		return f.rotateErr
	}
	current, ok := f.byHash[key(presented)]
	if !ok || current.RevokedAt != nil {
		// Mirrors the "AND revoked_at IS NULL" guard on the real UPDATE.
		return ErrSessionNotFound
	}

	now := nowForTests()
	current.RevokedAt = &now
	current.ReplacedBy = &next.ID

	cp := *next
	f.byHash[key(next.TokenHash)] = &cp
	return nil
}

func (f *fakeSessionStore) Revoke(_ context.Context, hash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if s, ok := f.byHash[key(hash)]; ok && s.RevokedAt == nil {
		now := nowForTests()
		s.RevokedAt = &now
	}
	return nil
}

func (f *fakeSessionStore) RevokeAllForUser(_ context.Context, userID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var n int64
	for _, s := range f.byHash {
		if s.UserID == userID && s.RevokedAt == nil {
			now := nowForTests()
			s.RevokedAt = &now
			n++
		}
	}
	f.revokeAllN += n
	return n, nil
}

func (f *fakeSessionStore) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	var n int
	for _, s := range f.byHash {
		if s.RevokedAt == nil {
			n++
		}
	}
	return n
}

// recordingBus captures published events so tests can assert on the audit trail
// without starting real workers.
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
