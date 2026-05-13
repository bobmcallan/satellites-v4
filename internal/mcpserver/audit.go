package mcpserver

// MCP-call audit middleware. Sty_1493c077.
//
// Writes one ledger row per tool call (kind:mcp-call). Reads
// (task_walk / story_get / *_list / *_search / *_get) land with
// durability=ephemeral and an expires_at; mutations land durable.
// Audit-write failures are logged at warn and never block the
// handler's response to the caller.
//
// Sty_4db0e025 slice A3: the three substrate calls (project.GetByID,
// task.GetByID, ledger.Append) live on *client.Client via
// AuditWrite. This file owns wire-shape concerns only —
// classification, sanitisation, summarisation — and delegates the
// substrate work through the typed surface.

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
)

// AuditTagKind is the canonical tag stamped on every audit row so
// callers (timeline view, ledger filters) can scope their queries.
const AuditTagKind = client.AuditTagKindMCPCall

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
	"task_log_list":           {},
	"system_list_mcp_tools":   {},
	"system_version":          {},
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
// Holds a Client builder so each call uses the same typed surface the
// rest of the transport layer delegates through (pr_mcp_cli_shared_path).
type auditLogger struct {
	build            func() *client.Client
	logger           arbor.ILogger
	readTTL          time.Duration
	defaultProjectID string
	nowFunc          func() time.Time
}

// newAuditLogger builds an auditLogger backed by the supplied client.
// build returns the *client.Client to use for each call — passing a
// builder (rather than a value) lets test fixtures wire stores after
// construction (the orchestratorFixture pattern). readTTL defaults to
// 720h, nowFunc defaults to time.Now().UTC().
func newAuditLogger(build func() *client.Client, logger arbor.ILogger, readTTL time.Duration, defaultProjectID string, nowFunc func() time.Time) *auditLogger {
	if readTTL <= 0 {
		readTTL = 720 * time.Hour
	}
	if nowFunc == nil {
		nowFunc = func() time.Time { return time.Now().UTC() }
	}
	return &auditLogger{
		build:            build,
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
	if a == nil || a.build == nil {
		return next
	}
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		start := a.nowFunc()
		result, err := next(ctx, req)
		a.write(ctx, req, result, err, start)
		return result, err
	}
}

// write delegates substrate work to client.AuditWrite. The middleware
// owns wire-shape concerns (classification, sanitisation,
// summarisation) and the typed method owns project / workspace /
// story resolution + the ledger.Append call.
func (a *auditLogger) write(ctx context.Context, req mcpgo.CallToolRequest, result *mcpgo.CallToolResult, handlerErr error, start time.Time) {
	cli := a.build()
	if cli == nil {
		return
	}
	verb := req.Params.Name
	args := req.GetArguments()
	duration := a.nowFunc().Sub(start)

	in := client.AuditWriteInput{
		Verb:             verb,
		ProjectIDArg:     auditStringArg(args, "project_id"),
		StoryIDArg:       auditStringArg(args, "story_id"),
		TaskIDArg:        auditStringArg(args, "task_id"),
		SanitisedArgs:    sanitiseAuditArgs(args),
		DurationMS:       duration.Milliseconds(),
		ResultSummary:    auditResultSummary(result, handlerErr),
		CallerUserID:     auditCallerUserID(ctx),
		Read:             isAuditReadVerb(verb),
		ReadTTL:          a.readTTL,
		DefaultProjectID: a.defaultProjectID,
		Now:              a.nowFunc(),
	}
	if handlerErr != nil {
		in.HandlerError = handlerErr.Error()
	}

	if _, err := cli.AuditWrite(ctx, client.Caller{UserID: in.CallerUserID}, in); err != nil {
		// Never propagate; the handler already returned. Surface in logs.
		if a.logger != nil {
			a.logger.Warn().
				Str("verb", verb).
				Str("error", err.Error()).
				Msg("mcp audit row write failed")
		}
	}
}

// auditStringArg returns the string-valued arg at key, or "" when the
// arg is missing / non-string.
func auditStringArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// auditCallerUserID returns the resolved user id from ctx, or "" when
// the auth middleware didn't attach one (test paths, anonymous calls).
func auditCallerUserID(ctx context.Context) string {
	caller, ok := auth.UserFrom(ctx)
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
