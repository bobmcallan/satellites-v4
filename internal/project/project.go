// Package project is the satellites-v4 project primitive: the top-level
// container within a workspace that every other primitive (documents,
// stories, tasks, ledger rows, repo references) scopes to. Workspace
// scoping is reserved for a later epic.
package project

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Status values for a Project. Write-once Active today; Archive is reserved
// for a later archive-story if/when it lands.
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// Project is the top-level primitive within a workspace. Every other v4
// primitive row carries a ProjectID scoping back to one of these rows, and
// every Project row carries a WorkspaceID scoping back to the tenant
// boundary (docs/architecture.md §8).
//
// The project↔git-remote binding lives on the per-project repo row
// (internal/repo) — one repo per project, keyed on (workspace_id,
// git_remote) and canonicalised on write. project_set looks up the
// remote there. sty_14dfd05b dropped the legacy git_remote column on
// this row.
//
// MCPURL is the explicit MCP connection string a user pastes into
// .mcp.json. Empty falls back to the derived form
// `<config.PublicURL>/mcp?project_id=<id>` via ResolveMCPURL.
type Project struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	Name        string    `json:"name"`
	MCPURL      string    `json:"mcp_url,omitempty"`
	OwnerUserID string    `json:"owner_user_id"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ResolveMCPURL returns p.MCPURL when non-empty, otherwise derives
// `<publicBaseURL>/mcp?project_id=<id>`. Returns empty when both the
// persisted URL and publicBaseURL are unset — the caller (portal panel,
// project_get JSON shape) renders the not-configured empty-state.
func ResolveMCPURL(p Project, publicBaseURL string) string {
	if p.MCPURL != "" {
		return p.MCPURL
	}
	publicBaseURL = strings.TrimRight(strings.TrimSpace(publicBaseURL), "/")
	if publicBaseURL == "" || p.ID == "" {
		return ""
	}
	return publicBaseURL + "/mcp?project_id=" + p.ID
}

// NewID returns a fresh project id in the canonical `proj_<8hex>` form.
func NewID() string {
	return fmt.Sprintf("proj_%s", uuid.NewString()[:8])
}
