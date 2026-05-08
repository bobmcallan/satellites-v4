package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/surrealdb/surrealdb.go"
	surrealmodels "github.com/surrealdb/surrealdb.go/pkg/models"
)

// SurrealAgentAPIKeyStore is the SurrealDB-backed APIKeyStore. The
// row id is the api-key id (`apk_<8hex>`); a UNIQUE index on
// key_hash powers the AuthMiddleware lookup. owner_user_id /
// project_id / workspace_id are indexed but non-unique — they back
// the agent_apikey_list filters.
//
// Story_3191fbfc — paired with handler dispatch in
// internal/mcpserver/agent_apikey_handlers.go and middleware
// fall-through in internal/mcpserver/auth.go.
type SurrealAgentAPIKeyStore struct {
	db *surrealdb.DB
}

// agentAPIKeyRow is the on-disk shape. JSON tags match APIKey so the
// public type can be returned directly.
type agentAPIKeyRow struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	ProjectID   string     `json:"project_id"`
	OwnerUserID string     `json:"owner_user_id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	KeyHash     string     `json:"key_hash"`
	KeySalt     string     `json:"key_salt"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// NewSurrealAgentAPIKeyStore wraps db with the agent_apikeys table
// created on first call. Idempotent — re-running the DEFINE
// statements is a no-op. The schema is documented in
// `ldg_c5a4c139` (the plan row) under "Schema".
func NewSurrealAgentAPIKeyStore(db *surrealdb.DB) *SurrealAgentAPIKeyStore {
	s := &SurrealAgentAPIKeyStore{db: db}
	stmts := []string{
		"DEFINE TABLE IF NOT EXISTS agent_apikeys SCHEMALESS",
		"DEFINE INDEX IF NOT EXISTS agent_apikeys_key_hash ON TABLE agent_apikeys FIELDS key_hash UNIQUE",
		"DEFINE INDEX IF NOT EXISTS agent_apikeys_owner ON TABLE agent_apikeys FIELDS owner_user_id",
		"DEFINE INDEX IF NOT EXISTS agent_apikeys_project ON TABLE agent_apikeys FIELDS project_id",
		"DEFINE INDEX IF NOT EXISTS agent_apikeys_workspace ON TABLE agent_apikeys FIELDS workspace_id",
	}
	for _, sql := range stmts {
		_, _ = surrealdb.Query[any](context.Background(), db, sql, nil)
	}
	return s
}

const agentAPIKeyCols = "meta::id(id) AS id, workspace_id, project_id, owner_user_id, name, prefix, key_hash, key_salt, status, expires_at, last_used_at, created_at"

// Create UPSERTs the row keyed by k.ID. UPSERT mirrors the user /
// session store shapes — duplicate ids replace the existing row; in
// practice the handler always passes a fresh `apk_<8hex>` so this is
// effectively an insert.
func (s *SurrealAgentAPIKeyStore) Create(ctx context.Context, k APIKey) error {
	row := keyToRow(k)
	return s.write(ctx, row)
}

// Get returns the row by id regardless of status. Archived rows
// remain queryable so audit ledger rows referencing `apikey:<id>`
// still resolve to a row a human can inspect.
func (s *SurrealAgentAPIKeyStore) Get(ctx context.Context, id string) (APIKey, error) {
	row, err := s.fetchByID(ctx, id)
	if err != nil {
		return APIKey{}, err
	}
	return rowToKey(row), nil
}

// GetByHash returns the row whose key_hash equals hashHex. Used by
// the hash-at-rest verification tests and by callers that hold the
// salted hash directly. Returns the row regardless of status — the
// AuthMiddleware path uses LookupByToken instead, which filters
// archived and expired rows up-front.
func (s *SurrealAgentAPIKeyStore) GetByHash(ctx context.Context, hashHex string) (APIKey, error) {
	sql := fmt.Sprintf("SELECT %s FROM agent_apikeys WHERE key_hash = $hash LIMIT 1", agentAPIKeyCols)
	vars := map[string]any{"hash": hashHex}
	results, err := surrealdb.Query[[]agentAPIKeyRow](ctx, s.db, sql, vars)
	if err != nil {
		return APIKey{}, fmt.Errorf("auth: agent_apikey select by hash: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return APIKey{}, ErrAPIKeyNotFound
	}
	return rowToKey((*results)[0].Result[0]), nil
}

// LookupByToken enumerates active non-expired rows, computes the
// salted hash per candidate, ConstantTimeCompare against the stored
// KeyHash, and returns the first match. The enumeration is bounded
// by tenant-active-key-count (typically a handful) and filtered
// server-side to status=active so the in-process loop never sees
// archived or otherwise-invalid rows. Per-row salt makes a single
// salted-hash index unusable for the lookup; the UNIQUE index on
// key_hash is retained for collision detection at write time.
func (s *SurrealAgentAPIKeyStore) LookupByToken(ctx context.Context, cleartext string) (APIKey, error) {
	sql := fmt.Sprintf("SELECT %s FROM agent_apikeys WHERE status = $status", agentAPIKeyCols)
	vars := map[string]any{"status": APIKeyStatusActive}
	results, err := surrealdb.Query[[]agentAPIKeyRow](ctx, s.db, sql, vars)
	if err != nil {
		return APIKey{}, fmt.Errorf("auth: agent_apikey lookup by token: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return APIKey{}, ErrAPIKeyNotFound
	}
	now := time.Now().UTC()
	for _, row := range (*results)[0].Result {
		if row.ExpiresAt != nil && now.After(*row.ExpiresAt) {
			continue
		}
		candidate := HashAPIKey(row.KeySalt, cleartext)
		if APIKeyHashesEqual(row.KeyHash, candidate) {
			return rowToKey(row), nil
		}
	}
	return APIKey{}, ErrAPIKeyNotFound
}

// List returns rows owned by ownerUserID, optionally filtered by
// projectID. Archived rows are excluded by default. Sorted newest-
// first by created_at to match V3's ordering.
func (s *SurrealAgentAPIKeyStore) List(ctx context.Context, ownerUserID string, projectID string, includeArchived bool) ([]APIKey, error) {
	clauses := []string{}
	vars := map[string]any{}
	if ownerUserID != "" {
		clauses = append(clauses, "owner_user_id = $owner")
		vars["owner"] = ownerUserID
	}
	if projectID != "" {
		clauses = append(clauses, "project_id = $project")
		vars["project"] = projectID
	}
	if !includeArchived {
		clauses = append(clauses, "status = $status")
		vars["status"] = APIKeyStatusActive
	}
	where := ""
	for i, c := range clauses {
		if i == 0 {
			where = " WHERE " + c
		} else {
			where += " AND " + c
		}
	}
	sql := fmt.Sprintf("SELECT %s FROM agent_apikeys%s ORDER BY created_at DESC", agentAPIKeyCols, where)
	results, err := surrealdb.Query[[]agentAPIKeyRow](ctx, s.db, sql, vars)
	if err != nil {
		return nil, fmt.Errorf("auth: agent_apikey list: %w", err)
	}
	if results == nil || len(*results) == 0 {
		return nil, nil
	}
	rows := (*results)[0].Result
	out := make([]APIKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, rowToKey(r))
	}
	return out, nil
}

// Delete soft-deletes by flipping Status to archived. ErrAPIKeyNotFound
// when the id is unknown so the handler can short-circuit before
// writing the audit ledger row.
func (s *SurrealAgentAPIKeyStore) Delete(ctx context.Context, id string) error {
	row, err := s.fetchByID(ctx, id)
	if err != nil {
		return err
	}
	row.Status = APIKeyStatusArchived
	return s.write(ctx, row)
}

// Touch best-effort bumps last_used_at. AuthMiddleware drops the
// returned error on the floor; the explicit return is here so unit
// tests can wire a Touch implementation that fails and assert the
// outer handler still ran.
func (s *SurrealAgentAPIKeyStore) Touch(ctx context.Context, id string, t time.Time) error {
	row, err := s.fetchByID(ctx, id)
	if err != nil {
		return err
	}
	tt := t.UTC()
	row.LastUsedAt = &tt
	return s.write(ctx, row)
}

func (s *SurrealAgentAPIKeyStore) fetchByID(ctx context.Context, id string) (agentAPIKeyRow, error) {
	sql := fmt.Sprintf("SELECT %s FROM agent_apikeys WHERE id = $rid LIMIT 1", agentAPIKeyCols)
	vars := map[string]any{"rid": surrealmodels.NewRecordID("agent_apikeys", id)}
	results, err := surrealdb.Query[[]agentAPIKeyRow](ctx, s.db, sql, vars)
	if err != nil {
		return agentAPIKeyRow{}, fmt.Errorf("auth: agent_apikey select by id: %w", err)
	}
	if results == nil || len(*results) == 0 || len((*results)[0].Result) == 0 {
		return agentAPIKeyRow{}, ErrAPIKeyNotFound
	}
	return (*results)[0].Result[0], nil
}

func (s *SurrealAgentAPIKeyStore) write(ctx context.Context, row agentAPIKeyRow) error {
	sql := "UPSERT $rid CONTENT $doc"
	vars := map[string]any{
		"rid": surrealmodels.NewRecordID("agent_apikeys", row.ID),
		"doc": row,
	}
	if _, err := surrealdb.Query[[]agentAPIKeyRow](ctx, s.db, sql, vars); err != nil {
		return fmt.Errorf("auth: agent_apikey upsert: %w", err)
	}
	return nil
}

func keyToRow(k APIKey) agentAPIKeyRow {
	return agentAPIKeyRow{
		ID:          k.ID,
		WorkspaceID: k.WorkspaceID,
		ProjectID:   k.ProjectID,
		OwnerUserID: k.OwnerUserID,
		Name:        k.Name,
		Prefix:      k.Prefix,
		KeyHash:     k.KeyHash,
		KeySalt:     k.KeySalt,
		Status:      k.Status,
		ExpiresAt:   k.ExpiresAt,
		LastUsedAt:  k.LastUsedAt,
		CreatedAt:   k.CreatedAt,
	}
}

func rowToKey(r agentAPIKeyRow) APIKey {
	return APIKey{
		ID:          r.ID,
		WorkspaceID: r.WorkspaceID,
		ProjectID:   r.ProjectID,
		OwnerUserID: r.OwnerUserID,
		Name:        r.Name,
		Prefix:      r.Prefix,
		KeyHash:     r.KeyHash,
		KeySalt:     r.KeySalt,
		Status:      r.Status,
		ExpiresAt:   r.ExpiresAt,
		LastUsedAt:  r.LastUsedAt,
		CreatedAt:   r.CreatedAt,
	}
}

// Compile-time assertion that SurrealAgentAPIKeyStore implements
// APIKeyStore.
var _ APIKeyStore = (*SurrealAgentAPIKeyStore)(nil)
