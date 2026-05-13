package story

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// Template is the parsed shape of a story_template document. Sty_d2a03cea.
// Templates live as system-scope documents with type=story_template; one
// per category. The structured payload carried in Document.Structured is
// the source of truth — this type is the in-process representation used
// by the lifecycle evaluator.
type Template struct {
	Category string                  `json:"category"`
	Fields   []TemplateField         `json:"fields"`
	Hooks    map[string]TemplateHook `json:"hooks"`
}

// TemplateField declares one schema field on a story of this category.
// Prompt is natural language describing what the field captures —
// surfaced verbatim to humans and agents that fill the value.
type TemplateField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Prompt   string `json:"prompt"`
}

// TemplateHook bundles the per-status enforcement: machine-evaluable
// structured checks (which block the transition on failure) and
// natural-language guidance (recorded but not enforced today —
// future-proofing for agent-evaluated hooks).
type TemplateHook struct {
	Structured      []TemplateStructuredCheck `json:"structured"`
	NaturalLanguage []string                  `json:"natural_language"`
}

// TemplateStructuredCheck is one machine-evaluable hook. The Type field
// names the check kind; remaining fields are check-specific. Unknown
// types are treated as warnings (logged, not enforced) so a future
// template can declare a hook the substrate doesn't yet implement
// without bricking transitions on existing stories.
type TemplateStructuredCheck struct {
	Type    string `json:"type"`
	Field   string `json:"field,omitempty"`
	Pattern string `json:"pattern,omitempty"`
	Tag     string `json:"tag,omitempty"`
	Message string `json:"message,omitempty"`
}

// LoadTemplate parses a story_template Document's Structured payload
// into a Template. Returns an error when the JSON is malformed.
func LoadTemplate(d document.Document) (Template, error) {
	if d.Type != document.TypeStoryTemplate {
		return Template{}, fmt.Errorf("story: document type %q is not a story_template", d.Type)
	}
	if len(d.Structured) == 0 {
		return Template{}, fmt.Errorf("story: template %q has no structured payload", d.Name)
	}
	var t Template
	if err := json.Unmarshal(d.Structured, &t); err != nil {
		return Template{}, fmt.Errorf("story: parse template %q: %w", d.Name, err)
	}
	if t.Category == "" {
		return Template{}, fmt.Errorf("story: template %q missing category", d.Name)
	}
	return t, nil
}

// EvaluationContext is what hook evaluation needs beyond the story
// itself: a ledger reader scoped to the story (for evidence-presence
// checks). Future hooks may need more — keep the surface narrow until
// they do.
type EvaluationContext struct {
	// LedgerEntriesForStory returns the ledger rows attached to the
	// given story id. Used by ledger_evidence_present checks. Returning
	// an error from this hook is treated as a check failure, not as a
	// substrate error.
	LedgerEntriesForStory func(ctx context.Context, storyID string) ([]ledger.LedgerEntry, error)
}

// EvaluateTransition runs the structured checks declared for `target`
// against the story and the EvaluationContext. Returns a slice of
// human-readable failure messages — empty slice means the transition
// is allowed. Unknown check types log nothing and pass; that's the
// "future hook" forward-compat slot.
//
// Natural-language hooks are not evaluated here — they're surfaced to
// the caller via Hook.NaturalLanguage so an agent or human can act on
// them, but they don't block transitions in this PR.
func (t Template) EvaluateTransition(ctx context.Context, target string, st Story, ev EvaluationContext) []string {
	hook, ok := t.Hooks[target]
	if !ok {
		return nil
	}
	var failures []string
	for _, check := range hook.Structured {
		if msg := check.evaluate(ctx, st, ev); msg != "" {
			failures = append(failures, msg)
		}
	}
	return failures
}

// evaluate runs a single structured check. Returns "" when the check
// passes, or a human-readable failure message when it fails.
func (c TemplateStructuredCheck) evaluate(ctx context.Context, st Story, ev EvaluationContext) string {
	switch c.Type {
	case "field_present":
		if c.Field == "" {
			return "" // misconfigured check — skip rather than block
		}
		v, ok := st.Fields[c.Field]
		if !ok || isBlank(v) {
			return c.messageOr(fmt.Sprintf("required field %q is empty — set it via story_update (fields={%q:…}) before this transition", c.Field, c.Field))
		}
		return ""
	case "regex_match":
		if c.Field == "" || c.Pattern == "" {
			return ""
		}
		v, ok := st.Fields[c.Field]
		if !ok {
			return c.messageOr(fmt.Sprintf("field %q must match pattern %q (field is unset)", c.Field, c.Pattern))
		}
		s, _ := v.(string)
		matched, err := regexp.MatchString(c.Pattern, s)
		if err != nil || !matched {
			return c.messageOr(fmt.Sprintf("field %q value %q does not match pattern %q", c.Field, s, c.Pattern))
		}
		return ""
	case "ledger_evidence_present":
		if ev.LedgerEntriesForStory == nil {
			return c.messageOr("ledger evidence required, but ledger reader unavailable in this context")
		}
		entries, err := ev.LedgerEntriesForStory(ctx, st.ID)
		if err != nil {
			return c.messageOr(fmt.Sprintf("ledger evidence check failed: %v", err))
		}
		for _, entry := range entries {
			if c.Tag == "" {
				return ""
			}
			for _, tag := range entry.Tags {
				if tag == c.Tag {
					return ""
				}
			}
		}
		return c.messageOr(fmt.Sprintf("ledger evidence with tag %q is required before this transition", c.Tag))
	default:
		// Unknown check type — forward-compat slot, no block.
		return ""
	}
}

func (c TemplateStructuredCheck) messageOr(fallback string) string {
	if strings.TrimSpace(c.Message) != "" {
		return c.Message
	}
	return fallback
}

// isBlank reports whether v is "absent" for hook purposes — empty
// string, empty []any, empty map[string]any. Non-string non-collection
// values are treated as present.
func isBlank(v any) bool {
	switch val := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(val) == ""
	case []any:
		return len(val) == 0
	case map[string]any:
		return len(val) == 0
	}
	return false
}

// TemplateLookup is the abstraction story_update (status branch) calls
// to resolve a category → Template. Implementations look up the
// system-scope document with type=story_template + name=category.
// nil-safe consumers (no template for a given category just means no
// hooks) keep the substrate functional even when the seed didn't run.
type TemplateLookup interface {
	GetByCategory(ctx context.Context, category string) (Template, bool)
}
