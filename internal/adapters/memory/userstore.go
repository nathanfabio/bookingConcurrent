package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	appauth "github.com/nathanfabio/bookingConcurrent/internal/application/auth"
	user "github.com/nathanfabio/bookingConcurrent/internal/domain/user"
)

// Compile-time proof the fake satisfies the port.
var _ appauth.UserStore = (*UserStore)(nil)

// UserStore is the in-memory fake of the auth.UserStore port.
//
// Faithfulness notes (where it deliberately matches Postgres behavior):
//   - Email uniqueness is CASE-INSENSITIVE, mirroring the unique index on
//     lower(email) (migrations/00001). A Create whose lowercased email
//     already exists returns user.ErrEmailTaken — the same sentinel the
//     Postgres repo maps the 23505 unique violation to.
//   - The mutex around the read-then-write in Create is what makes the
//     race test meaningful: exactly one concurrent Create for the same
//     email wins, exactly like the unique index does.
type UserStore struct {
	mu      sync.Mutex
	byID    map[string]*user.User
	byEmail map[string]string // lower(email) -> userID
}

// NewUserStore builds an empty fake.
func NewUserStore() *UserStore {
	return &UserStore{
		byID:    make(map[string]*user.User),
		byEmail: make(map[string]string),
	}
}

// Create implements auth.UserStore. It assigns ID and CreatedAt.
func (s *UserStore) Create(ctx context.Context, u user.User) (user.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := strings.ToLower(u.Email)
	if _, taken := s.byEmail[key]; taken {
		return user.User{}, user.ErrEmailTaken
	}
	if u.ID == "" {
		u.ID = uuid.NewString()
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	stored := u
	s.byID[u.ID] = &stored
	s.byEmail[key] = u.ID
	return stored, nil
}

// GetByEmail implements auth.UserStore (case-insensitive match).
func (s *UserStore) GetByEmail(ctx context.Context, email string) (user.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byEmail[strings.ToLower(email)]
	if !ok {
		return user.User{}, user.ErrUserNotFound
	}
	return *s.byID[id], nil
}

// GetByID implements auth.UserStore.
func (s *UserStore) GetByID(ctx context.Context, id string) (user.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.byID[id]
	if !ok {
		return user.User{}, user.ErrUserNotFound
	}
	return *u, nil
}
