// Package client — `satellites_info` typed method.
//
// Read-shape verb. Returns one orientation bundle describing the
// satellites server, the caller's resolved session, and a bounded
// slice of recent activity for the session-bound project. The
// orchestrator (or any agent) calls it to answer "what server am I
// talking to, who does it think I am, and what has just happened
// here?" in a single roundtrip.
//
// Per pr_mcp_cli_shared_path the wire adapters (mcpserver, httpserver,
// cmd/satellites-client) delegate here and hold no payload-assembly
// logic of their own.
//
// sty_0be97c3e — expanded shape (was {version, build, commit,
// started_at, user_email}); new shape is {server, caller,
// recent_activity}. No alias layer.

package client

import (
	"context"
	"time"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// SatellitesInfoInput carries optional caller-supplied parameters and
// the wire-layer-resolved session context. All fields are optional:
// the verb degrades gracefully when no project is bound.
type SatellitesInfoInput struct {
	// SessionID is the Mcp-Session-Id (or equivalent) the wire-layer
	// resolved for this request. Empty disables the session→project
	// lookup; caller and recent_activity then carry only identity
	// fields the JWT itself supplies.
	SessionID string
	// AuthKind is the auth.CallerIdentity.Source string the wire layer
	// resolved ("oauth:google", "apikey:apk_…", "session", …). Pure
	// passthrough — surfaced on the caller block so operators can see
	// which auth path produced the identity.
	AuthKind string
	// RecentActivityLimit caps the recent_activity ledger array.
	// 0 → DefaultRecentActivity. Above MaxRecentActivity clamps.
	RecentActivityLimit int
}

// Defaults for SatellitesInfoInput.RecentActivityLimit.
const (
	DefaultRecentActivity = 10
	MaxRecentActivity     = 20
)

// SatellitesInfoServer is the server-stamp half of the orientation
// bundle. Pure projection of compile-time + boot-time package config.
type SatellitesInfoServer struct {
	Version   string    `json:"version"`
	Build     string    `json:"build"`
	Commit    string    `json:"commit"`
	StartedAt time.Time `json:"started_at"`
}

// SatellitesInfoCaller is the session-identity half of the bundle.
// UserID + Email come from the bearer pipeline; ProjectID +
// WorkspaceID come from the session row written by project_set.
// AuthKind reports which bearer path resolved (oauth, apikey, session).
type SatellitesInfoCaller struct {
	UserID      string `json:"user_id,omitempty"`
	Email       string `json:"email,omitempty"`
	AuthKind    string `json:"auth_kind,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// SatellitesInfoLedgerRow is the slim projection of a LedgerEntry
// surfaced on satellites_info.recent_activity. Operators see what
// just happened in the bound project without paginating ledger_list.
// Content + Structured are intentionally omitted — recent_activity
// is the "headline" surface, not the full audit row.
type SatellitesInfoLedgerRow struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Tags      []string  `json:"tags,omitempty"`
	StoryID   string    `json:"story_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}

// SatellitesInfoRecentActivity bundles the project-scoped activity
// arrays. Empty when no project is bound on the caller's session.
type SatellitesInfoRecentActivity struct {
	LedgerRowsLastN []SatellitesInfoLedgerRow `json:"ledger_rows_last_n"`
}

// SatellitesInfoOutput is the wire shape of satellites_info.
type SatellitesInfoOutput struct {
	Server         SatellitesInfoServer         `json:"server"`
	Caller         SatellitesInfoCaller         `json:"caller"`
	RecentActivity SatellitesInfoRecentActivity `json:"recent_activity"`
}

// SatellitesInfo returns the build-stamped identity of the satellites
// server, the caller's resolved session identity, and a bounded
// projection of recent activity for the bound project.
func (c *Client) SatellitesInfo(ctx context.Context, caller Caller, in SatellitesInfoInput) (SatellitesInfoOutput, error) {
	out := SatellitesInfoOutput{
		Server: SatellitesInfoServer{
			Version:   config.Version,
			Build:     config.Build,
			Commit:    config.GitCommit,
			StartedAt: c.deps.StartedAt.UTC(),
		},
		Caller: SatellitesInfoCaller{
			UserID:   caller.UserID,
			Email:    caller.Email,
			AuthKind: in.AuthKind,
		},
		RecentActivity: SatellitesInfoRecentActivity{
			LedgerRowsLastN: []SatellitesInfoLedgerRow{},
		},
	}
	// Session→project lookup. Empty SessionID, missing Sessions dep, or
	// no active-project on the row all fall through to an empty Caller
	// project/workspace block + empty recent_activity. The verb itself
	// never errors on the orientation read.
	if in.SessionID == "" || c.deps.Sessions == nil || caller.UserID == "" {
		return out, nil
	}
	sess, err := c.deps.Sessions.Get(ctx, caller.UserID, in.SessionID)
	if err != nil || sess.ActiveProjectID == "" {
		return out, nil
	}
	out.Caller.ProjectID = sess.ActiveProjectID
	// Workspace resolves through the project store when wired. Missing
	// dep is non-fatal — operators still see project_id.
	if c.deps.Projects != nil {
		if p, perr := c.deps.Projects.GetByID(ctx, sess.ActiveProjectID, caller.Memberships); perr == nil {
			out.Caller.WorkspaceID = p.WorkspaceID
		}
	}
	// Recent activity. Pull the last N ledger rows in the bound project
	// using the caller's memberships. Empty memberships restrict the
	// store to the caller's own writes — operators with no workspace
	// reach see only what they themselves wrote, never sensitive data
	// in workspaces they don't own.
	if c.deps.Ledger == nil {
		return out, nil
	}
	limit := in.RecentActivityLimit
	if limit <= 0 {
		limit = DefaultRecentActivity
	}
	if limit > MaxRecentActivity {
		limit = MaxRecentActivity
	}
	rows, err := c.deps.Ledger.List(ctx, sess.ActiveProjectID, ledger.ListOptions{Limit: limit}, caller.Memberships)
	if err != nil {
		return out, nil
	}
	out.RecentActivity.LedgerRowsLastN = toLedgerRowSlim(rows)
	return out, nil
}

func toLedgerRowSlim(rows []ledger.LedgerEntry) []SatellitesInfoLedgerRow {
	if len(rows) == 0 {
		return []SatellitesInfoLedgerRow{}
	}
	slim := make([]SatellitesInfoLedgerRow, 0, len(rows))
	for _, r := range rows {
		row := SatellitesInfoLedgerRow{
			ID:        r.ID,
			Type:      r.Type,
			Tags:      r.Tags,
			CreatedAt: r.CreatedAt,
			CreatedBy: r.CreatedBy,
		}
		if r.StoryID != nil {
			row.StoryID = *r.StoryID
		}
		slim = append(slim, row)
	}
	return slim
}
