package auth

import (
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ErrNoSuchUser is returned when a user lookup misses. Callers should keep
// the login-fail code path timing-indistinguishable from ErrPasswordMismatch.
var ErrNoSuchUser = errors.New("auth: no such user")

// ErrSessionNotFound is returned by SessionStore.Get when the id is unknown
// or expired.
var ErrSessionNotFound = errors.New("auth: session not found")

// UserStore resolves users by email. v4 uses MemoryUserStore; story 10.9
// swaps in a SurrealDB-backed implementation.
type UserStore interface {
	GetByEmail(email string) (User, error)
}

// SessionStore creates, fetches, and deletes server-side sessions keyed by
// id. The id is returned on Create and set in the cookie.
type SessionStore interface {
	Create(userID string, ttl time.Duration) (Session, error)
	Get(id string) (Session, error)
	Delete(id string) error
}

// Session is the server-side session record. ActiveWorkspaceID is the
// workspace the session is currently scoped to — populated by the portal
// switcher (feature-order:5). Empty means "fall back to the user's first
// member workspace" at handler resolution time.
type Session struct {
	ID                string
	UserID            string
	ActiveWorkspaceID string
	ExpiresAt         time.Time
}

// Expired reports whether the session is past its expiry.
func (s Session) Expired() bool { return time.Now().After(s.ExpiresAt) }

// MemoryUserStore is the in-process user store used by v4 until 10.9 lands
// a DB-backed one. Safe for concurrent use.
type MemoryUserStore struct {
	mu      sync.RWMutex
	byEmail map[string]User
	byID    map[string]User
}

// NewMemoryUserStore returns an empty store.
func NewMemoryUserStore() *MemoryUserStore {
	return &MemoryUserStore{
		byEmail: make(map[string]User),
		byID:    make(map[string]User),
	}
}

// Add inserts or replaces a user. Indexed by both email (lowercased) and id.
func (s *MemoryUserStore) Add(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byEmail[normaliseEmail(u.Email)] = u
	s.byID[u.ID] = u
}

// GetByEmail returns the user whose email (case-insensitive) matches.
func (s *MemoryUserStore) GetByEmail(email string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byEmail[normaliseEmail(email)]
	if !ok {
		return User{}, ErrNoSuchUser
	}
	return u, nil
}

// GetByID returns the user by id.
func (s *MemoryUserStore) GetByID(id string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byID[id]
	if !ok {
		return User{}, ErrNoSuchUser
	}
	return u, nil
}

// MemorySessionStore is the in-process session store. Safe for concurrent
// use. Expired rows are lazily pruned on access.
type MemorySessionStore struct {
	mu    sync.RWMutex
	byID  map[string]Session
	clock func() time.Time
}

// NewMemorySessionStore returns an empty store using time.Now.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		byID:  make(map[string]Session),
		clock: time.Now,
	}
}

// Create mints a new session id, stores it, and returns the session.
func (s *MemorySessionStore) Create(userID string, ttl time.Duration) (Session, error) {
	sess := Session{
		ID:        uuid.NewString(),
		UserID:    userID,
		ExpiresAt: s.clock().Add(ttl),
	}
	s.mu.Lock()
	s.byID[sess.ID] = sess
	s.mu.Unlock()
	return sess, nil
}

// Get returns the session; ErrSessionNotFound when absent or expired.
func (s *MemorySessionStore) Get(id string) (Session, error) {
	s.mu.RLock()
	sess, ok := s.byID[id]
	s.mu.RUnlock()
	if !ok {
		return Session{}, ErrSessionNotFound
	}
	if s.clock().After(sess.ExpiresAt) {
		s.mu.Lock()
		delete(s.byID, id)
		s.mu.Unlock()
		return Session{}, ErrSessionNotFound
	}
	return sess, nil
}

// Delete removes the session row. Missing ids are silently ignored so
// logout is idempotent.
func (s *MemorySessionStore) Delete(id string) error {
	s.mu.Lock()
	delete(s.byID, id)
	s.mu.Unlock()
	return nil
}

func normaliseEmail(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
