package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
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

// StoryUpdateStatusInput names the transition target. ID + Status
// are required. Memberships scope the GetByID + UpdateStatus
// lookups. Now is overridable for testing; zero falls through to
// time.Now().UTC().
type StoryUpdateStatusInput struct {
	ID          string
	Status      string
	Memberships []string
	Now         time.Time
}

// StoryUpdateStatus transitions a story to a new status. When the
// resolved category has a story_template, its lifecycle hooks for the
// target status are evaluated against the existing row; failures
// block the transition with a natural-language explanation. Missing
// template → no hooks → pass-through (forward-compat for categories
// without a template yet).
//
// Propagates story.ErrStoryHasOpenTasks unwrapped so wire-layer
// callers (the HTTP layer, Layer 3) can errors.Is on it and map to
// the documented 422 envelope.
func (c *Client) StoryUpdateStatus(ctx context.Context, caller Caller, in StoryUpdateStatusInput) (story.Story, error) {
	if c.deps.Stories == nil {
		return story.Story{}, errStoryStoreNotConfigured
	}
	if in.ID == "" {
		return story.Story{}, errors.New("id required")
	}
	if in.Status == "" {
		return story.Story{}, errors.New("status is required")
	}
	existing, err := c.deps.Stories.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return story.Story{}, errStoryNotFoundForUpdate
	}
	if t, ok := c.loadStoryTemplate(ctx, existing.Category); ok {
		ev := story.EvaluationContext{
			LedgerEntriesForStory: func(ctx context.Context, storyID string) ([]ledger.LedgerEntry, error) {
				if c.deps.Ledger == nil {
					return nil, nil
				}
				return c.deps.Ledger.List(ctx, existing.ProjectID, ledger.ListOptions{StoryID: storyID, Limit: 50}, in.Memberships)
			},
		}
		if failures := t.EvaluateTransition(ctx, in.Status, existing, ev); len(failures) > 0 {
			return story.Story{}, fmt.Errorf("transition blocked by %s story template:\n  - %s", existing.Category, strings.Join(failures, "\n  - "))
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Stories.UpdateStatus(ctx, in.ID, in.Status, caller.UserID, now, in.Memberships)
}

// StoryFieldSetInput names a single template-defined field write.
type StoryFieldSetInput struct {
	ID          string
	Field       string
	Value       string
	Memberships []string
	Now         time.Time
}

// StoryFieldSet writes a single template-defined value onto a story.
// Validates the field name against the resolved category template —
// fields not declared by the template are rejected with a list of
// what the template DOES declare.
func (c *Client) StoryFieldSet(ctx context.Context, caller Caller, in StoryFieldSetInput) (story.Story, error) {
	if c.deps.Stories == nil {
		return story.Story{}, errStoryStoreNotConfigured
	}
	if in.ID == "" {
		return story.Story{}, errors.New("id required")
	}
	if in.Field == "" {
		return story.Story{}, errors.New("field required")
	}
	existing, err := c.deps.Stories.GetByID(ctx, in.ID, in.Memberships)
	if err != nil {
		return story.Story{}, errStoryNotFoundForUpdate
	}
	if t, ok := c.loadStoryTemplate(ctx, existing.Category); ok {
		known := false
		names := make([]string, 0, len(t.Fields))
		for _, f := range t.Fields {
			names = append(names, f.Name)
			if f.Name == in.Field {
				known = true
				break
			}
		}
		if !known {
			return story.Story{}, fmt.Errorf("field %q is not declared by the %q story template (declared: %s)", in.Field, existing.Category, strings.Join(names, ", "))
		}
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return c.deps.Stories.SetField(ctx, in.ID, in.Field, in.Value, caller.UserID, now, in.Memberships)
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
