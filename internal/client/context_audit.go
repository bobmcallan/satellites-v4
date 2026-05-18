// Package client — context-fetch audit (sty_9f658001 slice 1: R1).
//
// emitContextFetch writes one kind:context-fetch evidence row per
// orientation-verb call. The row records per-section bytes, hashes,
// scopes, and embedded project-scoped id literals, then runs R1 —
// the first mechanical drift rule: any system-tier section whose
// embedded_refs is non-empty fires audit:r1_fail.
//
// Per pr_mcp_cli_shared_path the helper lives on *Client; transport
// handlers (mcpserver / httpserver) stamp ctx with the originating
// verb name via WithOriginVerb before calling the typed method.
//
// Per the plan-review verdict (ldg_d947b606): manual Events.Publish
// for the ledger.append event is intentionally omitted —
// internal/wshandler/translate.go already synthesises ledger.append
// WireEvents on every ledger CREATE via SurrealLive, so manual
// emission would duplicate. Cf pr_no_unrequested_compat.
package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// AuditTagKindContextFetch is the canonical tag stamped on every
// context-fetch evidence row so callers (the portal panel, ledger
// filters, the development_reviewer rubric) can scope their queries.
const AuditTagKindContextFetch = "kind:context-fetch"

// Origin-scope enum values stamped on each ContextSection.
const (
	OriginScopeSystem    = "system"
	OriginScopeWorkspace = "workspace"
	OriginScopeProject   = "project"
	OriginScopeUnscoped  = "unscoped"
)

// embeddedRefPattern matches the substrate id literals R1 cares about.
// Width-bound (8 hex chars) so partial id-like strings don't false-fire.
var embeddedRefPattern = regexp.MustCompile(`proj_[0-9a-f]{8}|sty_[0-9a-f]{8}|wksp_[0-9a-f]{8}|task_[0-9a-f]{8}`)

// contextAuditChannelCap bounds the async emission queue. Backpressure
// drops a row + logs at warn once; no retries.
const contextAuditChannelCap = 64

// ContextSection is one section of an orientation-verb response body.
// Name is the JSON key path (e.g. "intent_body", "principles[0]");
// Bytes is the UTF-8 length of the rendered content; Hash is the
// sha256 hex first-16 of the same content; OriginScope is one of
// the OriginScope* constants; EmbeddedRefs is the set of substrate
// id literals embedded in the section body.
type ContextSection struct {
	Name         string   `json:"name"`
	Bytes        int      `json:"bytes"`
	Hash         string   `json:"hash"`
	OriginScope  string   `json:"origin_scope"`
	EmbeddedRefs []string `json:"embedded_refs,omitempty"`
}

// ContextFetchInput carries the per-call audit payload assembled by
// the section introspectors. The emitContextFetch worker turns this
// into a ledger row.
type ContextFetchInput struct {
	Verb        string
	ProjectID   string
	WorkspaceID string
	StoryID     string
	ArgsHash    string
	Sections    []ContextSection
	Now         time.Time
	CallerID    string
}

// R1Violation captures one offending section that failed R1.
type R1Violation struct {
	Section string   `json:"section"`
	Scope   string   `json:"scope"`
	Refs    []string `json:"refs"`
}

// originVerbKey is the context key the transport layer stamps with
// the verb name (see WithOriginVerb / OriginVerbFromContext).
type originVerbKey struct{}

// WithOriginVerb returns a context derived from parent that carries
// verb as the originating MCP verb name. Transport handlers call this
// before invoking the typed *Client method when the verb name is not
// recoverable from the typed call alone (e.g. DocumentGet collapses
// agent_get / contract_get / document_get / principle_get / reviewer_get
// / role_get / skill_get into one method).
func WithOriginVerb(ctx context.Context, verb string) context.Context {
	if verb == "" {
		return ctx
	}
	return context.WithValue(ctx, originVerbKey{}, verb)
}

// OriginVerbFromContext returns the verb name stamped on ctx by
// WithOriginVerb, or empty string when none is set.
func OriginVerbFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(originVerbKey{}).(string); ok {
		return v
	}
	return ""
}

// contextAuditWorker runs the bounded-queue drain. Single goroutine
// per Client; lazily started by ensureContextAuditWorker on the first
// emit call.
//
// inflight tracks rows currently in the pipeline — incremented on a
// successful enqueue and decremented after the worker's Ledger.Append
// returns. FlushContextAudit waits on inflight (not on queue length),
// because the worker reads a job — and thus drops queue length — before
// Append completes; iter-1's len(queue)==0 proxy was racy under -race
// (4/10 iterations dropped). wg tracks the worker goroutine's lifetime
// only and gates StopContextAudit. Cf pr_root_cause, pr_local_iteration.
type contextAuditWorker struct {
	queue    chan contextAuditJob
	wg       sync.WaitGroup
	inflight sync.WaitGroup
	once     sync.Once
}

type contextAuditJob struct {
	entry ledger.LedgerEntry
	now   time.Time
}

// ensureContextAuditWorker lazily starts the audit worker. The worker
// drains c.audit.queue and calls c.deps.Ledger.Append. Tests call
// FlushContextAudit to wait for in-flight Append calls to complete.
func (c *Client) ensureContextAuditWorker() {
	c.auditInit.Do(func() {
		c.audit = &contextAuditWorker{
			queue: make(chan contextAuditJob, contextAuditChannelCap),
		}
		c.audit.wg.Add(1)
		go func() {
			defer c.audit.wg.Done()
			for job := range c.audit.queue {
				if c.deps.Ledger != nil {
					_, _ = c.deps.Ledger.Append(context.Background(), job.entry, job.now)
				}
				c.audit.inflight.Done()
			}
		}()
	})
}

// FlushContextAudit waits for every enqueued context-audit row to be
// appended to the ledger. Tests call this before asserting against
// ledger_list output. Gating on the in-flight WaitGroup (not on the
// queue length) makes the wait deterministic under -race — the worker
// reads a job, drops queue length, and only then calls Ledger.Append;
// the earlier len(queue)==0 proxy raced with the Append.
func (c *Client) FlushContextAudit(ctx context.Context) error {
	if c.audit == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		c.audit.inflight.Wait()
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = time.After(time.Until(dl))
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-deadline:
		return fmt.Errorf("context audit flush timed out")
	}
}

// StopContextAudit closes the audit queue and waits for the worker to
// exit. Wired into a future shutdown sequence; left exported so tests
// can deterministically tear down the worker.
func (c *Client) StopContextAudit() {
	if c.audit == nil {
		return
	}
	close(c.audit.queue)
	c.audit.wg.Wait()
	c.audit = nil
	c.auditInit = sync.Once{}
}

// evaluateR1 walks sections and returns the R1 violations + the
// audit:r1_pass | audit:r1_fail outcome. R1 fires only on system-tier
// sections; workspace-tier sections are recorded as info (severity
// raised in a separate follow-up — see story body "workspace-tier
// strict R1").
func evaluateR1(sections []ContextSection) (violations []R1Violation, audit string) {
	audit = "r1_pass"
	for _, s := range sections {
		if s.OriginScope != OriginScopeSystem {
			continue
		}
		if len(s.EmbeddedRefs) == 0 {
			continue
		}
		violations = append(violations, R1Violation{
			Section: s.Name,
			Scope:   s.OriginScope,
			Refs:    append([]string(nil), s.EmbeddedRefs...),
		})
		audit = "r1_fail"
	}
	return violations, audit
}

// findEmbeddedRefs returns the sorted, deduplicated set of substrate
// id literals matched by embeddedRefPattern in body.
func findEmbeddedRefs(body string) []string {
	matches := embeddedRefPattern.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// sectionFromBody builds a ContextSection from a single string body.
// Hash is the sha256 hex first-16; Bytes is the UTF-8 length.
func sectionFromBody(name, body, scope string) ContextSection {
	h := sha256.Sum256([]byte(body))
	return ContextSection{
		Name:         name,
		Bytes:        len(body),
		Hash:         hex.EncodeToString(h[:])[:16],
		OriginScope:  scope,
		EmbeddedRefs: findEmbeddedRefs(body),
	}
}

// argsHashFromMap canonicalises the args into a sha256 hex first-16.
// JSON encoding sorts map keys deterministically.
func argsHashFromMap(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	buf, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(buf)
	return hex.EncodeToString(h[:])[:16]
}

// emitContextFetch is the non-blocking entry point. Builds the ledger
// entry, runs R1, queues the row for the worker, and returns. On a
// full queue: drops the row and logs at warn (once per logger).
func (c *Client) emitContextFetch(ctx context.Context, in ContextFetchInput) {
	if c.deps.Ledger == nil || in.ProjectID == "" {
		return
	}
	c.ensureContextAuditWorker()

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	totalBytes := 0
	for _, s := range in.Sections {
		totalBytes += s.Bytes
	}

	violations, audit := evaluateR1(in.Sections)

	payload := map[string]any{
		"verb":        in.Verb,
		"args_hash":   in.ArgsHash,
		"total_bytes": totalBytes,
		"sections":    in.Sections,
		"rules": map[string]any{
			"r1": map[string]any{"violations": violations},
		},
	}
	structured, _ := json.Marshal(payload)

	tags := []string{AuditTagKindContextFetch, "verb:" + in.Verb, "project_id:" + in.ProjectID}
	if in.StoryID != "" {
		tags = append(tags, "story_id:"+in.StoryID)
	}
	tags = append(tags, "audit:"+audit)

	content := renderContextFetchContent(in.Verb, totalBytes, in.Sections, len(violations))

	exp := now.Add(720 * time.Hour)
	entry := ledger.LedgerEntry{
		WorkspaceID: in.WorkspaceID,
		ProjectID:   in.ProjectID,
		StoryID:     ledger.StringPtr(in.StoryID),
		Type:        ledger.TypeEvidence,
		Tags:        tags,
		Content:     content,
		Structured:  structured,
		Durability:  ledger.DurabilityEphemeral,
		ExpiresAt:   &exp,
		SourceType:  ledger.SourceSystem,
		CreatedBy:   in.CallerID,
	}

	// inflight.Add must happen before the send so a Flush that runs
	// between Add and a successful send still observes the in-flight
	// row. On a drop (queue full) the Add is reversed so the WaitGroup
	// counter stays accurate.
	c.audit.inflight.Add(1)
	select {
	case c.audit.queue <- contextAuditJob{entry: entry, now: now}:
	default:
		c.audit.inflight.Done()
		if c.deps.Logger != nil {
			c.deps.Logger.Warn().
				Str("verb", in.Verb).
				Str("project_id", in.ProjectID).
				Msg("context-fetch ledger drop (queue full)")
		}
	}
}

// renderContextFetchContent renders the short Markdown body shown in
// the ledger panel. Lists verb, total_bytes, top 3 sections by byte
// size, and violation count.
func renderContextFetchContent(verb string, totalBytes int, sections []ContextSection, violationCount int) string {
	sorted := make([]ContextSection, len(sections))
	copy(sorted, sections)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Bytes > sorted[j].Bytes
	})
	top := sorted
	if len(top) > 3 {
		top = top[:3]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "context-fetch: verb=%s total_bytes=%d violations=%d\n", verb, totalBytes, violationCount)
	for _, s := range top {
		fmt.Fprintf(&b, "- %s: %d bytes (%s)\n", s.Name, s.Bytes, s.OriginScope)
	}
	return b.String()
}

// callerFromCtx pulls the caller user id from the typical context
// stamping the wire layer does. Best-effort — the audit row stamps
// an empty string when none is set.
func callerFromCtx(_ context.Context, caller Caller) string {
	return caller.UserID
}

// fetchAuditDispatchInput bundles the per-verb dispatch metadata each
// typed method threads into the audit hook. ProjectID + WorkspaceID +
// StoryID are the row's scope; ArgsMap is hashed to args_hash; the
// per-verb introspector picks the response shape apart.
type fetchAuditDispatchInput struct {
	Verb        string
	ProjectID   string
	WorkspaceID string
	StoryID     string
	ArgsMap     map[string]any
	Caller      Caller
}

// dispatchContextAudit assembles a ContextFetchInput from the dispatch
// metadata + the introspector-built sections and hands it to
// emitContextFetch. Returns silently when the row would carry no
// project scope (mirrors AuditWrite's no-project skip).
func (c *Client) dispatchContextAudit(ctx context.Context, in fetchAuditDispatchInput, sections []ContextSection) {
	if in.Verb == "" {
		in.Verb = OriginVerbFromContext(ctx)
	}
	if in.Verb == "" || in.ProjectID == "" {
		return
	}
	c.emitContextFetch(ctx, ContextFetchInput{
		Verb:        in.Verb,
		ProjectID:   in.ProjectID,
		WorkspaceID: in.WorkspaceID,
		StoryID:     in.StoryID,
		ArgsHash:    argsHashFromMap(in.ArgsMap),
		Sections:    sections,
		CallerID:    in.Caller.UserID,
	})
}

// ---------------------------------------------------------------------
// Per-verb section introspectors. Each pulls the rendered text from
// the typed response body, classifies origin scope, and runs the
// embedded-ref scan.
// ---------------------------------------------------------------------

// sectionsForStoryGetOutput introspects StoryGetOutput. story body +
// AcceptanceCriteria are unscoped (per-story authoring); IntentBody
// is project-tier; AgentProcess is system-tier; Template body is
// system-tier; principles split by their stored scope.
func sectionsForStoryGetOutput(out StoryGetOutput) []ContextSection {
	sections := make([]ContextSection, 0, 8)
	if out.Story.Description != "" {
		sections = append(sections, sectionFromBody("story.description", out.Story.Description, OriginScopeUnscoped))
	}
	if out.Story.AcceptanceCriteria != "" {
		sections = append(sections, sectionFromBody("story.acceptance_criteria", out.Story.AcceptanceCriteria, OriginScopeUnscoped))
	}
	if out.IntentBody != "" {
		sections = append(sections, sectionFromBody("intent_body", out.IntentBody, OriginScopeProject))
	}
	if out.AgentProcess != "" {
		sections = append(sections, sectionFromBody("agent_process", out.AgentProcess, OriginScopeSystem))
	}
	if out.Template != nil {
		if buf, err := json.Marshal(out.Template); err == nil && len(buf) > 0 {
			sections = append(sections, sectionFromBody("template", string(buf), OriginScopeSystem))
		}
	}
	for i, p := range out.Principles {
		if p.Body == "" {
			continue
		}
		scope := scopeFromString(p.Scope)
		name := fmt.Sprintf("principles[%d]", i)
		sections = append(sections, sectionFromBody(name, p.Body, scope))
	}
	return sections
}

// sectionsForDocumentGet introspects a document.Document. The doc's
// own scope drives origin_scope; body is the only rendered surface.
func sectionsForDocumentGet(doc document.Document) []ContextSection {
	if doc.Body == "" {
		return nil
	}
	return []ContextSection{
		sectionFromBody("document.body", doc.Body, scopeFromString(doc.Scope)),
	}
}

// sectionsForPrincipleGet is the same as sectionsForDocumentGet — the
// type-pin doesn't change the surface, but the verb name on the row
// distinguishes the call site for downstream filters.
func sectionsForPrincipleGet(doc document.Document) []ContextSection {
	if doc.Body == "" {
		return nil
	}
	return []ContextSection{
		sectionFromBody("principle.body", doc.Body, scopeFromString(doc.Scope)),
	}
}

// sectionsForTaskWalkOutput introspects TaskWalkOutput. Story header +
// per-task descriptions are unscoped (per-story authoring).
func sectionsForTaskWalkOutput(out TaskWalkOutput) []ContextSection {
	sections := make([]ContextSection, 0, 4)
	if out.Story.Title != "" {
		sections = append(sections, sectionFromBody("story.title", out.Story.Title, OriginScopeUnscoped))
	}
	var taskBodies strings.Builder
	for _, t := range out.Tasks {
		if t.Description != "" {
			taskBodies.WriteString(t.Description)
			taskBodies.WriteString("\n")
		}
	}
	if taskBodies.Len() > 0 {
		sections = append(sections, sectionFromBody("tasks.descriptions", taskBodies.String(), OriginScopeUnscoped))
	}
	return sections
}

// sectionsForLedgerListOutput introspects []ledger.LedgerEntry rows.
// Each row contributes one section keyed by its id; origin scope is
// unscoped (ledger rows carry their own provenance, not orientation).
func sectionsForLedgerListOutput(rows []ledger.LedgerEntry) []ContextSection {
	if len(rows) == 0 {
		return nil
	}
	var combined strings.Builder
	for _, r := range rows {
		combined.WriteString(r.Content)
		combined.WriteString("\n")
	}
	return []ContextSection{
		sectionFromBody("ledger.rows", combined.String(), OriginScopeUnscoped),
	}
}

// sectionsForProjectSetOutput introspects ProjectSetOutput.Orientation
// (intent + principles), classifying by the principle's scope.
func sectionsForProjectSetOutput(out ProjectSetOutput) []ContextSection {
	sections := make([]ContextSection, 0, 4)
	if out.Orientation.IntentBody != "" {
		sections = append(sections, sectionFromBody("intent_body", out.Orientation.IntentBody, OriginScopeProject))
	}
	for i, p := range out.Orientation.Principles {
		if p.Body == "" {
			continue
		}
		name := fmt.Sprintf("principles[%d]", i)
		sections = append(sections, sectionFromBody(name, p.Body, scopeFromString(p.Scope)))
	}
	return sections
}

// sectionsForDocumentList introspects a []document.Document list. Used
// by principle_list (the wrapper that lists principle-typed documents).
func sectionsForDocumentList(docs []document.Document) []ContextSection {
	sections := make([]ContextSection, 0, len(docs))
	for i, d := range docs {
		if d.Body == "" {
			continue
		}
		name := fmt.Sprintf("documents[%d]", i)
		sections = append(sections, sectionFromBody(name, d.Body, scopeFromString(d.Scope)))
	}
	return sections
}

// scopeFromString normalises a document scope string into one of the
// OriginScope* constants. Unknown scopes fall through to unscoped.
func scopeFromString(s string) string {
	switch s {
	case document.ScopeSystem:
		return OriginScopeSystem
	case document.ScopeWorkspace:
		return OriginScopeWorkspace
	case document.ScopeProject:
		return OriginScopeProject
	}
	return OriginScopeUnscoped
}
