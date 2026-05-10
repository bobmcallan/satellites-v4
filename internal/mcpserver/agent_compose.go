// Package mcpserver — agent_compose handler + ephemeral lifecycle sweeper
// (story_b19260d8). Lets the orchestrator author an agent document
// for a story (or as canonical) with explicit skill refs + permission
// patterns; the sweeper archives ephemeral agents whose owning story is
// terminal.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bobmcallan/satellites/internal/auth"
	"os"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// defaultEphemeralAgentRetentionHours is the cap before the sweeper
// archives an ephemeral agent whose story is in a terminal state.
// Override via SATELLITES_EPHEMERAL_AGENT_RETENTION_HOURS.
const defaultEphemeralAgentRetentionHours = 24

// ephemeralAgentRetention reads the env-overridable retention window;
// returns the default when unset or unparseable.
func ephemeralAgentRetention() time.Duration {
	v := os.Getenv("SATELLITES_EPHEMERAL_AGENT_RETENTION_HOURS")
	if v == "" {
		return defaultEphemeralAgentRetentionHours * time.Hour
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultEphemeralAgentRetentionHours * time.Hour
	}
	return time.Duration(n) * time.Hour
}

// hookPatternPrefixes lists the recognised PreToolUse enforce hook
// pattern prefixes. Used by handleAgentCompose to reject obviously
// malformed entries before they reach the agent document. Kept small
// and deliberate — adding a prefix here is an opt-in step to surface
// agent permissions in the audit chain.
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

// handleAgentCompose creates a type=agent document carrying the
// supplied skill refs + permission patterns, and writes a
// kind:agent-compose ledger row. When ephemeral is set, the agent is
// scoped to story_id and the sweeper will archive it on story
// completion.
func (s *Server) handleAgentCompose(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	name, err := req.RequireString("name")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	projectID := req.GetString("project_id", "")
	skillRefs := req.GetStringSlice("skill_refs", nil)
	permissionPatterns := req.GetStringSlice("permission_patterns", nil)
	ephemeral := req.GetBool("ephemeral", false)
	storyID := req.GetString("story_id", "")
	reason := req.GetString("reason", "")

	if ephemeral && storyID == "" {
		return mcpgo.NewToolResultError("story_id is required when ephemeral=true"), nil
	}
	for i, p := range permissionPatterns {
		if !isRecognisedPattern(p) {
			errBody, _ := json.Marshal(map[string]any{
				"error":   "unknown_permission_pattern",
				"index":   i,
				"pattern": p,
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
	}

	memberships := s.resolveCallerMemberships(ctx, caller)

	// Validate every skill_ref resolves to an active type=skill document.
	// Use nil memberships so system-scope skills (which carry no
	// WorkspaceID) resolve alongside workspace-scoped ones; the agent's
	// scope check below still gates the resulting agent document.
	for i, sid := range skillRefs {
		d, err := s.docs.GetByID(ctx, sid, nil)
		if err != nil {
			errBody, _ := json.Marshal(map[string]any{
				"error":     "unknown_skill_ref",
				"index":     i,
				"skill_ref": sid,
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
		if d.Type != document.TypeSkill || d.Status != document.StatusActive {
			errBody, _ := json.Marshal(map[string]any{
				"error":      "skill_ref_not_active_skill",
				"index":      i,
				"skill_ref":  sid,
				"got_type":   d.Type,
				"got_status": d.Status,
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
	}

	// Resolve workspace + project. Compose accepts an explicit project_id
	// for project-scoped agents; ephemeral agents inherit project from
	// the owning story when omitted.
	var workspaceID string
	if storyID != "" && s.stories != nil {
		st, err := s.stories.GetByID(ctx, storyID, memberships)
		if err != nil {
			return mcpgo.NewToolResultError("story_id not found"), nil
		}
		workspaceID = st.WorkspaceID
		if projectID == "" {
			projectID = st.ProjectID
		}
	}
	if workspaceID == "" && projectID != "" {
		workspaceID = s.resolveProjectWorkspaceID(ctx, projectID)
	}

	settings := document.AgentSettings{
		PermissionPatterns: permissionPatterns,
		SkillRefs:          skillRefs,
		Ephemeral:          ephemeral,
	}
	if storyID != "" {
		settings.StoryID = &storyID
	}
	structured, err := document.MarshalAgentSettings(settings)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal agent settings: %v", err)), nil
	}

	scope := document.ScopeProject
	var pidPtr *string
	if projectID != "" {
		pidPtr = &projectID
	} else {
		scope = document.ScopeSystem
	}

	now := time.Now().UTC()
	created, err := s.docs.Create(ctx, document.Document{
		WorkspaceID: workspaceID,
		Type:        document.TypeAgent,
		Scope:       scope,
		Name:        name,
		Body:        reason,
		ProjectID:   pidPtr,
		Status:      document.StatusActive,
		Structured:  structured,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		errBody, _ := json.Marshal(map[string]any{
			"error":   "agent_create_failed",
			"message": err.Error(),
		})
		return mcpgo.NewToolResultError(string(errBody)), nil
	}

	// Write the kind:agent-compose ledger row. Tags include the story
	// scope (when ephemeral) so reviewers and the sweeper can discover
	// the row by story.
	tags := []string{"kind:agent-compose"}
	if storyID != "" {
		tags = append(tags, "story:"+storyID)
	}
	auditPayload, _ := json.Marshal(map[string]any{
		"agent_id":            created.ID,
		"name":                name,
		"skill_refs":          skillRefs,
		"permission_patterns": permissionPatterns,
		"story_id":            storyID,
		"ephemeral":           ephemeral,
		"reason":              reason,
	})
	var storyRef *string
	if storyID != "" {
		storyRef = ledger.StringPtr(storyID)
	}
	row, err := s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: workspaceID,
		ProjectID:   projectID,
		StoryID:     storyRef,
		Type:        ledger.TypeAgentCompose,
		Tags:        tags,
		Content:     reason,
		Structured:  auditPayload,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	principlesContext := s.loadActivePrinciples(ctx, projectID, memberships)
	body, _ := json.Marshal(map[string]any{
		"agent":                   created,
		"agent_compose_ledger_id": row.ID,
		"principles_context":      principlesContext,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "agent_compose").
		Str("agent_id", created.ID).
		Str("story_id", storyID).
		Bool("ephemeral", ephemeral).
		Int("skill_count", len(skillRefs)).
		Int("perm_count", len(permissionPatterns)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleAgentEphemeralSummary returns the per-project count of active
// ephemeral agents — the substrate hint that satellites_project_status
// surfaces (story_b19260d8 AC #7). Optional skill_set tag in the
// response groups agents by their sorted skill_refs slice so callers
// can spot promotion candidates ("3 agents with skills X+Y → promote
// to canonical?"). projectID may be empty for an all-projects summary.
func (s *Server) handleAgentEphemeralSummary(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := auth.UserFrom(ctx)
	projectID := req.GetString("project_id", "")
	memberships := s.resolveCallerMemberships(ctx, caller)

	rows, err := s.docs.List(ctx, document.ListOptions{Type: document.TypeAgent, Limit: 1000}, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("list agents: %v", err)), nil
	}

	type group struct {
		SkillSet []string `json:"skill_set"`
		Count    int      `json:"count"`
	}
	groups := make(map[string]*group)
	total := 0
	for _, d := range rows {
		if d.Status != document.StatusActive {
			continue
		}
		if projectID != "" {
			if d.ProjectID == nil || *d.ProjectID != projectID {
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
			groups[key] = &group{SkillSet: cp, Count: 1}
		}
	}

	bySkillSet := make([]group, 0, len(groups))
	for _, g := range groups {
		bySkillSet = append(bySkillSet, *g)
	}
	body, _ := json.Marshal(map[string]any{
		"project_id":                     projectID,
		"ephemeral_agent_count":          total,
		"by_skill_set":                   bySkillSet,
		"promote_to_canonical_threshold": 3,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "agent_ephemeral_summary").
		Str("project_id", projectID).
		Int("total", total).
		Int("groups", len(groups)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// archiveEphemeralAgentsForStory archives ephemeral type=agent documents
// whose StoryID equals storyID and whose owning story has been in a
// terminal state for at least the configured retention window. Returns
// the number of agents archived. Idempotent — agents already in
// status=archived are skipped.
func (s *Server) archiveEphemeralAgentsForStory(ctx context.Context, storyID string, terminalAt time.Time, memberships []string) (int, error) {
	if s.docs == nil || storyID == "" {
		return 0, nil
	}
	if time.Since(terminalAt) < ephemeralAgentRetention() {
		return 0, nil
	}
	rows, err := s.docs.List(ctx, document.ListOptions{Type: document.TypeAgent}, memberships)
	if err != nil {
		return 0, fmt.Errorf("list agents: %w", err)
	}
	caller, _ := auth.UserFrom(ctx)
	now := time.Now().UTC()
	archived := 0
	for _, d := range rows {
		if d.Status != document.StatusActive {
			continue
		}
		settings, err := document.UnmarshalAgentSettings(d.Structured)
		if err != nil {
			continue
		}
		if !settings.Ephemeral || settings.StoryID == nil || *settings.StoryID != storyID {
			continue
		}
		if err := s.docs.Delete(ctx, d.ID, document.DeleteArchive, memberships); err != nil {
			if errors.Is(err, document.ErrNotFound) {
				continue
			}
			return archived, fmt.Errorf("archive agent %s: %w", d.ID, err)
		}
		archivePayload, _ := json.Marshal(map[string]any{
			"agent_id": d.ID,
			"story_id": storyID,
		})
		archiveProject := ""
		if d.ProjectID != nil {
			archiveProject = *d.ProjectID
		}
		_, _ = s.ledger.Append(ctx, ledger.LedgerEntry{
			WorkspaceID: d.WorkspaceID,
			ProjectID:   archiveProject,
			StoryID:     ledger.StringPtr(storyID),
			Type:        ledger.TypeAgentArchive,
			Tags:        []string{"kind:agent-archive", "story:" + storyID},
			Content:     "ephemeral agent archived",
			Structured:  archivePayload,
			CreatedBy:   caller.UserID,
		}, now)
		archived++
	}
	return archived, nil
}

// principleSummary is the per-principle wire shape on the
// agent_compose response's principles_context field. story_c0489be2
// (S7): every agent_compose response carries the resolved active
// principles so downstream invokers can layer them onto the agent's
// system message without a separate principle_list round-trip.
type principleSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// loadActivePrinciples resolves the active principle set for a project
// (system principles always; project-scope principles when projectID
// is non-empty). Mirrors the semantics of
// satellites_principle_list(active_only=true, project_id=...).
// story_c0489be2 (S7).
func (s *Server) loadActivePrinciples(ctx context.Context, projectID string, memberships []string) []principleSummary {
	if s.docs == nil {
		return nil
	}
	out := make([]principleSummary, 0, 16)
	sysRows, err := s.docs.List(ctx, document.ListOptions{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Limit: 200,
	}, nil)
	if err == nil {
		for _, r := range sysRows {
			if r.Status != document.StatusActive {
				continue
			}
			out = append(out, principleSummary{ID: r.ID, Name: r.Name, Description: r.Body})
		}
	}
	if projectID != "" {
		projRows, err := s.docs.List(ctx, document.ListOptions{
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
				out = append(out, principleSummary{ID: r.ID, Name: r.Name, Description: r.Body})
			}
		}
	}
	return out
}
