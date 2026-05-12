package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
)

// AuditTagKindMCPCall is the canonical tag stamped on every audit
// row so callers (timeline view, ledger filters) can scope their
// queries. Mirrors mcpserver.AuditTagKind ("kind:mcp-call").
const AuditTagKindMCPCall = "kind:mcp-call"

// AuditWriteInput carries the per-call audit payload the wire layer
// assembles. The transport-side middleware classifies the verb,
// sanitises args, and summarises the result; this input bundles
// those pre-computed values so the typed method only owns project /
// workspace / story resolution + the ledger.Append call.
//
// Sty_4db0e025 slice A3: converges audit.go onto internal/client by
// owning the three substrate calls (project.GetByID, task.GetByID,
// ledger.Append) on the typed surface.
type AuditWriteInput struct {
	// Verb is the MCP tool name (e.g. "task_walk").
	Verb string
	// ProjectIDArg is the explicit project_id arg from the caller, or
	// empty when absent. Empty falls back to the server's default
	// project.
	ProjectIDArg string
	// StoryIDArg is the explicit story_id arg from the caller, or
	// empty when absent. Non-empty short-circuits the task lookup.
	StoryIDArg string
	// TaskIDArg is the explicit task_id arg, or empty. Used to walk
	// task → parent story when StoryIDArg is empty.
	TaskIDArg string
	// SanitisedArgs is the redacted+truncated args map the wire
	// layer produced from the call.
	SanitisedArgs map[string]any
	// DurationMS is the inner handler's wall-clock duration.
	DurationMS int64
	// ResultSummary is the wire-layer-formatted result string.
	ResultSummary string
	// HandlerError is the handler's error string when non-nil; empty
	// otherwise. The typed method records it in the structured
	// payload.
	HandlerError string
	// CallerUserID is the resolved caller from the auth pipeline; ""
	// when the auth middleware didn't attach one (anonymous calls,
	// test paths).
	CallerUserID string
	// Read is true when the verb classifies as a read (durability =
	// ephemeral, ExpiresAt = Now + ReadTTL). False → durable with no
	// expiry. The wire layer owns classification.
	Read bool
	// ReadTTL is the durability window for read-classified rows.
	// Zero falls back to 720h.
	ReadTTL time.Duration
	// DefaultProjectID is the server's fallback project id when
	// ProjectIDArg is empty.
	DefaultProjectID string
	// Now is the audit-write timestamp. Zero falls back to
	// time.Now().UTC().
	Now time.Time
}

// AuditWriteOutput reports whether the typed method actually wrote a
// row. Unscoped calls (no project resolvable from args, no default
// project) are skipped — the wire layer relies on this to log at the
// right level.
type AuditWriteOutput struct {
	// Skipped is true when the call carried no project context. No
	// ledger row was appended.
	Skipped bool
	// Entry is the persisted row when Skipped is false. The wire
	// layer doesn't consume it today but tests want a handle.
	Entry ledger.LedgerEntry
}

// AuditWrite records one audit row for the supplied MCP tool call.
// Resolves project / workspace from the args (or default-project
// fallback), walks task_id → parent story when needed, and appends
// the row. Returns Skipped=true when no project resolved — the
// caller may surface that at the warn level. Substrate errors from
// ledger.Append propagate to the caller; the middleware in
// mcpserver swallows them (audit failures must never block the
// handler's response).
//
// Sty_4db0e025 slice A3: owns the three substrate calls
// (project.GetByID, task.GetByID, ledger.Append) so the transport
// file imports only internal/client.
func (c *Client) AuditWrite(ctx context.Context, caller Caller, in AuditWriteInput) (AuditWriteOutput, error) {
	if c.deps.Ledger == nil {
		return AuditWriteOutput{}, errors.New("ledger store not configured")
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	readTTL := in.ReadTTL
	if readTTL <= 0 {
		readTTL = 720 * time.Hour
	}

	projectID, workspaceID := c.auditResolveProjectScope(ctx, in.ProjectIDArg, in.DefaultProjectID)
	if projectID == "" {
		// No project context — system-level call (satellites_info,
		// session_register from a fresh client). Skip without writing.
		return AuditWriteOutput{Skipped: true}, nil
	}

	storyID := c.auditResolveStoryID(ctx, in.StoryIDArg, in.TaskIDArg, workspaceID)

	payload := map[string]any{
		"verb":           in.Verb,
		"arguments":      in.SanitisedArgs,
		"duration_ms":    in.DurationMS,
		"result_summary": in.ResultSummary,
		"caller_user_id": in.CallerUserID,
	}
	if in.HandlerError != "" {
		payload["error"] = in.HandlerError
	}
	structured, _ := json.Marshal(payload)

	tags := []string{AuditTagKindMCPCall, "verb:" + in.Verb}
	if storyID != "" {
		tags = append(tags, "story_id:"+storyID)
	}

	durability := ledger.DurabilityDurable
	var expiresAt *time.Time
	if in.Read {
		durability = ledger.DurabilityEphemeral
		exp := now.Add(readTTL)
		expiresAt = &exp
	}

	entry := ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     ledger.StringPtr(storyID),
		Type:        ledger.TypeDecision,
		Tags:        tags,
		Content:     fmt.Sprintf("mcp call: %s", in.Verb),
		Structured:  structured,
		Durability:  durability,
		ExpiresAt:   expiresAt,
		SourceType:  ledger.SourceUser,
		CreatedBy:   in.CallerUserID,
	}
	written, err := c.deps.Ledger.Append(ctx, entry, now)
	if err != nil {
		return AuditWriteOutput{}, err
	}
	return AuditWriteOutput{Entry: written}, nil
}

// auditResolveProjectScope returns (project_id, workspace_id) for the
// call. Tries the explicit project_id arg first; falls back to the
// server's default project. Workspace id is read off the resolved
// project. Mirrors mcpserver.auditLogger.resolveProjectScope but
// targets internal/client's typed Project store.
func (c *Client) auditResolveProjectScope(ctx context.Context, projectIDArg, defaultProjectID string) (string, string) {
	if projectIDArg != "" {
		if c.deps.Projects != nil {
			if p, err := c.deps.Projects.GetByID(ctx, projectIDArg, nil); err == nil {
				return p.ID, p.WorkspaceID
			}
		}
		return projectIDArg, ""
	}
	if defaultProjectID != "" {
		ws := ""
		if c.deps.Projects != nil {
			if p, err := c.deps.Projects.GetByID(ctx, defaultProjectID, nil); err == nil {
				ws = p.WorkspaceID
			}
		}
		return defaultProjectID, ws
	}
	return "", ""
}

// auditResolveStoryID stamps the row's story_id when the call carries
// one. Three sources, in order: explicit story_id arg → task_id arg
// resolved through the task store → empty (project_id stamping is
// enough).
func (c *Client) auditResolveStoryID(ctx context.Context, storyIDArg, taskIDArg, workspaceID string) string {
	if storyIDArg != "" {
		return storyIDArg
	}
	if c.deps.Tasks == nil || taskIDArg == "" {
		return ""
	}
	var memberships []string
	if workspaceID != "" {
		memberships = []string{workspaceID}
	}
	t, err := c.deps.Tasks.GetByID(ctx, taskIDArg, memberships)
	if err != nil {
		return ""
	}
	return t.StoryID
}
