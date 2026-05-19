// Package client — render_prompt: orchestrator-side inline prompt
// builder. RenderTaskPrompt assembles a self-contained markdown
// document from a task's bound story + contract + agent + cited
// skills + cited principles + prior chain, so a dispatched executor
// running under the restricted role envelope (sty_056b68f6) has every
// piece of context it needs without invoking any read verb (those
// verbs all 403 under the task-scoped apikey).
//
// Cite pr_substrate_model: per-verb retrieval is honoured at the
// orchestrator. The renderer batches reads here so the executor can
// run under its tightened envelope.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/task"
)

// Per-section byte caps. Values match the story defaults and are
// surfaced as named constants (not magic numbers) per AC4. The CLI
// does NOT expose flags that alter these — AC6 enforces a one-template
// no-flag-toggle policy.
const (
	AgentCap     = 4096
	ContractCap  = 4096
	SkillCap     = 4096
	StoryCap     = 8192
	PrincipleCap = 2048
)

// truncationMarkerFmt is the literal marker appended on its own line
// when a section body is truncated. The doc id lets the orchestrator
// (which DOES carry document_get) refetch the full body on a retry.
// AC4 binds the exact phrase "[…cited body continues…]".
const truncationMarkerFmt = "\n[…cited body continues; see document_get(id=%s) …]"

// principleCiteRE matches the canonical `pr_<lower>` id form anywhere
// in cited prose (contract body, story body). The filter is on-purpose
// strict: only principles cited in prose appear in "Principles in
// force" — uncited active principles are silently dropped per AC2 §6.
var principleCiteRE = regexp.MustCompile(`pr_[a-z0-9_]+`)

// RenderTaskPromptInput names the task whose context should be
// composed into a markdown prompt. WorkBody is the orchestrator's
// task-specific instruction body; the renderer threads it under the
// "## Your work" section verbatim (no transform).
type RenderTaskPromptInput struct {
	TaskID   string
	Action   string // "contract:<name>"
	StoryID  string
	WorkBody string
	Now      time.Time
}

// RenderTaskPrompt composes a self-contained markdown prompt for the
// dispatched executor of TaskID. The returned string is the raw
// markdown the CLI subcommand writes to stdout; the orchestrator
// pipes it into `task_add(prompt=…)` for the next dispatch.
//
// Sections with no content (no skills, no chain, no work body) are
// omitted entirely — never an empty header. This is a fixed template;
// no flag toggles the section set or order (pr_no_unrequested_compat).
func (c *Client) RenderTaskPrompt(ctx context.Context, caller Caller, in RenderTaskPromptInput) (string, error) {
	if in.TaskID == "" {
		return "", errors.New("task_id required")
	}
	if in.Action == "" {
		return "", errors.New("action required")
	}
	if in.StoryID == "" {
		return "", errors.New("story_id required")
	}
	if !strings.HasPrefix(in.Action, "contract:") {
		return "", fmt.Errorf("action must be of the form contract:<name>, got %q", in.Action)
	}
	contractName := strings.TrimPrefix(in.Action, "contract:")
	if contractName == "" {
		return "", errors.New("action must name a contract")
	}

	memberships := c.ResolveCallerMemberships(ctx, caller)

	// 1) task
	t, err := c.TaskGet(ctx, caller, TaskGetInput{ID: in.TaskID, Memberships: memberships})
	if err != nil {
		return "", fmt.Errorf("task_get: %w", err)
	}

	// 2) story
	stOut, err := c.StoryGet(ctx, caller, StoryGetInput{ID: in.StoryID, Memberships: memberships})
	if err != nil {
		return "", fmt.Errorf("story_get: %w", err)
	}

	// 3) contract
	contractDoc, err := c.DocumentGet(ctx, caller, DocumentGetInput{
		Name:              contractName,
		Type:              document.TypeContract,
		WorkspaceID:       t.WorkspaceID,
		ResolvedProjectID: t.ProjectID,
		Memberships:       memberships,
	})
	if err != nil {
		return "", fmt.Errorf("contract_get(%s): %w", contractName, err)
	}

	// 4) agent
	var agentDoc document.Document
	if t.AgentID != "" {
		ag, gerr := c.DocumentGet(ctx, caller, DocumentGetInput{
			ID:                t.AgentID,
			WorkspaceID:       t.WorkspaceID,
			ResolvedProjectID: t.ProjectID,
			Memberships:       memberships,
		})
		if gerr != nil {
			return "", fmt.Errorf("agent_get(%s): %w", t.AgentID, gerr)
		}
		agentDoc = ag
	}

	// 5) skill union (agent.skill_refs ∪ contract.skills_required)
	skillIDs := skillUnion(agentDoc, contractDoc)
	skills := make([]document.Document, 0, len(skillIDs))
	for _, sid := range skillIDs {
		sd, gerr := c.DocumentGet(ctx, caller, DocumentGetInput{
			ID:                sid,
			WorkspaceID:       t.WorkspaceID,
			ResolvedProjectID: t.ProjectID,
			Memberships:       memberships,
		})
		if gerr != nil {
			// A dangling skill ref shouldn't break dispatch — the marker
			// lets the executor see the absence without invoking
			// document_get itself.
			skills = append(skills, document.Document{ID: sid, Name: "", Body: fmt.Sprintf("[…skill %s not found…]", sid)})
			continue
		}
		skills = append(skills, sd)
	}

	// 6) cited principles: only ids referenced in contract OR story prose.
	citedIDs := citedPrincipleIDs(contractDoc.Body, stOut.Story.Description)
	principles := c.fetchCitedPrinciples(ctx, t.ProjectID, memberships, citedIDs)

	// 7) prior chain
	var walk TaskWalkOutput
	if w, werr := c.TaskWalk(ctx, caller, TaskWalkInput{StoryID: in.StoryID, Memberships: memberships}); werr == nil {
		walk = w
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	return composeMarkdown(composeInput{
		Task:       t,
		Action:     in.Action,
		StoryID:    in.StoryID,
		Agent:      agentDoc,
		Contract:   contractDoc,
		Skills:     skills,
		Story:      stOut,
		Principles: principles,
		Walk:       walk,
		WorkBody:   in.WorkBody,
		Now:        now,
	}), nil
}

// composeInput bundles the resolved primitives composeMarkdown needs.
type composeInput struct {
	Task       task.Task
	Action     string
	StoryID    string
	Agent      document.Document
	Contract   document.Document
	Skills     []document.Document
	Story      StoryGetOutput
	Principles []document.Document
	Walk       TaskWalkOutput
	WorkBody   string
	Now        time.Time
}

// composeMarkdown assembles the rendered prompt. Sections with no
// content are omitted; the section order is fixed (AC2).
func composeMarkdown(in composeInput) string {
	var b strings.Builder

	// Header.
	fmt.Fprintf(&b, "# Task %s\n\n", in.Task.ID)
	agentLabel := in.Agent.Name
	if agentLabel == "" {
		agentLabel = in.Task.AgentID
	}
	fmt.Fprintf(&b, "> Action: %s · Story: %s · Agent: %s (%s)\n",
		in.Action, in.StoryID, agentLabel, in.Task.AgentID)
	fmt.Fprintf(&b, "> Project: %s · Generated: %s\n\n",
		in.Task.ProjectID, in.Now.UTC().Format(time.RFC3339))

	// Your role (agent body).
	if body := strings.TrimSpace(in.Agent.Body); body != "" {
		b.WriteString("## Your role\n")
		b.WriteString(truncateWithMarker(body, AgentCap, in.Agent.ID))
		b.WriteString("\n\n")
	}

	// Contract.
	if body := strings.TrimSpace(in.Contract.Body); body != "" {
		b.WriteString("## Contract\n")
		b.WriteString(truncateWithMarker(body, ContractCap, in.Contract.ID))
		b.WriteString("\n\n")
	}

	// Skills.
	if len(in.Skills) > 0 {
		b.WriteString("## Skills available\n")
		for _, s := range in.Skills {
			fmt.Fprintf(&b, "### skill: %s (%s)\n", s.Name, s.ID)
			b.WriteString(truncateWithMarker(s.Body, SkillCap, s.ID))
			b.WriteString("\n\n")
		}
	}

	// Story (title + first body line + truncated body to cap).
	storyTitle := strings.TrimSpace(in.Story.Story.Title)
	storyBody := strings.TrimSpace(in.Story.Story.Description)
	if storyTitle != "" || storyBody != "" {
		b.WriteString("## Story\n")
		if storyTitle != "" {
			fmt.Fprintf(&b, "**%s**\n\n", storyTitle)
		}
		if storyBody != "" {
			b.WriteString(truncateWithMarker(storyBody, StoryCap, in.Story.Story.ID))
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Principles.
	if len(in.Principles) > 0 {
		b.WriteString("## Principles in force\n")
		for _, p := range in.Principles {
			fmt.Fprintf(&b, "### principle: %s (%s)\n", p.Name, p.ID)
			b.WriteString(truncateWithMarker(p.Body, PrincipleCap, p.ID))
			b.WriteString("\n\n")
		}
	}

	// Prior chain.
	if len(in.Walk.Tasks) > 0 {
		b.WriteString("## Prior chain\n")
		b.WriteString("| task_id | kind | status | outcome | action |\n")
		b.WriteString("|---------|------|--------|---------|--------|\n")
		for _, row := range in.Walk.Tasks {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				row.ID, row.Kind, row.Status, row.Outcome, row.Action)
		}
		b.WriteString("\n")
	}

	// Your work (verbatim, no transform).
	if body := strings.TrimSpace(in.WorkBody); body != "" {
		b.WriteString("## Your work\n")
		b.WriteString(in.WorkBody)
		if !strings.HasSuffix(in.WorkBody, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	// Close.
	b.WriteString("## Close\n")
	fmt.Fprintf(&b, "- Evidence: `satellites-client ledger append --project-id %s --type evidence --content \"...\" --tags task_id:%s,kind:evidence`\n", in.Task.ProjectID, in.Task.ID)
	fmt.Fprintf(&b, "- Close success: `satellites-client task update --id %s --status closed --outcome success`\n", in.Task.ID)
	fmt.Fprintf(&b, "- Close failure: `satellites-client task update --id %s --status closed --outcome failure`\n", in.Task.ID)

	return b.String()
}

// truncateWithMarker returns body capped at cap bytes. When the cap
// is hit, it backs off to the previous UTF-8 codepoint boundary so
// the output never contains a partial codepoint, then appends the
// truncation marker on its own line. A body whose length equals cap
// is returned verbatim with no marker (AC4).
func truncateWithMarker(body string, cap int, docID string) string {
	if cap <= 0 || len(body) <= cap {
		return body
	}
	cut := cap
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + fmt.Sprintf(truncationMarkerFmt, docID)
}

// skillUnion returns the de-duplicated, stably-ordered union of an
// agent's skill_refs and a contract's skills_required slice. Order:
// agent.skill_refs first (in declared order), then any
// contract.skills_required not already present.
//
// contract.skills_required is read defensively from the structured
// JSON payload — the field is not yet a typed property on contract
// documents; absence returns an empty slice.
func skillUnion(agent, contract document.Document) []string {
	seen := make(map[string]struct{}, 8)
	out := make([]string, 0, 8)

	if len(agent.Structured) > 0 {
		if as, err := document.UnmarshalAgentSettings(agent.Structured); err == nil {
			for _, sid := range as.SkillRefs {
				if sid == "" {
					continue
				}
				if _, dup := seen[sid]; dup {
					continue
				}
				seen[sid] = struct{}{}
				out = append(out, sid)
			}
		}
	}

	for _, sid := range parseContractSkillsRequired(contract.Structured) {
		if sid == "" {
			continue
		}
		if _, dup := seen[sid]; dup {
			continue
		}
		seen[sid] = struct{}{}
		out = append(out, sid)
	}
	return out
}

// parseContractSkillsRequired reads the contract's Structured payload
// and returns the value of `skills_required` when present and a JSON
// array of strings. Returns nil on any decode error or unexpected
// shape — the renderer must tolerate contracts that don't declare the
// field (true for every contract before sty_447b9fe0 lands).
func parseContractSkillsRequired(structured []byte) []string {
	if len(structured) == 0 {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal(structured, &raw); err != nil {
		return nil
	}
	v, ok := raw["skills_required"]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// citedPrincipleIDs returns the set of principle ids cited by either
// the contract body or the story body (regex pr_[a-z0-9_]+). Stable
// sorted order so output is deterministic.
func citedPrincipleIDs(contractBody, storyBody string) []string {
	seen := make(map[string]struct{}, 8)
	for _, body := range []string{contractBody, storyBody} {
		for _, m := range principleCiteRE.FindAllString(body, -1) {
			seen[m] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// fetchCitedPrinciples loads the active principle set for the project
// and returns only those whose Name matches an id in citedIDs.
// Principles cited by id but no longer active (or not present) are
// silently dropped; the executor can read the citation in the prose
// without the body if needed.
func (c *Client) fetchCitedPrinciples(ctx context.Context, projectID string, memberships, citedIDs []string) []document.Document {
	if len(citedIDs) == 0 || c.deps.Documents == nil {
		return nil
	}
	want := make(map[string]struct{}, len(citedIDs))
	for _, id := range citedIDs {
		want[id] = struct{}{}
	}
	out := make([]document.Document, 0, len(citedIDs))

	sysRows, _ := c.deps.Documents.List(ctx, document.ListOptions{
		Type:  document.TypePrinciple,
		Scope: document.ScopeSystem,
		Limit: 200,
	}, nil)
	for _, r := range sysRows {
		if r.Status != document.StatusActive {
			continue
		}
		if _, ok := want[r.Name]; ok {
			out = append(out, r)
		}
	}
	if projectID != "" {
		projRows, _ := c.deps.Documents.List(ctx, document.ListOptions{
			Type:      document.TypePrinciple,
			Scope:     document.ScopeProject,
			ProjectID: projectID,
			Limit:     200,
		}, memberships)
		for _, r := range projRows {
			if r.Status != document.StatusActive {
				continue
			}
			if _, ok := want[r.Name]; ok {
				out = append(out, r)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
