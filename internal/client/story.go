package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/agentprocess"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/tasklog"
)

// ErrStoryStoreNotConfiguredV2 (named to avoid collision with the
// task.go-declared sentinel that also signals story-store absence)
// is returned when the typed story surface is called against a
// Client whose Deps.Stories is nil.
//
// TODO: collapse to a single ErrStoryStoreNotConfigured once Slice C
// lands — both task.go and story.go currently check the same
// dependency and the per-file sentinels accumulate noise.
var errStoryStoreNotConfigured = errors.New("story store not configured")

// ErrStoryNotFoundV2 is the wire-shape error the story handlers
// historically returned when the story id does not resolve in the
// caller's workspaces. Distinct from task.go's ErrStoryNotFound so
// callers can map "story_not_found" envelope tags consistently.
var errStoryNotFoundForUpdate = errors.New("story not found")

// ErrStoryProjectIDRequired is returned when StoryAdd is called
// without a project id. Mirrors the historical "project_id is required"
// envelope produced by the MCP RequireString path.
var ErrStoryProjectIDRequired = errors.New("project_id is required")

// ErrStoryTitleRequired is returned when StoryAdd is called without
// a title. Mirrors the historical "title is required" envelope.
var ErrStoryTitleRequired = errors.New("title is required")

// ErrStoryIDRequired is returned when StoryUpdate is called without
// an id. Mirrors the historical "id is required" envelope.
var ErrStoryIDRequired = errors.New("id is required")

// validStoryCategories enumerates the categories accepted by
// StoryAdd / StoryUpdate. Kept on the typed surface because the
// store layer is intentionally schema-free on category strings.
var validStoryCategories = map[string]struct{}{
	"feature":        {},
	"bug":            {},
	"improvement":    {},
	"infrastructure": {},
	"documentation":  {},
}

// (StoryUpdateStatusInput / StoryUpdateStatus / StoryFieldSetInput /
// StoryFieldSet removed sty_4db0e025 slice D1 — folded into the
// consolidated StoryUpdate verb below. Status transitions still flow
// through *MemoryStore.UpdateStatus, preserving pr_story_terminal_gate.
// Template-field writes still flow through *MemoryStore.SetField with
// the same unknown-field rejection rule story_field_set used to
// enforce.)

// StoryGetInput identifies the story to retrieve. Memberships scope
// the GetByID lookup; cross-workspace stories return errStoryNotFoundForUpdate.
type StoryGetInput struct {
	ID          string
	Memberships []string
}

// StoryGetOutput is the orientation bundle the substrate returns on
// story_get. Mirrors the shape mcpserver.handleStoryGet emitted before
// the typed-method extraction (sty_73207fc8): the story row + owning
// project + recent ledger evidence + resolved agent_process + category
// template + project orientation (intent + principles).
type StoryGetOutput struct {
	Story          story.Story          `json:"story"`
	Project        *project.Project     `json:"project,omitempty"`
	RecentEvidence []ledger.LedgerEntry `json:"recent_evidence,omitempty"`
	AgentProcess   string               `json:"agent_process,omitempty"`
	Template       *story.Template      `json:"template,omitempty"`
	IntentBody     string               `json:"intent_body,omitempty"`
	Principles     []PrincipleEntry     `json:"principles,omitempty"`
}

// recentEvidenceLimit caps the recent_evidence section.
const recentEvidenceLimit = 10

// StoryGet returns the orientation bundle for the given story id.
// Workspace-scoped via memberships; cross-workspace stories return
// errStoryNotFoundForUpdate.
//
// Mirrors the prior mcpserver.handleStoryGet composer: pulls the story,
// the owning project (with intent + principles via BuildOrientation),
// recent ledger evidence, the resolved agent_process artifact, and
// the category template when one exists. Missing dependencies degrade
// individual sections; the call only fails when the story itself
// can't be resolved.
func (c *Client) StoryGet(ctx context.Context, caller Caller, in StoryGetInput) (out StoryGetOutput, err error) {
	defer func() {
		if err != nil {
			return
		}
		c.dispatchContextAudit(ctx, fetchAuditDispatchInput{
			Verb:        "story_get",
			ProjectID:   out.Story.ProjectID,
			WorkspaceID: out.Story.WorkspaceID,
			StoryID:     out.Story.ID,
			ArgsMap:     map[string]any{"id": in.ID},
			Caller:      caller,
		}, sectionsForStoryGetOutput(out))
	}()
	if c.deps.Stories == nil {
		err = errStoryStoreNotConfigured
		return
	}
	if in.ID == "" {
		err = errors.New("id required")
		return
	}
	memberships := in.Memberships
	if memberships == nil {
		memberships = c.ResolveCallerMemberships(ctx, caller)
	}
	st, getErr := c.deps.Stories.GetByID(ctx, in.ID, memberships)
	if getErr != nil {
		err = errStoryNotFoundForUpdate
		return
	}
	if _, rerr := c.ResolveProjectID(ctx, st.ProjectID, "", caller, memberships); rerr != nil {
		err = errStoryNotFoundForUpdate
		return
	}

	out.Story = st

	if c.deps.Projects != nil {
		if p, perr := c.deps.Projects.GetByID(ctx, st.ProjectID, memberships); perr == nil {
			out.Project = &p
			bundle := c.BuildOrientation(ctx, p)
			out.IntentBody = bundle.IntentBody
			out.Principles = bundle.Principles
		}
	}

	if c.deps.Ledger != nil {
		entries, lerr := c.deps.Ledger.List(ctx, st.ProjectID, ledger.ListOptions{
			StoryID: st.ID,
			Limit:   recentEvidenceLimit,
		}, memberships)
		if lerr == nil && len(entries) > 0 {
			out.RecentEvidence = entries
		}
	}

	if c.deps.Documents != nil {
		out.AgentProcess = agentprocess.Resolve(ctx, c.deps.Documents, st.ProjectID, nil)
	}

	if t, ok := c.loadStoryTemplate(ctx, st.Category); ok {
		out.Template = &t
	}

	return out, nil
}

// StoryAddInput captures the story_add request shape. The wire
// layer pre-resolves project + workspace ids (the MCP handler runs
// resolveProjectID + resolveProjectWorkspaceID before calling); the
// typed method requires those to be supplied so it stays free of
// wire-only request context. Now overrides the timestamp for
// deterministic tests; zero falls back to time.Now().UTC().
type StoryAddInput struct {
	ProjectID          string
	WorkspaceID        string
	Title              string
	Description        string
	AcceptanceCriteria string
	Priority           string
	Category           string
	Tags               []string
	Now                time.Time
}

// StoryAdd mints a new story row owned by caller.UserID. Returns
// the freshly-persisted story.Story; the wire layer marshals it
// unchanged. Default priority is "medium"; default category is
// "feature" — mirrors the historical MCP handler defaults so the
// JSON wire shape is byte-identical for omitted fields.
func (c *Client) StoryAdd(ctx context.Context, caller Caller, in StoryAddInput) (story.Story, error) {
	if c.deps.Stories == nil {
		return story.Story{}, errStoryStoreNotConfigured
	}
	if in.ProjectID == "" {
		return story.Story{}, ErrStoryProjectIDRequired
	}
	if in.Title == "" {
		return story.Story{}, ErrStoryTitleRequired
	}
	priority := in.Priority
	if priority == "" {
		priority = "medium"
	}
	category := in.Category
	if category == "" {
		category = "feature"
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	candidate := story.Story{
		WorkspaceID:        in.WorkspaceID,
		ProjectID:          in.ProjectID,
		Title:              in.Title,
		Description:        in.Description,
		AcceptanceCriteria: in.AcceptanceCriteria,
		Priority:           priority,
		Category:           category,
		Tags:               in.Tags,
		CreatedBy:          caller.UserID,
	}
	return c.deps.Stories.Create(ctx, candidate, now)
}

// StoryUpdateInput captures the per-call mutable subset for
// StoryUpdate. Each *string / *[]string carries pointer-presence
// semantics: nil means "field omitted, leave untouched"; a non-nil
// pointer to an empty value clears the field on the story row. This
// mirrors the wire-layer GetArguments()-presence test the MCP
// handler historically used.
//
// Status + Fields fold the prior story_update_status and
// story_field_set verbs into this single mutation surface
// (sty_4db0e025 slice D1):
//
//   - Status (non-nil) triggers a status transition. The store's
//     UpdateStatus enforces ValidTransition + pr_story_terminal_gate
//     (open-tasks check on done|cancelled); template lifecycle hooks
//     run before the store call, same as story_update_status did.
//   - Fields keys name template-declared field names; values follow
//     the same pointer-to-string presence semantics — an empty-string
//     value clears the field (the SetField "" → delete idiom), and
//     unknown field names are rejected against the resolved template
//     (the gate story_field_set used to apply).
type StoryUpdateInput struct {
	ID                 string
	Title              *string
	Description        *string
	AcceptanceCriteria *string
	Category           *string
	Priority           *string
	Tags               *[]string
	Status             *string
	Fields             map[string]*string
	Memberships        []string
	Now                time.Time
}

// StoryUpdate applies the input's optional fields to the story.
// Resolves the existing row first (workspace-scoped via memberships)
// to surface a "story not found" envelope on cross-workspace access,
// then applies the requested mutations in a defined order:
//
//  1. Template-field writes (Fields). Unknown fields are rejected
//     against the resolved category template — same rule
//     story_field_set enforced.
//  2. Non-status field updates (title, description, ...). Delegated
//     to the store's narrow Update verb.
//  3. Status transition (Status). Delegated to the store's
//     UpdateStatus verb, which fires ValidTransition +
//     pr_story_terminal_gate; template lifecycle hooks for the target
//     status run first.
//
// On any failure the call returns early — preceding mutations remain
// (the substrate has no transaction primitive across these three
// store calls), matching the prior verb-set behaviour where each
// verb was its own atomic store operation. Propagates
// story.ErrStoryHasOpenTasks unwrapped so wire-layer callers can
// errors.Is on it and map to the documented 422 envelope.
func (c *Client) StoryUpdate(ctx context.Context, caller Caller, in StoryUpdateInput) (story.Story, error) {
	if c.deps.Stories == nil {
		return story.Story{}, errStoryStoreNotConfigured
	}
	if in.ID == "" {
		return story.Story{}, ErrStoryIDRequired
	}
	existing, err := c.deps.Stories.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return story.Story{}, errStoryNotFoundForUpdate
	}
	if in.Category != nil && *in.Category != "" {
		if _, ok := validStoryCategories[*in.Category]; !ok {
			return story.Story{}, fmt.Errorf("invalid category %q (allowed: feature | bug | improvement | infrastructure | documentation)", *in.Category)
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	updated := existing
	// 1. Template fields (folded from story_field_set).
	if len(in.Fields) > 0 {
		var template story.Template
		var hasTemplate bool
		if t, ok := c.loadStoryTemplate(ctx, existing.Category); ok {
			template = t
			hasTemplate = true
		}
		for name, value := range in.Fields {
			if name == "" {
				return story.Story{}, errors.New("field name required")
			}
			if value == nil {
				continue
			}
			if hasTemplate {
				known := false
				declared := make([]string, 0, len(template.Fields))
				for _, f := range template.Fields {
					declared = append(declared, f.Name)
					if f.Name == name {
						known = true
					}
				}
				if !known {
					return story.Story{}, fmt.Errorf("field %q is not declared by the %q story template (declared: %s)", name, existing.Category, strings.Join(declared, ", "))
				}
			}
			updated, err = c.deps.Stories.SetField(ctx, in.ID, name, *value, caller.UserID, now, in.Memberships)
			if err != nil {
				return story.Story{}, err
			}
		}
	}

	// 2. Non-status field updates (folded from the prior StoryUpdate body).
	if in.Title != nil || in.Description != nil || in.AcceptanceCriteria != nil || in.Category != nil || in.Priority != nil || in.Tags != nil {
		fields := story.UpdateFields{
			Title:              in.Title,
			Description:        in.Description,
			AcceptanceCriteria: in.AcceptanceCriteria,
			Category:           in.Category,
			Priority:           in.Priority,
			Tags:               in.Tags,
		}
		updated, err = c.deps.Stories.Update(ctx, in.ID, fields, caller.UserID, now, in.Memberships)
		if err != nil {
			return story.Story{}, err
		}
	}

	// 3. Status transition (folded from story_update_status).
	if in.Status != nil {
		if *in.Status == "" {
			return story.Story{}, errors.New("status is required")
		}
		if t, ok := c.loadStoryTemplate(ctx, updated.Category); ok {
			ev := story.EvaluationContext{
				LedgerEntriesForStory: func(ctx context.Context, storyID string) ([]ledger.LedgerEntry, error) {
					if c.deps.Ledger == nil {
						return nil, nil
					}
					return c.deps.Ledger.List(ctx, updated.ProjectID, ledger.ListOptions{StoryID: storyID, Limit: 50}, in.Memberships)
				},
			}
			if failures := t.EvaluateTransition(ctx, *in.Status, updated, ev); len(failures) > 0 {
				return story.Story{}, fmt.Errorf("transition blocked by %s story template:\n  - %s", updated.Category, strings.Join(failures, "\n  - "))
			}
		}
		previousStatus := updated.Status
		updated, err = c.deps.Stories.UpdateStatus(ctx, in.ID, *in.Status, caller.UserID, now, in.Memberships)
		if err != nil {
			return story.Story{}, err
		}
		// sty_090f6183: emit KindStatusChange on each task scoped to
		// the story so subscribers to any task on the story observe
		// the story-level transition. Best-effort — telemetry never
		// fails the underlying typed-method call.
		if previousStatus != updated.Status && c.deps.Tasks != nil {
			tasks, terr := c.deps.Tasks.List(ctx, task.ListOptions{StoryID: in.ID, Limit: 200}, nil)
			if terr == nil {
				payload := map[string]any{
					"from":  previousStatus,
					"to":    updated.Status,
					"actor": caller.UserID,
				}
				for _, t := range tasks {
					c.publishTaskLog(ctx, t.ID, tasklog.KindStatusChange, payload)
				}
			}
		}
	}

	return updated, nil
}

// loadStoryTemplate resolves a category → story.Template by reading
// the system-scope document with type=story_template and name=category.
// Best-effort: missing or malformed templates return (zero, false),
// which the caller treats as "no hooks for this category".
//
// Mirrors mcpserver.Server.loadStoryTemplate. Inlined here so the
// typed surface has no wire-layer dependency.
func (c *Client) loadStoryTemplate(ctx context.Context, category string) (story.Template, bool) {
	if c.deps.Documents == nil || category == "" {
		return story.Template{}, false
	}
	doc, err := c.deps.Documents.GetByName(ctx, "", category, nil)
	if err != nil {
		return story.Template{}, false
	}
	if doc.Type != document.TypeStoryTemplate {
		return story.Template{}, false
	}
	t, err := story.LoadTemplate(doc)
	if err != nil {
		return story.Template{}, false
	}
	return t, true
}
