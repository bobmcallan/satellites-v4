package auth

import (
	"context"
	"fmt"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealUserStore is the SurrealDB-backed UserStore. The user id is the
// row id; an index on the email column powers GetByEmail. Add and Update
// share the same UPSERT path so OAuth signup remains idempotent across
// satellites restarts. Story_7512783a — replaces MemoryUserStore on
// production deploys where Fly rolling deploys would otherwise wipe
// every minted user row.
type SurrealUserStore struct {
	db *surrealdb.DB
}

// authUserRow is the on-disk shape of a user row.
type authUserRow struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	DisplayName    string `json:"display_name"`
	Provider       string `json:"provider"`
	HashedPassword string `json:"hashed_password"`
}

// NewSurrealUserStore wraps db with the auth_users table. Idempotent
// DEFINE statements run on first call.
func NewSurrealUserStore(db *surrealdb.DB) *SurrealUserStore {
	s := &SurrealUserStore{db: db}
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE TABLE IF NOT EXISTS auth_users SCHEMALESS", nil)
	_, _ = surrealdb.Query[any](context.Background(), db, "DEFINE INDEX IF NOT EXISTS auth_users_email ON TABLE auth_users FIELDS email", nil)
	return s
}

const authUserCols = "meta::id(id) AS id, email, display_name, provider, hashed_password"

// Add upserts a user row. Matches MemoryUserStore.Add — never errors on
// duplicate ids; later writes win. The signature is `void` to mirror
// MemoryUserStore and avoid noisy returns in the hot OAuth signup path.
func (s *SurrealUserStore) Add(u User) {
	row := userToRow(u)
	if err := s.write(context.Background(), row); err != nil {
		// MemoryUserStore swallows errors silently; mirror the surface
		// (the upstream callers in OAuth + DevMode treat Add as
		// fire-and-forget).
		_ = err
	}
}

// Update overwrites the row matching u.ID. Returns ErrNoSuchUser when
// the id does not exist, matching the MemoryUserStore semantics.
func (s *SurrealUserStore) Update(u User) error {
	if _, err := s.GetByID(u.ID); err != nil {
		return err
	}
	return s.write(context.Background(), userToRow(u))
}

// GetByEmail returns the user whose email matches (case-insensitive via
// the column). The email field is stored normalised on Add; the query
// applies the same normalisation just in case callers don't.
func (s *SurrealUserStore) GetByEmail(email string) (User, error) {
	sql := fmt.Sprintf("SELECT %s FROM auth_users WHERE email = $email LIMIT 1", authUserCols)
	vars := map[string]any{"email": normaliseEmail(email)}
	results, err := surrealdb.Query[[]authUserRow](context.Background(), s.db, sql, vars)
	if err != nil {
		return User{}, fmt.Errorf("auth: user select by email: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return User{}, ErrNoSuchUser
	}
	return rowToUser((*results)[0].Result[0]), nil
}

// GetByID returns the user whose row id matches.
func (s *SurrealUserStore) GetByID(id string) (User, error) {
	sql := fmt.Sprintf("SELECT %s FROM auth_users WHERE id = $rid LIMIT 1", authUserCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("auth_users", id)}
	results, err := surrealdb.Query[[]authUserRow](context.Background(), s.db, sql, vars)
	if err != nil {
		return User{}, fmt.Errorf("auth: user select by id: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return User{}, ErrNoSuchUser
	}
	return rowToUser((*results)[0].Result[0]), nil
}

func (s *SurrealUserStore) write(ctx context.Context, row authUserRow) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("auth_users", row.ID),
		"doc": row,
	}
	if _, err := surrealdb.Query[[]authUserRow](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("auth: user upsert: %w", err)
	}
	return nil
}

func userToRow(u User) authUserRow {
	return authUserRow{
		ID:             u.ID,
		Email:          normaliseEmail(u.Email),
		DisplayName:    u.DisplayName,
		Provider:       u.Provider,
		HashedPassword: u.HashedPassword,
	}
}

func rowToUser(r authUserRow) User {
	return User{
		ID:             r.ID,
		Email:          r.Email,
		DisplayName:    r.DisplayName,
		Provider:       r.Provider,
		HashedPassword: r.HashedPassword,
	}
}

// Compile-time assertion that SurrealUserStore implements UserStore.
var _ UserStore = (*SurrealUserStore)(nil)
