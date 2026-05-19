// Package auth — agent api-key primitive (story_3191fbfc).
//
// This file holds the in-process pieces: the row shape, the store
// interface AuthMiddleware reads, the cleartext-key generator, the
// salt+sha256 hashing helpers, and the in-memory test double the
// handler / middleware unit tests wire against. The Surreal-backed
// implementation lives next door in agent_apikey_surreal.go and the
// MCP handlers in internal/mcpserver/agent_apikey_handlers.go.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// APIKeyPrefix is the cleartext key namespace prefix. Operators (and
// the Bearer-presenting agents) see `sat_<43 base64url chars>`. The
// prefix doubles as a quick visual sanity-check that a token is
// satellites-shaped before a hash lookup is attempted.
const APIKeyPrefix = "sat_"

// APIKeyPrefixCleartextLen is the number of cleartext characters the
// list verb retains in the row's `Prefix` field. Operators eyeball
// this when answering "which key was minting these ledger rows" — the
// prefix is the trade-off between human-recognisable and "no hash
// pre-image leaks". 8 chars (4 namespace + 4 of the random tail) is
// the same shape V3's REST surface used.
const APIKeyPrefixCleartextLen = 8

// APIKeyStatusActive marks a row that AuthMiddleware will accept.
// APIKeyStatusArchived is the soft-delete terminal state — the row
// remains queryable by id (so audit ledger rows referencing
// `apikey:<id>` still resolve) but the AuthMiddleware lookup misses.
const (
	APIKeyStatusActive   = "active"
	APIKeyStatusArchived = "archived"
)

// ErrAPIKeyNotFound is returned by APIKeyStore.GetByHash and
// APIKeyStore.Get when the row is absent. Callers in AuthMiddleware
// treat any non-nil error as "fall through to OAuth / session".
var ErrAPIKeyNotFound = errors.New("auth: agent api-key not found")

// APIKey is the on-disk shape of an agent api-key row. Field names
// match the JSON tags surrealdb.go marshals against. Per AC4 the
// struct MUST NOT carry a RawKey-shaped field — the cleartext key
// exists only on the `agent_apikey_create` response wire.
//
// sty_056b68f6: TaskID + AllowedVerbs bind a key to a single task's
// permission envelope. When TaskID == "" the key is project-scoped
// (legacy shape) and AllowedVerbs is ignored. When TaskID != ""
// AuthMiddleware stamps both fields onto CallerIdentity so the
// verb-allowlist gate (`AssertVerbAllowed`) can reject out-of-set
// calls per `pr_role_grid`.
type APIKey struct {
	ID           string     `json:"id"`
	WorkspaceID  string     `json:"workspace_id"`
	ProjectID    string     `json:"project_id,omitempty"`
	OwnerUserID  string     `json:"owner_user_id"`
	Name         string     `json:"name"`
	Prefix       string     `json:"prefix"`
	KeyHash      string     `json:"key_hash"`
	KeySalt      string     `json:"key_salt"`
	Status       string     `json:"status"`
	TaskID       string     `json:"task_id,omitempty"`
	AllowedVerbs []string   `json:"allowed_verbs,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// APIKeyStore is the storage surface AuthMiddleware and the MCP
// handlers consume. It is intentionally narrow — Touch is the
// fire-and-forget mutation the middleware emits, and Delete is
// soft-delete only (status flip).
//
// GetByHash returns the row whose KeyHash equals hashHex (used by
// hash-at-rest verification tests). LookupByToken is the
// AuthMiddleware path: it accepts the cleartext, enumerates active
// non-expired rows, computes the salted hash per candidate, and
// returns the first ConstantTimeCompare-equal match. Per-row salt
// makes a single salted-hash index unusable for the lookup; the
// UNIQUE index on key_hash is retained for collision detection at
// write time. Active row sets are tenant-bounded — typically a
// handful per project — so the enumeration is fine in practice.
type APIKeyStore interface {
	Create(ctx context.Context, k APIKey) error
	Get(ctx context.Context, id string) (APIKey, error)
	GetByHash(ctx context.Context, hashHex string) (APIKey, error)
	LookupByToken(ctx context.Context, cleartext string) (APIKey, error)
	List(ctx context.Context, ownerUserID string, projectID string, includeArchived bool) ([]APIKey, error)
	Delete(ctx context.Context, id string) error
	Touch(ctx context.Context, id string, t time.Time) error
	// RevokeByTaskID flips every active row whose TaskID == taskID to
	// archived and returns the count flipped. Idempotent — re-call
	// against the same id returns 0 once the rows are already
	// archived. sty_056b68f6: invoked from task_update(status=closed)
	// so a task-scoped key cannot outlive its task.
	RevokeByTaskID(ctx context.Context, taskID string) (int, error)
}

// NewAPIKeyID returns a fresh `apk_<8hex>` id. Mirrors the rest of
// the substrate's id naming (`ldg_`, `tsk_`, `sty_`).
func NewAPIKeyID() string {
	return fmt.Sprintf("apk_%s", uuid.NewString()[:8])
}

// GenerateAPIKey returns the cleartext key shape an operator copies
// out of `agent_apikey_create`'s response and the per-row salt the
// store persists. The cleartext is 32 bytes of crypto/rand encoded
// base64url (43 chars) plus the `sat_` prefix → 47 chars total.
func GenerateAPIKey() (cleartext string, salt string, err error) {
	rawKey := make([]byte, 32)
	if _, err = rand.Read(rawKey); err != nil {
		return "", "", fmt.Errorf("auth: rand.Read raw key: %w", err)
	}
	rawSalt := make([]byte, 16)
	if _, err = rand.Read(rawSalt); err != nil {
		return "", "", fmt.Errorf("auth: rand.Read salt: %w", err)
	}
	cleartext = APIKeyPrefix + base64.RawURLEncoding.EncodeToString(rawKey)
	salt = hex.EncodeToString(rawSalt)
	return cleartext, salt, nil
}

// HashAPIKey returns the hex-encoded sha256 of (saltHex bytes ||
// cleartext bytes). Storing hex (not raw bytes) keeps the field
// human-greppable in DB inspections — same shape as document
// `body_hash`. The salt is decoded first so the input to sha256 is
// the original 16-byte salt, not its 32-char hex spelling — that
// keeps the hashing scheme aligned with the description in
// review-criteria.md AC4.
func HashAPIKey(saltHex string, cleartext string) string {
	saltBytes, err := hex.DecodeString(saltHex)
	if err != nil {
		// On a malformed salt we fall back to the literal hex string
		// so collisions remain deterministic — the only producer of a
		// salt is GenerateAPIKey, so this branch is unreachable in
		// practice. Keeping it deterministic avoids panics in tests
		// that hand-craft a salt.
		saltBytes = []byte(saltHex)
	}
	h := sha256.New()
	h.Write(saltBytes)
	h.Write([]byte(cleartext))
	return hex.EncodeToString(h.Sum(nil))
}

// APIKeyHashesEqual is a constant-time hex-string compare. Wraps
// crypto/subtle.ConstantTimeCompare to give the AuthMiddleware a
// named entry point that the review-criteria checklist can grep
// against.
func APIKeyHashesEqual(want, got string) bool {
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// APIKeyCleartextPrefix returns the first APIKeyPrefixCleartextLen
// characters of the cleartext (e.g. `sat_AbCd`). Used when stamping
// `Prefix` on the row and on the `agent_apikey_create` response.
func APIKeyCleartextPrefix(cleartext string) string {
	if len(cleartext) <= APIKeyPrefixCleartextLen {
		return cleartext
	}
	return cleartext[:APIKeyPrefixCleartextLen]
}

// MemoryAgentAPIKeyStore is the in-process APIKeyStore used by
// handler / middleware unit tests. Production wires the
// SurrealAgentAPIKeyStore.
type MemoryAgentAPIKeyStore struct {
	mu   sync.RWMutex
	rows map[string]APIKey
}

// NewMemoryAgentAPIKeyStore returns an empty in-memory store.
func NewMemoryAgentAPIKeyStore() *MemoryAgentAPIKeyStore {
	return &MemoryAgentAPIKeyStore{rows: map[string]APIKey{}}
}

// Create inserts k. Duplicate ids replace the existing row to match
// the Surreal UPSERT semantics.
func (m *MemoryAgentAPIKeyStore) Create(ctx context.Context, k APIKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[k.ID] = k
	return nil
}

// Get returns the row by id regardless of status — archived rows
// remain visible by id so the delete handler can confirm a row
// exists before a no-op flip.
func (m *MemoryAgentAPIKeyStore) Get(ctx context.Context, id string) (APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if k, ok := m.rows[id]; ok {
		return k, nil
	}
	return APIKey{}, ErrAPIKeyNotFound
}

// GetByHash returns the row whose KeyHash equals hashHex
// regardless of status. Used by hash-at-rest tests and by callers
// that already know the salted hash (rare). Archived rows resolve
// — callers filter by status themselves.
func (m *MemoryAgentAPIKeyStore) GetByHash(ctx context.Context, hashHex string) (APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, k := range m.rows {
		if k.KeyHash == hashHex {
			return k, nil
		}
	}
	return APIKey{}, ErrAPIKeyNotFound
}

// LookupByToken implements the AuthMiddleware lookup path:
// enumerate active non-expired rows, compute the salted hash per
// candidate, ConstantTimeCompare against the stored KeyHash, return
// the first match. Archived or expired rows are filtered out
// up-front so they cannot match.
func (m *MemoryAgentAPIKeyStore) LookupByToken(ctx context.Context, cleartext string) (APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now().UTC()
	for _, k := range m.rows {
		if k.Status != APIKeyStatusActive {
			continue
		}
		if k.ExpiresAt != nil && now.After(*k.ExpiresAt) {
			continue
		}
		candidate := HashAPIKey(k.KeySalt, cleartext)
		if APIKeyHashesEqual(k.KeyHash, candidate) {
			return k, nil
		}
	}
	return APIKey{}, ErrAPIKeyNotFound
}

// List returns rows owned by ownerUserID, optionally filtered by
// projectID. Archived rows are excluded by default; pass
// includeArchived=true to opt in (the delete-then-list audit path).
func (m *MemoryAgentAPIKeyStore) List(ctx context.Context, ownerUserID string, projectID string, includeArchived bool) ([]APIKey, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]APIKey, 0, len(m.rows))
	for _, k := range m.rows {
		if ownerUserID != "" && k.OwnerUserID != ownerUserID {
			continue
		}
		if projectID != "" && k.ProjectID != projectID {
			continue
		}
		if !includeArchived && k.Status == APIKeyStatusArchived {
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

// Delete soft-deletes the row by flipping Status to archived.
// Returns ErrAPIKeyNotFound when the id is unknown so the handler
// can short-circuit before writing the audit ledger row.
func (m *MemoryAgentAPIKeyStore) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.rows[id]
	if !ok {
		return ErrAPIKeyNotFound
	}
	k.Status = APIKeyStatusArchived
	m.rows[id] = k
	return nil
}

// Touch bumps LastUsedAt. Errors are returned but the AuthMiddleware
// caller drops them on the floor — Touch is fire-and-forget.
func (m *MemoryAgentAPIKeyStore) Touch(ctx context.Context, id string, t time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.rows[id]
	if !ok {
		return ErrAPIKeyNotFound
	}
	tt := t.UTC()
	k.LastUsedAt = &tt
	m.rows[id] = k
	return nil
}

// RevokeByTaskID flips every active row whose TaskID == taskID to
// archived and returns the count flipped. Idempotent — re-call
// returns 0 once the rows are already archived. sty_056b68f6.
func (m *MemoryAgentAPIKeyStore) RevokeByTaskID(ctx context.Context, taskID string) (int, error) {
	if taskID == "" {
		return 0, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for id, k := range m.rows {
		if k.TaskID != taskID {
			continue
		}
		if k.Status == APIKeyStatusArchived {
			continue
		}
		k.Status = APIKeyStatusArchived
		m.rows[id] = k
		count++
	}
	return count, nil
}

// Compile-time assertion that MemoryAgentAPIKeyStore satisfies
// APIKeyStore.
var _ APIKeyStore = (*MemoryAgentAPIKeyStore)(nil)
