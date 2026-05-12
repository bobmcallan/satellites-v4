// Package client — agent verbs (agent_compose + agent_ephemeral_summary
// + the ephemeral-agent sweeper). Slice 6 of sty_f3f7bf9b: business
// logic lifted out of mcpserver/agent_compose.go into the typed client
// surface; the mcpserver handlers shrink to ≤25-line adapters.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// DefaultEphemeralAgentRetentionHours is the cap before the sweeper
// archives an ephemeral agent whose story is in a terminal state.
// Override via SATELLITES_EPHEMERAL_AGENT_RETENTION_HOURS. Exported so
// the wire layer can reference the same constant in tests.
const DefaultEphemeralAgentRetentionHours = 24

// ephemeralAgentRetention reads the env-overridable retention window;
// returns the default when unset or unparseable.
func ephemeralAgentRetention() time.Duration {
	v := os.Getenv("SATELLITES_EPHEMERAL_AGENT_RETENTION_HOURS")
	if v == "" {
		return DefaultEphemeralAgentRetentionHours * time.Hour
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return DefaultEphemeralAgentRetentionHours * time.Hour
	}
	return time.Duration(n) * time.Hour
}

// hookPatternPrefixes lists the recognised PreToolUse enforce hook
// pattern prefixes. Used by AgentCompose to reject obviously malformed
// entries before they reach the agent document. Kept small and
// deliberate — adding a prefix here is an opt-in step to surface agent
// permissions in the audit chain.
var hookPatternPrefixes = []string{
	"Read:", "Edit:", "Write:", "MultiEdit:", "NotebookEdit:",
	"Glob", "Grep", "TodoWrite", "Task", "ToolSearch",
	"AskUserQuestion", "BashOutput", "KillShell",
	"Bash:", "mcp__",
}

// isRecognisedPattern returns true when p starts with one of the known
// hook prefixes. Kept lenient: bare names like "Glob" or "Grep" (no
// colon) are valid; everything else must be `<Family>:<args>`.
func isRecognisedPattern(p string) bool {
	if p == "" {
		return false
	}
	for _, pref := range hookPatternPrefixes {
		if strings.HasPrefix(p, pref) {
			return true
		}
	}
	return false
}

// ErrAgentStoreNotConfigured is returned when an agent verb is invoked
// against a Client whose Deps.Documents is nil.
var ErrAgentStoreNotConfigured = errors.New("agent store not configured")

// ErrAgentLedgerNotConfigured is returned when AgentCompose is invoked
// against a Client whose Deps.Ledger is nil.
var ErrAgentLedgerNotConfigured = errors.New("agent ledger not configured")

// AgentComposeError carries a JSON-envelope wire shape for the
// validation rejections agent_compose previously emitted. The wire
// layer stringifies err.Body() into NewToolResultError verbatim,
// preserving the byte-identical envelope.
type AgentComposeError struct {
	Code    string         // machine-readable error code
	Payload map[string]any // additional fields the envelope carries
}

// Error returns the machine-readable code. Use Body() for the wire
// envelope.
func (e *AgentComposeError) Error() string { return e.Code }

// Body returns the JSON envelope the wire layer emits as the
// NewToolResultError text. Falls back to {"error": code} on marshal
// failure (cannot happen for the payload shapes used here).
func (e *AgentComposeError) Body() string {
	m := map[string]any{"error": e.Code}
	for k, v := range e.Payload {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// PrincipleSummary is the per-principle wire shape on the AgentCompose
// response's principles_context field. story_c0489be2 (S7): every
// agent_compose response carries the resolved active principles so
// downstream invokers can layer them onto the agent's system message
// without a separate principle_list round-trip.
type PrincipleSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// AgentComposeInput captures the agent_compose request shape. The wire
// layer pre-resolves Memberships and threads the request fields in.
type AgentComposeInput struct {
	Name               string
	ProjectID          string
	SkillRefs          []string
	PermissionPatterns []string
	Ephemeral          bool
	StoryID            string
	Reason             string
	Memberships        []string
	Now                time.Time
}

// AgentComposeOutput mirrors the JSON shape the wire handler
// previously emitted: agent document + ledger row id +
// principles_context.
type AgentComposeOutput struct {
	Agent                document.Document  `json:"agent"`
	AgentComposeLedgerID string             `json:"agent_compose_ledger_id"`
	PrinciplesContext    []PrincipleSummary `json:"principles_context"`
	// SkillRefCount + PermPatternCount are surfaced for the wire-layer
	// audit log; they're not on the JSON wire shape.
	SkillRefCount    int `json:"-"`
	PermPatternCount int `json:"-"`
}

// AgentCompose mints a type=agent document carrying the supplied skill
// refs + permission patterns and writes a kind:agent-compose ledger
// row. When Ephemeral is set, the agent is scoped to StoryID and the
// sweeper will archive it on story completion.
func (c *Client) AgentCompose(ctx context.Context, caller Caller, in AgentComposeInput) (AgentComposeOutput, error) {
	if c.deps.Documents == nil {
		return AgentComposeOutput{}, ErrAgentStoreNotConfigured
	}
	if c.deps.Ledger == nil {
		return AgentComposeOutput{}, ErrAgentLedgerNotConfigured
	}
	if in.Name == "" {
		return AgentComposeOutput{}, errors.New("name required")
	}
	if in.Ephemeral && in.StoryID == "" {
		return AgentComposeOutput{}, errors.New("story_id is required when ephemeral=true")
	}
	for i, p := range in.PermissionPatterns {
		if !isRecognisedPattern(p) {
			return AgentComposeOutput{}, &AgentComposeError{
				Code:    "unknown_permission_pattern",
				Payload: map[string]any{"index": i, "pattern": p},
			}
		}
	}

	// Validate every skill_ref resolves to an active type=skill document.
	// Use nil memberships so system-scope skills (which carry no
	// WorkspaceID) resolve alongside workspace-scoped ones; the agent's
	// scope check below still gates the resulting agent document.
	for i, sid := range in.SkillRefs {
		d, err := c.deps.Documents.GetByID(ctx, sid, nil)
		if err != nil {
			return AgentComposeOutput{}, &AgentComposeError{
				Code:    "unknown_skill_ref",
				Payload: map[string]any{"index": i, "skill_ref": sid},
			}
		}
		if d.Type != document.TypeSkill || d.Status != document.StatusActive {
			return AgentComposeOutput{}, &AgentComposeError{
				Code: "skill_ref_not_active_skill",
				Payload: map[string]any{
					"index":      i,
					"skill_ref":  sid,
					"got_type":   d.Type,
					"got_status": d.Status,
				},
			}
		}
	}

	projectID := in.ProjectID

	// Resolve workspace + project. Compose accepts an explicit project_id
	// for project-scoped agents; ephemeral agents inherit project from
	// the owning story when omitted.
	var workspaceID string
	if in.StoryID != "" && c.deps.Stories != nil {
		st, err := c.deps.Stories.GetByID(ctx, in.StoryID, in.Memberships)
		if err != nil {
			return AgentComposeOutput{}, errors.New("story_id not found")
		}
		workspaceID = st.WorkspaceID
		if projectID == "" {
			projectID = st.ProjectID
		}
	}
	if workspaceID == "" && projectID != "" {
		workspaceID = c.ResolveProjectWorkspaceID(ctx, projectID)
	}

	settings := document.AgentSettings{
		PermissionPatterns: in.PermissionPatterns,
		SkillRefs:          in.SkillRefs,
		Ephemeral:          in.Ephemeral,
	}
	if in.StoryID != "" {
		storyRef := in.StoryID
		settings.StoryID = &storyRef
	}
	structured, err := document.MarshalAgentSettings(settings)
	if err != nil {
		return AgentComposeOutput{}, fmt.Errorf("marshal agent settings: %v", err)
	}

	scope := document.ScopeProject
	var pidPtr *string
	if projectID != "" {
		pidPtr = &projectID
	} else {
		scope = document.ScopeSystem
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	created, err := c.deps.Documents.Create(ctx, document.Document{
		WorkspaceID: workspaceID,
		Type:        document.TypeAgent,
		Scope:       scope,
		Name:        in.Name,
		Body:        in.Reason,
		ProjectID:   pidPtr,
		Status:      document.StatusActive,
		Structured:  structured,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return AgentComposeOutput{}, &AgentComposeError{
			Code:    "agent_create_failed",
			Payload: map[string]any{"message": err.Error()},
		}
	}

	// Write the kind:agent-compose ledger row. Tags include the story
	// scope (when ephemeral) so reviewers and the sweeper can discover
	// the row by story.
	tags := []string{"kind:agent-compose"}
	if in.StoryID != "" {
		tags = append(tags, "story:"+in.StoryID)
	}
	auditPayload, _ := json.Marshal(map[string]any{
		"agent_id":            created.ID,
		"name":                in.Name,
		"skill_refs":          in.SkillRefs,
		"permission_patterns": in.PermissionPatterns,
		"story_id":            in.StoryID,
		"ephemeral":           in.Ephemeral,
		"reason":              in.Reason,
	})
	var storyPtr *string
	if in.StoryID != "" {
		storyPtr = ledger.StringPtr(in.StoryID)
	}
	row, err := c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     storyPtr,
		Type:        ledger.TypeAgentCompose,
		Tags:        tags,
		Content:     in.Reason,
		Structured:  auditPayload,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return AgentComposeOutput{}, err
	}

	return AgentComposeOutput{
		Agent:                created,
		AgentComposeLedgerID: row.ID,
		PrinciplesContext:    c.loadActivePrinciples(ctx, projectID, in.Memberships),
		SkillRefCount:        len(in.SkillRefs),
		PermPatternCount:     len(in.PermissionPatterns),
	}, nil
}

// AgentEphemeralSummaryInput captures the agent_ephemeral_summary
// request shape.
type AgentEphemeralSummaryInput struct {
	ProjectID   string
	Memberships []string
}

// AgentEphemeralSummaryGroup groups ephemeral agents by their sorted
// skill_refs slice. Promotion candidates surface when Count ≥
// promote_to_canonical_threshold (3).
type AgentEphemeralSummaryGroup struct {
	SkillSet []string `json:"skill_set"`
	Count    int      `json:"count"`
}

// AgentEphemeralSummaryOutput mirrors the JSON shape the wire handler
// previously emitted.
type AgentEphemeralSummaryOutput struct {
	ProjectID                   string                       `json:"project_id"`
	EphemeralAgentCount         int                          `json:"ephemeral_agent_count"`
	BySkillSet                  []AgentEphemeralSummaryGroup `json:"by_skill_set"`
	PromoteToCanonicalThreshold int                          `json:"promote_to_canonical_threshold"`
}

// AgentEphemeralSummary returns the per-project count of active
// ephemeral agents — the substrate hint that satellites_project_status
// surfaces (story_b19260d8 AC #7). Groups agents by sorted skill_refs
// so callers can spot promotion candidates. ProjectID may be empty for
// an all-projects summary.
func (c *Client) AgentEphemeralSummary(ctx context.Context, caller Caller, in AgentEphemeralSummaryInput) (AgentEphemeralSummaryOutput, error) {
	if c.deps.Documents == nil {
		return AgentEphemeralSummaryOutput{}, ErrAgentStoreNotConfigured
	}
	rows, err := c.deps.Documents.List(ctx, document.ListOptions{Type: document.TypeAgent, Limit: 1000}, in.Memberships)
	if err != nil {
		return AgentEphemeralSummaryOutput{}, fmt.Errorf("list agents: %v", err)
	}
	groups := make(map[string]*AgentEphemeralSummaryGroup)
	total := 0
	for _, d := range rows {
		if d.Status != document.StatusActive {
			continue
		}
		if in.ProjectID != "" {
			if d.ProjectID == nil || *d.ProjectID != in.ProjectID {
				continue
			}
		}
		settings, err := document.UnmarshalAgentSettings(d.Structured)
		if err != nil || !settings.Ephemeral {
			continue
		}
		total++
		key := strings.Join(settings.SkillRefs, ",")
		if g, ok := groups[key]; ok {
			g.Count++
		} else {
			cp := append([]string(nil), settings.SkillRefs...)
			groups[key] = &AgentEphemeralSummaryGroup{SkillSet: cp, Count: 1}
		}
	}
	bySkillSet := make([]AgentEphemeralSummaryGroup, 0, len(groups))
	for _, g := range groups {
		bySkillSet = append(bySkillSet, *g)
	}
	return AgentEphemeralSummaryOutput{
		ProjectID:                   in.ProjectID,
		EphemeralAgentCount:         total,
		BySkillSet:                  bySkillSet,
		PromoteToCanonicalThreshold: 3,
	}, nil
}

// AgentArchiveEphemeralInput captures the input for the per-story
// ephemeral-agent sweeper. The wire layer calls this from the
// storystatus reconciler; tests call it directly.
type AgentArchiveEphemeralInput struct {
	StoryID     string
	TerminalAt  time.Time
	Memberships []string
	Now         time.Time
}

// AgentArchiveEphemeralForStory archives ephemeral type=agent documents
// whose StoryID equals in.StoryID and whose owning story has been in a
// terminal state for at least the configured retention window. Returns
// the number of agents archived. Idempotent — agents already archived
// are skipped.
func (c *Client) AgentArchiveEphemeralForStory(ctx context.Context, caller Caller, in AgentArchiveEphemeralInput) (int, error) {
	if c.deps.Documents == nil || in.StoryID == "" {
		return 0, nil
	}
	if time.Since(in.TerminalAt) < ephemeralAgentRetention() {
		return 0, nil
	}
	rows, err := c.deps.Documents.List(ctx, document.ListOptions{Type: document.TypeAgent}, in.Memberships)
	if err != nil {
		return 0, fmt.Errorf("list agents: %w", err)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	archived := 0
	for _, d := range rows {
		if d.Status != document.StatusActive {
			continue
		}
		settings, err := document.UnmarshalAgentSettings(d.Structured)
		if err != nil {
			continue
		}
		if !settings.Ephemeral || settings.StoryID == nil || *settings.StoryID != in.StoryID {
			continue
		}
		if err := c.deps.Documents.Delete(ctx, d.ID, document.DeleteArchive, in.Memberships); err != nil {
			if errors.Is(err, document.ErrNotFound) {
				continue
			}
			return archived, fmt.Errorf("archive agent %s: %w", d.ID, err)
		}
		archivePayload, _ := json.Marshal(map[string]any{
			"agent_id": d.ID,
			"story_id": in.StoryID,
		})
		archiveProject := ""
		if d.ProjectID != nil {
			archiveProject = *d.ProjectID
		}
		if c.deps.Ledger != nil {
			_, _ = c.deps.Ledger.Append(ctx, ledger.LedgerEntry{
				WorkspaceID: d.WorkspaceID,
				ProjectID:   archiveProject,
				StoryID:     ledger.StringPtr(in.StoryID),
				Type:        ledger.TypeAgentArchive,
				Tags:        []string{"kind:agent-archive", "story:" + in.StoryID},
				Content:     "ephemeral agent archived",
				Structured:  archivePayload,
				CreatedBy:   caller.UserID,
			}, now)
		}
		archived++
	}
	return archived, nil
}

// loadActivePrinciples resolves the active principle set for a project
// (system principles always; project-scope principles when projectID
// is non-empty). Mirrors the semantics of
// principle_list(active_only=true, project_id=...). story_c0489be2
// (S7).
func (c *Client) loadActivePrinciples(ctx context.Context, projectID string, memberships []string) []PrincipleSummary {
	if c.deps.Documents == nil {
		return nil
	}
	out := make([]PrincipleSummary, 0, 16)
	sysRows, err := c.deps.Documents.List(ctx, document.ListOptions{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Limit: 200,
	}, nil)
	if err == nil {
		for _, r := range sysRows {
			if r.Status != document.StatusActive {
				continue
			}
			out = append(out, PrincipleSummary{ID: r.ID, Name: r.Name, Description: r.Body})
		}
	}
	if projectID != "" {
		projRows, err := c.deps.Documents.List(ctx, document.ListOptions{
			Type:      document.TypePrinciple,
			Scope:     document.ScopeProject,
			ProjectID: projectID,
			Limit:     200,
		}, memberships)
		if err == nil {
			for _, r := range projRows {
				if r.Status != document.StatusActive {
					continue
				}
				out = append(out, PrincipleSummary{ID: r.ID, Name: r.Name, Description: r.Body})
			}
		}
	}
	return out
}
