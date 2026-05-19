// Package client — agent_apikey verbs (story_3191fbfc).
//
// Slice 7 of sty_f3f7bf9b lifted the business logic out of
// mcpserver/agent_apikey_handlers.go into this file. The mcpserver
// adapters now parse the request, build an AgentAPIKey*Input, call
// the typed surface, and marshal the wire envelope. Validation
// rejections ride on *AgentAPIKeyError so the adapter can stringify
// the JSON envelope verbatim — byte-identical to the pre-extraction
// wire shape.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// ErrAPIKeyStoreNotConfigured is returned when an agent_apikey verb is
// invoked against a Client whose Deps.APIKeys is nil.
var ErrAPIKeyStoreNotConfigured = errors.New("agent_apikey: store not configured")

// AgentAPIKeyError carries the JSON-envelope wire shape for the
// validation rejections the agent_apikey_* handlers previously
// emitted (invalid_expires_at, project_not_found, forbidden,
// agent_apikey_not_found). The wire layer stringifies err.Body() into
// NewToolResultError verbatim.
type AgentAPIKeyError struct {
	Code    string         // machine-readable error code
	Payload map[string]any // additional fields the envelope carries
}

// Error returns the machine-readable code. Use Body() for the wire
// envelope.
func (e *AgentAPIKeyError) Error() string { return e.Code }

// Body returns the JSON envelope the wire layer emits as the
// NewToolResultError text. Falls back to {"error": code} on marshal
// failure (cannot happen for the payload shapes used here).
func (e *AgentAPIKeyError) Body() string {
	m := map[string]any{"error": e.Code}
	for k, v := range e.Payload {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// APIKeyMetadata is the wire shape returned by AgentAPIKeyList and on
// the metadata block of AgentAPIKeyCreate. The cleartext key is
// emitted ONCE on create — under a separate `key` field — and never
// echoed on list. KeyHash and KeySalt are server-side only and are
// deliberately absent from this struct.
type APIKeyMetadata struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	ProjectID   string     `json:"project_id,omitempty"`
	OwnerUserID string     `json:"owner_user_id"`
	Name        string     `json:"name"`
	Prefix      string     `json:"prefix"`
	Status      string     `json:"status"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// ToAPIKeyMetadata projects an auth.APIKey row to the wire-safe
// metadata shape. Exported so the wire layer can call it directly
// when projecting list rows.
func ToAPIKeyMetadata(k auth.APIKey) APIKeyMetadata {
	return APIKeyMetadata{
		ID:          k.ID,
		WorkspaceID: k.WorkspaceID,
		ProjectID:   k.ProjectID,
		OwnerUserID: k.OwnerUserID,
		Name:        k.Name,
		Prefix:      k.Prefix,
		Status:      k.Status,
		ExpiresAt:   k.ExpiresAt,
		LastUsedAt:  k.LastUsedAt,
		CreatedAt:   k.CreatedAt,
	}
}

// AgentAPIKeyCreateInput captures the agent_apikey_create request
// shape. ExpiresAt is the already-parsed UTC pointer; the wire layer
// parses the RFC3339 string and emits the invalid_expires_at envelope
// itself so the parse-error fields (raw, parse_err) stay close to the
// request text. ActorSource carries the auth-resolved source string
// ("session" | "apikey" | "oauth:<provider>" | …) for stamping on
// the kind:agent-apikey-created ledger row.
//
// sty_056b68f6: TaskID + AllowedVerbs bind a key to a single task.
// When TaskID is non-empty the mint resolves the task → its agent's
// role → pr_role_grid defaults; AllowedVerbs (when supplied) must be
// a SUBSET of those defaults (AC6: shrink-but-not-expand) or the
// mint is rejected as allowed_verbs_not_subset. When AllowedVerbs is
// nil the role's full default is stored. ExpiresAt defaults to
// `now + DefaultTaskScopedKeyTTL` (6h) when caller did not supply.
type AgentAPIKeyCreateInput struct {
	Name         string
	ProjectID    string
	TaskID       string
	AllowedVerbs []string
	ExpiresAt    *time.Time
	ActorSource  string
	Now          time.Time
}

// DefaultTaskScopedKeyTTL is the default expiry for a task-scoped
// api-key whose mint did not supply an explicit ExpiresAt. The
// primary lifecycle bound is `task_update(status=closed) →
// RevokeByTaskID`; this TTL is the safety net for tasks that crash
// without closing. sty_056b68f6.
const DefaultTaskScopedKeyTTL = 6 * time.Hour

// AgentAPIKeyCreateOutput mirrors the JSON shape the wire handler
// previously emitted. The cleartext key is returned ONCE.
type AgentAPIKeyCreateOutput struct {
	ID           string     `json:"id"`
	Key          string     `json:"key"`
	Prefix       string     `json:"prefix"`
	Name         string     `json:"name"`
	OwnerUserID  string     `json:"owner_user_id"`
	ProjectID    string     `json:"project_id"`
	WorkspaceID  string     `json:"workspace_id"`
	Status       string     `json:"status"`
	TaskID       string     `json:"task_id,omitempty"`
	AllowedVerbs []string   `json:"allowed_verbs,omitempty"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
}

// AgentAPIKeyCreate mints a new agent api-key, persists the
// hash + salt + metadata, writes a kind:agent-apikey-created ledger
// row, and returns the cleartext key ONCE. The cleartext is never
// retrievable after this call.
//
// sty_056b68f6: when in.TaskID is non-empty, the mint is task-scoped:
//   - resolve the task → resolve the task's AgentID → read the agent
//     doc's `role:` field → compute the role's verb defaults.
//   - if in.AllowedVerbs is nil → use the role defaults.
//   - if in.AllowedVerbs is non-nil → assert subset (AC6: shrink-but-
//     not-expand). Reject as `allowed_verbs_not_subset` on failure.
//   - default ExpiresAt to now+DefaultTaskScopedKeyTTL when caller
//     did not supply one.
func (c *Client) AgentAPIKeyCreate(ctx context.Context, caller Caller, in AgentAPIKeyCreateInput) (AgentAPIKeyCreateOutput, error) {
	if c.deps.APIKeys == nil {
		return AgentAPIKeyCreateOutput{}, ErrAPIKeyStoreNotConfigured
	}
	if in.Name == "" {
		return AgentAPIKeyCreateOutput{}, errors.New("name is required")
	}

	memberships := caller.Memberships
	workspaceID := ""
	if in.ProjectID != "" {
		workspaceID = c.ResolveProjectWorkspaceID(ctx, in.ProjectID)
		if workspaceID == "" {
			return AgentAPIKeyCreateOutput{}, &AgentAPIKeyError{
				Code:    "project_not_found",
				Payload: map[string]any{"project_id": in.ProjectID},
			}
		}
		// Cross-tenant write guard: the project's workspace must be in
		// the caller's membership slice (or the caller is a global
		// admin).
		if !caller.GlobalAdmin && !WorkspaceInMemberships(workspaceID, memberships) {
			return AgentAPIKeyCreateOutput{}, &AgentAPIKeyError{
				Code: "forbidden",
				Payload: map[string]any{
					"reason":     "caller is not a member of the project's workspace",
					"project_id": in.ProjectID,
				},
			}
		}
	} else {
		workspaceID = c.ResolveCallerWorkspaceID(ctx, caller)
	}

	// sty_056b68f6: task-scoped path. Resolve role and clamp
	// AllowedVerbs to the role's defaults.
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	allowedVerbs := in.AllowedVerbs
	var resolvedTaskID string
	if in.TaskID != "" {
		if c.deps.Tasks == nil {
			return AgentAPIKeyCreateOutput{}, errors.New("task store not configured")
		}
		tsk, err := c.deps.Tasks.GetByID(ctx, in.TaskID, memberships)
		if err != nil {
			return AgentAPIKeyCreateOutput{}, &AgentAPIKeyError{
				Code:    "task_not_found",
				Payload: map[string]any{"task_id": in.TaskID},
			}
		}
		resolvedTaskID = tsk.ID
		role, rerr := c.resolveAgentRole(ctx, tsk.AgentID, memberships)
		if rerr != nil {
			return AgentAPIKeyCreateOutput{}, &AgentAPIKeyError{
				Code: "agent_role_unresolved",
				Payload: map[string]any{
					"task_id":  in.TaskID,
					"agent_id": tsk.AgentID,
					"reason":   rerr.Error(),
				},
			}
		}
		defaults := auth.DefaultsForRole(role)
		if defaults == nil {
			return AgentAPIKeyCreateOutput{}, &AgentAPIKeyError{
				Code:    "unknown_role",
				Payload: map[string]any{"role": role, "task_id": in.TaskID},
			}
		}
		if in.AllowedVerbs == nil {
			allowedVerbs = defaults
		} else {
			ok, offending := auth.VerbAllowlistSubset(role, in.AllowedVerbs)
			if !ok {
				return AgentAPIKeyCreateOutput{}, &AgentAPIKeyError{
					Code: "allowed_verbs_not_subset",
					Payload: map[string]any{
						"role":      role,
						"task_id":   in.TaskID,
						"offending": offending,
					},
				}
			}
			allowedVerbs = append([]string(nil), in.AllowedVerbs...)
		}
		if in.ExpiresAt == nil {
			t := now.Add(DefaultTaskScopedKeyTTL)
			in.ExpiresAt = &t
		}
	}

	cleartext, salt, err := auth.GenerateAPIKey()
	if err != nil {
		return AgentAPIKeyCreateOutput{}, fmt.Errorf("generate api key: %v", err)
	}
	row := auth.APIKey{
		ID:           auth.NewAPIKeyID(),
		WorkspaceID:  workspaceID,
		ProjectID:    in.ProjectID,
		OwnerUserID:  caller.UserID,
		Name:         in.Name,
		Prefix:       auth.APIKeyCleartextPrefix(cleartext),
		KeyHash:      auth.HashAPIKey(salt, cleartext),
		KeySalt:      salt,
		Status:       auth.APIKeyStatusActive,
		TaskID:       resolvedTaskID,
		AllowedVerbs: allowedVerbs,
		ExpiresAt:    in.ExpiresAt,
		CreatedAt:    now,
	}
	if err := c.deps.APIKeys.Create(ctx, row); err != nil {
		return AgentAPIKeyCreateOutput{}, fmt.Errorf("store create: %v", err)
	}

	// kind:agent-apikey-created ledger row. Cleartext is NOT logged
	// — only the prefix + id + actor — so the ledger never carries a
	// pre-image of the secret.
	auditPayload, _ := json.Marshal(map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"prefix":        row.Prefix,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"task_id":       row.TaskID,
		"allowed_verbs": row.AllowedVerbs,
		"actor":         caller.UserID,
		"actor_source":  in.ActorSource,
	})
	if c.deps.Ledger != nil {
		_, _ = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: workspaceID,
			ProjectID:   in.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:agent-apikey-created", "apikey:" + row.ID},
			Content:     "agent api-key minted",
			Structured:  auditPayload,
			CreatedBy:   caller.UserID,
		}, now)
	}

	return AgentAPIKeyCreateOutput{
		ID:           row.ID,
		Key:          cleartext,
		Prefix:       row.Prefix,
		Name:         row.Name,
		OwnerUserID:  row.OwnerUserID,
		ProjectID:    row.ProjectID,
		WorkspaceID:  row.WorkspaceID,
		Status:       row.Status,
		TaskID:       row.TaskID,
		AllowedVerbs: row.AllowedVerbs,
		ExpiresAt:    row.ExpiresAt,
		CreatedAt:    row.CreatedAt,
	}, nil
}

// resolveAgentRole resolves agentID via the Documents store and
// reads `role:` off the structured payload. Returns the role string
// or an error when the doc is absent / unreadable / missing role.
func (c *Client) resolveAgentRole(ctx context.Context, agentID string, memberships []string) (string, error) {
	if agentID == "" {
		return "", errors.New("agent_id is empty")
	}
	if c.deps.Documents == nil {
		return "", errors.New("document store not configured")
	}
	doc, err := c.deps.Documents.GetByID(ctx, agentID, memberships)
	if err != nil {
		return "", fmt.Errorf("get agent doc: %w", err)
	}
	if len(doc.Structured) == 0 {
		return "", errors.New("agent doc has no structured payload")
	}
	var payload map[string]any
	if err := json.Unmarshal(doc.Structured, &payload); err != nil {
		return "", fmt.Errorf("unmarshal agent structured: %w", err)
	}
	role, _ := payload["role"].(string)
	if role == "" {
		return "", errors.New("agent doc has no role field")
	}
	return role, nil
}

// AgentAPIKeyListInput captures the agent_apikey_list request shape.
type AgentAPIKeyListInput struct {
	ProjectID       string
	IncludeArchived bool
}

// AgentAPIKeyListOutput mirrors the JSON shape the wire handler
// previously emitted. Owner is the resolved owner-user filter the
// store call applied — surfaced for the wire-layer audit log; not on
// the JSON wire shape.
type AgentAPIKeyListOutput struct {
	Items     []APIKeyMetadata `json:"items"`
	Count     int              `json:"count"`
	ProjectID string           `json:"project_id"`
	Owner     string           `json:"-"`
}

// AgentAPIKeyList returns the caller's owned api-keys. Global admins
// see every key; the ProjectID arg further narrows the list. The raw
// key, key_hash, and key_salt are absent from every row.
func (c *Client) AgentAPIKeyList(ctx context.Context, caller Caller, in AgentAPIKeyListInput) (AgentAPIKeyListOutput, error) {
	if c.deps.APIKeys == nil {
		return AgentAPIKeyListOutput{}, ErrAPIKeyStoreNotConfigured
	}
	owner := caller.UserID
	if caller.GlobalAdmin {
		owner = "" // admin sees all owners
	}
	rows, err := c.deps.APIKeys.List(ctx, owner, in.ProjectID, in.IncludeArchived)
	if err != nil {
		return AgentAPIKeyListOutput{}, fmt.Errorf("store list: %v", err)
	}
	out := make([]APIKeyMetadata, 0, len(rows))
	for _, k := range rows {
		out = append(out, ToAPIKeyMetadata(k))
	}
	return AgentAPIKeyListOutput{
		Items:     out,
		Count:     len(out),
		ProjectID: in.ProjectID,
		Owner:     owner,
	}, nil
}

// AgentAPIKeyDeleteInput captures the agent_apikey_delete request
// shape. ActorSource carries the auth-resolved source string for
// stamping on the kind:agent-apikey-archived ledger row.
type AgentAPIKeyDeleteInput struct {
	ID          string
	ActorSource string
	Now         time.Time
}

// AgentAPIKeyDeleteOutput mirrors the JSON shape the wire handler
// previously emitted. AlreadyArchived signals the idempotent re-delete
// path the wire layer uses for log-line context; not on the JSON wire
// shape.
type AgentAPIKeyDeleteOutput struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	AlreadyArchived bool   `json:"-"`
}

// AgentAPIKeyDelete soft-deletes the row and writes a
// kind:agent-apikey-archived ledger row. Cross-owner deletes are
// forbidden unless the caller is a global admin. Re-deleting an
// already-archived row is a no-op that still returns the archived
// shape so callers needn't differentiate.
func (c *Client) AgentAPIKeyDelete(ctx context.Context, caller Caller, in AgentAPIKeyDeleteInput) (AgentAPIKeyDeleteOutput, error) {
	if c.deps.APIKeys == nil {
		return AgentAPIKeyDeleteOutput{}, ErrAPIKeyStoreNotConfigured
	}
	if in.ID == "" {
		return AgentAPIKeyDeleteOutput{}, errors.New("id is required")
	}
	row, err := c.deps.APIKeys.Get(ctx, in.ID)
	if err != nil {
		if errors.Is(err, auth.ErrAPIKeyNotFound) {
			return AgentAPIKeyDeleteOutput{}, &AgentAPIKeyError{
				Code:    "agent_apikey_not_found",
				Payload: map[string]any{"id": in.ID},
			}
		}
		return AgentAPIKeyDeleteOutput{}, fmt.Errorf("store get: %v", err)
	}
	if row.OwnerUserID != caller.UserID && !caller.GlobalAdmin {
		return AgentAPIKeyDeleteOutput{}, &AgentAPIKeyError{
			Code: "forbidden",
			Payload: map[string]any{
				"reason": "caller does not own the api-key",
				"id":     in.ID,
			},
		}
	}
	if row.Status == auth.APIKeyStatusArchived {
		// Idempotent: re-deleting is a no-op but still returns the
		// archived shape so callers needn't differentiate.
		return AgentAPIKeyDeleteOutput{
			ID:              row.ID,
			Status:          row.Status,
			AlreadyArchived: true,
		}, nil
	}

	if err := c.deps.APIKeys.Delete(ctx, in.ID); err != nil {
		return AgentAPIKeyDeleteOutput{}, fmt.Errorf("store delete: %v", err)
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	auditPayload, _ := json.Marshal(map[string]any{
		"id":            row.ID,
		"name":          row.Name,
		"prefix":        row.Prefix,
		"owner_user_id": row.OwnerUserID,
		"project_id":    row.ProjectID,
		"workspace_id":  row.WorkspaceID,
		"actor":         caller.UserID,
		"actor_source":  in.ActorSource,
	})
	if c.deps.Ledger != nil {
		_, _ = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: row.WorkspaceID,
			ProjectID:   row.ProjectID,
			Type:        ledger.TypeDecision,
			Tags:        []string{"kind:agent-apikey-archived", "apikey:" + row.ID, "actor:" + caller.UserID},
			Content:     "agent api-key archived",
			Structured:  auditPayload,
			CreatedBy:   caller.UserID,
		}, now)
	}

	return AgentAPIKeyDeleteOutput{
		ID:     row.ID,
		Status: auth.APIKeyStatusArchived,
	}, nil
}
