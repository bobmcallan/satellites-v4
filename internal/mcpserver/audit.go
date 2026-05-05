package mcpserver

// MCP-call audit middleware. Sty_1493c077.
//
// Writes one ledger row per tool call (kind:mcp-call). Reads
// (task_walk / story_get / *_list / *_search / *_get) land with
// durability=ephemeral and an expires_at; mutations land durable.
// Audit-write failures are logged at warn and never block the
// handler's response to the caller.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/task"
)

// AuditTagKind is the canonical tag stamped on every audit row so
// callers (timeline view, ledger filters) can scope their queries.
const AuditTagKind = "kind:mcp-call"

// auditMaxArgBytes is the head-truncation threshold for raw arg blobs
// before they land in the audit row's structured payload.
const auditMaxArgBytes = 4096

// auditReadVerbSuffixes name the suffixes that classify a verb as a
// read (ephemeral durability). Anything else defaults to durable.
var auditReadVerbSuffixes = []string{
	"_get", "_list", "_search", "_walk", "_recall",
	"_whoami", "_context", "_outline", "_export_walk", "_get_resolved",
	"_template_get", "_template_list", "_get_file",
}

// auditReadVerbsExact name verbs that are reads but don't match the
// suffix rule (e.g. `satellites_info`, `system_seed_run`).
var auditReadVerbsExact = map[string]struct{}{
	"satellites_info":         {},
	"task_walk":               {},
	"system_list_mcp_tools":   {},
	"agent_ephemeral_summary": {},
	"story_template_list":     {},
	"document_search":         {},
	"portal_replicate":        {},
}

// auditSecretArgKeys are scrubbed from the recorded arg payload. Match
// is case-insensitive substring on the JSON key.
var auditSecretArgKeys = []string{
	"password", "token", "api_key", "apikey", "secret", "authorization",
}

// auditLogger writes an audit ledger row for each MCP tool call.
type auditLogger struct {
	led              ledger.Store
	tasks            task.Store
	projects         project.Store
	logger           arbor.ILogger
	readTTL          time.Duration
	defaultProjectID string
	nowFunc          func() time.Time
}

// newAuditLogger builds an auditLogger. led is required; the others are
// optional — when omitted the corresponding resolution paths return
// empty (project-only fallback or unscoped row).
func newAuditLogger(led ledger.Store, tasks task.Store, projects project.Store, logger arbor.ILogger, readTTL time.Duration, defaultProjectID string, nowFunc func() time.Time) *auditLogger {
	if readTTL <= 0 {
		readTTL = 720 * time.Hour
	}
	if nowFunc == nil {
		nowFunc = func() time.Time { return time.Now().UTC() }
	}
	return &auditLogger{
		led:              led,
		tasks:            tasks,
		projects:         projects,
		logger:           logger,
		readTTL:          readTTL,
		defaultProjectID: defaultProjectID,
		nowFunc:          nowFunc,
	}
}

// middleware returns a mcpserver.ToolHandlerMiddleware that times the
// inner handler and writes an audit row from a `defer`-style after
// hook. Errors from the audit-write path do not propagate to the
// caller.
func (a *auditLogger) middleware(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	if a == nil || a.led == nil {
		return next
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := a.nowFunc()
		result, err := next(ctx, req)
		a.write(ctx, req, result, err, start)
		return result, err
	}
}

// write records one audit row. Unscoped calls (no project resolvable
// from args, no default project) are skipped at the warn level — a row
// with no workspace_id is invisible to every operator anyway.
func (a *auditLogger) write(ctx context.Context, req mcpgo.CallToolRequest, result *mcpgo.CallToolResult, handlerErr error, start time.Time) {
	verb := req.Params.Name
	args := req.GetArguments()
	sanitised := sanitiseAuditArgs(args)
	duration := a.nowFunc().Sub(start)

	projectID, workspaceID := a.resolveProjectScope(ctx, args)
	if projectID == "" {
		// No project context — system-level calls (satellites_info,
		// session_register from a fresh client). Skip without surfacing.
		return
	}
	storyID := a.resolveStoryID(ctx, args, workspaceID)

	payload := map[string]any{
		"verb":           verb,
		"arguments":      sanitised,
		"duration_ms":    duration.Milliseconds(),
		"result_summary": auditResultSummary(result, handlerErr),
		"caller_user_id": auditCallerUserID(ctx),
	}
	if handlerErr != nil {
		payload["error"] = handlerErr.Error()
	}
	structured, _ := json.Marshal(payload)

	tags := []string{AuditTagKind, "verb:" + verb}
	if storyID != "" {
		tags = append(tags, "story_id:"+storyID)
	}

	durability := ledger.DurabilityDurable
	var expiresAt *time.Time
	if isAuditReadVerb(verb) {
		durability = ledger.DurabilityEphemeral
		exp := a.nowFunc().Add(a.readTTL)
		expiresAt = &exp
	}

	entry := ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     ledger.StringPtr(storyID),
		Type:        ledger.TypeDecision,
		Tags:        tags,
		Content:     fmt.Sprintf("mcp call: %s", verb),
		Structured:  structured,
		Durability:  durability,
		ExpiresAt:   expiresAt,
		SourceType:  ledger.SourceUser,
		CreatedBy:   auditCallerUserID(ctx),
	}
	if _, err := a.led.Append(ctx, entry, a.nowFunc()); err != nil {
		// Never propagate; the handler already returned. Surface in logs.
		if a.logger != nil {
			a.logger.Warn().
				Str("verb", verb).
				Str("error", err.Error()).
				Msg("mcp audit row write failed")
		}
	}
}

// resolveProjectScope returns (project_id, workspace_id) for the call.
// Tries the call's `project_id` arg first; falls back to the server's
// default project. Workspace id is read off the resolved project.
func (a *auditLogger) resolveProjectScope(ctx context.Context, args map[string]any) (string, string) {
	if v, ok := args["project_id"].(string); ok && v != "" {
		if a.projects != nil {
			if p, err := a.projects.GetByID(ctx, v, nil); err == nil {
				return p.ID, p.WorkspaceID
			}
		}
		return v, ""
	}
	if a.defaultProjectID != "" {
		ws := ""
		if a.projects != nil {
			if p, err := a.projects.GetByID(ctx, a.defaultProjectID, nil); err == nil {
				ws = p.WorkspaceID
			}
		}
		return a.defaultProjectID, ws
	}
	return "", ""
}

// resolveStoryID stamps `story_id:<id>` when the call carries one.
// Three sources, in order: explicit `story_id` arg → `task_id` arg
// resolved through the task store → empty (project_id stamping is
// enough).
func (a *auditLogger) resolveStoryID(ctx context.Context, args map[string]any, workspaceID string) string {
	if v, ok := args["story_id"].(string); ok && v != "" {
		return v
	}
	if a.tasks == nil {
		return ""
	}
	taskID, ok := args["task_id"].(string)
	if !ok || taskID == "" {
		return ""
	}
	memberships := []string(nil)
	if workspaceID != "" {
		memberships = []string{workspaceID}
	}
	t, err := a.tasks.GetByID(ctx, taskID, memberships)
	if err != nil {
		return ""
	}
	return t.StoryID
}

// auditCallerUserID returns the resolved user id from ctx, or "" when
// the auth middleware didn't attach one (test paths, anonymous calls).
func auditCallerUserID(ctx context.Context) string {
	caller, ok := UserFrom(ctx)
	if !ok {
		return ""
	}
	return caller.UserID
}

// isAuditReadVerb returns true when verb classifies as a read (and
// hence lands ephemeral). Suffix-driven so new verbs follow the same
// rule by naming convention; an explicit allow-list covers
// non-suffixed reads.
func isAuditReadVerb(verb string) bool {
	if _, ok := auditReadVerbsExact[verb]; ok {
		return true
	}
	for _, suf := range auditReadVerbSuffixes {
		if strings.HasSuffix(verb, suf) {
			return true
		}
	}
	return false
}

// sanitiseAuditArgs returns a sanitised copy of args. Keys matching
// auditSecretArgKeys are replaced with `<redacted>`; string values
// over auditMaxArgBytes are truncated to a head sample with the total
// length appended.
func sanitiseAuditArgs(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if isSecretAuditKey(k) {
			out[k] = "<redacted>"
			continue
		}
		out[k] = truncateAuditValue(v)
	}
	return out
}

func isSecretAuditKey(key string) bool {
	low := strings.ToLower(key)
	for _, secret := range auditSecretArgKeys {
		if strings.Contains(low, secret) {
			return true
		}
	}
	return false
}

func truncateAuditValue(v any) any {
	s, ok := v.(string)
	if !ok || len(s) <= auditMaxArgBytes {
		return v
	}
	return fmt.Sprintf("%s…<truncated total_len=%d>", s[:auditMaxArgBytes], len(s))
}

// auditResultSummary returns a short string describing the handler's
// outcome. Tool errors are returned as `error: <text>`; successful
// calls fall back to the first content item's text (truncated) or a
// row count when easily extractable.
func auditResultSummary(result *mcpgo.CallToolResult, handlerErr error) string {
	if handlerErr != nil {
		return "error: " + handlerErr.Error()
	}
	if result == nil {
		return ""
	}
	if result.IsError {
		return "tool_error"
	}
	if len(result.Content) == 0 {
		return "ok"
	}
	if tc, ok := result.Content[0].(mcpgo.TextContent); ok {
		body := tc.Text
		if len(body) > 240 {
			body = body[:240] + "…"
		}
		return body
	}
	return "ok"
}
