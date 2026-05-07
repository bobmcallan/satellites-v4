package configseed

import (
	"encoding/json"
	"fmt"

	"github.com/bobmcallan/satellites/internal/document"
)

// agentToInput builds a document.UpsertInput for a kind=agent file.
// Frontmatter carries the agent's structured payload directly:
// permission_patterns + skill_refs flow into AgentSettings; any other
// keys (e.g. tool_ceiling for the orchestrator) are merged into the
// raw JSON structured payload alongside.
//
// sty_e2512dbd: parsers stamp every system-tier UpsertInput with
// WorkspaceID="" — the system tier is non-tenant. RunProject
// overrides WorkspaceID/ProjectID/Scope when re-targeting the same
// inputs at the project tier; the workspaceID parameter is preserved
// only for signature symmetry with that retargeting path.
func agentToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("agent: name required")
	}
	// AgentSettings is the documented narrow subset; serialize that
	// first, then layer non-AgentSettings keys on top so callers like
	// the orchestrator (tool_ceiling) round-trip too.
	settings := document.AgentSettings{
		PermissionPatterns: fm.StringSlice("permission_patterns"),
		SkillRefs:          fm.StringSlice("skill_refs"),
	}
	base, err := document.MarshalAgentSettings(settings)
	if err != nil {
		return document.UpsertInput{}, fmt.Errorf("agent %q: marshal: %w", name, err)
	}
	merged, err := mergeFrontmatterIntoJSON(base, fm, structuredAgentSkipKeys)
	if err != nil {
		return document.UpsertInput{}, fmt.Errorf("agent %q: merge: %w", name, err)
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeAgent,
		Name:        name,
		Body:        body,
		Structured:  merged,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed"),
		Actor:       actor,
	}, nil
}

// contractToInput builds a document.UpsertInput for a kind=contract file.
//
// Contract documents carry only contract-level concerns: category,
// required_categories, evidence_required, and validation_mode. The
// action-claim path sources permission_patterns exclusively from the
// claiming agent document (story_b39b393f / story_cc55e093).
func contractToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("contract: name required")
	}
	payload := map[string]any{
		"category":            fm.String("category"),
		"required_categories": fm.StringSlice("required_categories"),
		"evidence_required":   fm.String("evidence_required"),
		"validation_mode":     fm.String("validation_mode"),
	}
	pruneEmpty(payload)
	structured, err := json.Marshal(payload)
	if err != nil {
		return document.UpsertInput{}, fmt.Errorf("contract %q: marshal: %w", name, err)
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeContract,
		Name:        name,
		Body:        body,
		Structured:  structured,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed"),
		Actor:       actor,
	}, nil
}

// workflowToInput builds a document.UpsertInput for a kind=workflow file.
// After epic:configuration-over-code-mandate (story_af79cf95) workflow
// documents carry only prose body — the substrate no longer parses
// required_slots frontmatter; the orchestrator and reviewer agents
// read the body for context and the mandate principle defines the
// floor.
func workflowToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("workflow: name required")
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeWorkflow,
		Name:        name,
		Body:        body,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed"),
		Actor:       actor,
	}, nil
}

// principleToInput builds a document.UpsertInput for a kind=principle
// file. Frontmatter carries `name` (required, the friendly principle
// title) plus optional `id`, `scope`, `tags`. Body is the principle's
// description text. story_ac3dc4d0.
//
// `id` is read but not propagated to UpsertInput — UpsertInput dedups
// by Name (Upsert.GetByName at internal/document/surreal.go:197). The
// `id` frontmatter field serves as a documentation slug matching the
// filename stem, so a reader of pr_agile.md sees `id: pr_agile`
// without having to infer it.
func principleToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("principle: name required")
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypePrinciple,
		Name:        name,
		Body:        body,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed"),
		Actor:       actor,
	}, nil
}

// storyTemplateToInput builds a document.UpsertInput for a
// kind=story_template file. The frontmatter declares the per-category
// schema (name, fields, hooks); the parser walks it into a structured
// JSON payload the story package consumes via story.LoadTemplate.
//
// One template per category. The `category` frontmatter key is the
// canonical identity; `name` mirrors it for the document's name column
// (Document.Name is the dedup key on Upsert). Sty_d2a03cea.
func storyTemplateToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	category := fm.String("category")
	if category == "" {
		return document.UpsertInput{}, fmt.Errorf("story_template: category required")
	}
	name := fm.String("name")
	if name == "" {
		name = category
	}
	payload := map[string]any{
		"category": category,
		"fields":   storyTemplateFields(fm["fields"]),
		"hooks":    storyTemplateHooks(fm["hooks"]),
	}
	structured, err := json.Marshal(payload)
	if err != nil {
		return document.UpsertInput{}, fmt.Errorf("story_template %q: marshal: %w", category, err)
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeStoryTemplate,
		Name:        name,
		Body:        body,
		Structured:  structured,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed", "story-template"),
		Actor:       actor,
	}, nil
}

// storyTemplateFields normalises the `fields:` frontmatter array into
// the canonical per-field shape: name, type, required, prompt. Unknown
// or malformed entries are skipped (per configseed's fail-loud-on-file
// pattern; structural error is what would matter, not field shape).
func storyTemplateFields(raw any) []map[string]any {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		m := asMap(entry)
		if m == nil {
			continue
		}
		name := stringOf(m["name"])
		if name == "" {
			continue
		}
		out = append(out, map[string]any{
			"name":     name,
			"type":     stringOf(m["type"]),
			"required": boolOf(m["required"]),
			"prompt":   stringOf(m["prompt"]),
		})
	}
	return out
}

// storyTemplateHooks normalises the `hooks:` frontmatter into a
// per-status object carrying `structured` (machine-evaluable checks)
// and `natural_language` (free-text instructions). Both branches are
// preserved verbatim so the structured branch can drive enforcement
// while the natural-language branch surfaces in story_get for agents
// and humans.
func storyTemplateHooks(raw any) map[string]any {
	source := asMap(raw)
	if source == nil {
		return nil
	}
	out := make(map[string]any, len(source))
	for status, v := range source {
		entry := asMap(v)
		if entry == nil {
			continue
		}
		structured := []map[string]any{}
		if list, ok := entry["structured"].([]any); ok {
			for _, item := range list {
				m := asMap(item)
				if m == nil {
					continue
				}
				structured = append(structured, m)
			}
		}
		natural := []string{}
		if list, ok := entry["natural_language"].([]any); ok {
			for _, item := range list {
				if s, ok := item.(string); ok && s != "" {
					natural = append(natural, s)
				}
			}
		}
		out[status] = map[string]any{
			"structured":       structured,
			"natural_language": natural,
		}
	}
	return out
}

// replicateVocabularyToInput builds a document.UpsertInput for a
// kind=replicate_vocabulary file. The frontmatter's `aliases:` map
// is the source of truth — alias strings as keys, canonical
// action-type strings as values. The parser validates nothing about
// the values (the runner enforces IsKnownAction at resolve time);
// authoring mistakes surface there. Sty_088f6d5c.
func replicateVocabularyToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("replicate_vocabulary: name required")
	}
	rawAliases := asMap(fm["aliases"])
	aliases := make(map[string]string, len(rawAliases))
	for alias, v := range rawAliases {
		canon := stringOf(v)
		if canon == "" {
			continue
		}
		aliases[alias] = canon
	}
	payload := map[string]any{"aliases": aliases}
	structured, err := json.Marshal(payload)
	if err != nil {
		return document.UpsertInput{}, fmt.Errorf("replicate_vocabulary %q: marshal: %w", name, err)
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeReplicateVocabulary,
		Name:        name,
		Body:        body,
		Structured:  structured,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed", "replicate-vocabulary"),
		Actor:       actor,
	}, nil
}

// artifactToInput builds a document.UpsertInput for a kind=artifact file.
// Frontmatter carries `name` (required) and `tags`; the body is the
// artifact content. Used today for the system-scope `default_agent_process`
// handshake markdown the MCP server returns to connecting clients —
// authors edit a file under config/seed/system/artifacts/ instead of patching a
// Go string constant. Sty_6c3f8091.
func artifactToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("artifact: name required")
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeArtifact,
		Name:        name,
		Body:        body,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed"),
		Actor:       actor,
	}, nil
}

// skillToInput builds a document.UpsertInput for a kind=skill file.
// Skills bind to agents via agent.skill_refs (story_b1108d4a) — the
// frontmatter `contract_binding` is preserved when present (legacy rows
// still carry it) but is no longer required.
//
// Sty_92271886 introduced the parser to support the workspace-tier seed
// phase. The system-tier Run does not walk skills (per the
// "Skills and reviewers are ad-hoc, not baseline" project principle);
// RunWorkspace is the only loader that consumes skill files today.
func skillToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("skill: name required")
	}
	in := document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeSkill,
		Name:        name,
		Body:        body,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed"),
		Actor:       actor,
	}
	if binding := fm.String("contract_binding"); binding != "" {
		in.ContractBinding = &binding
	}
	return in, nil
}

// reviewerToInput builds a document.UpsertInput for a kind=reviewer file.
// Reviewer docs require `contract_binding` per Document.Validate.
// Sty_92271886.
func reviewerToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	name := fm.String("name")
	if name == "" {
		return document.UpsertInput{}, fmt.Errorf("reviewer: name required")
	}
	binding := fm.String("contract_binding")
	if binding == "" {
		return document.UpsertInput{}, fmt.Errorf("reviewer %q: contract_binding required", name)
	}
	return document.UpsertInput{
		WorkspaceID:     "",
		ProjectID:       nil,
		Type:            document.TypeReviewer,
		Name:            name,
		Body:            body,
		Scope:           document.ScopeSystem,
		ContractBinding: &binding,
		Tags:            appendDistinct(fm.StringSlice("tags"), "seed", "configseed"),
		Actor:           actor,
	}, nil
}

// helpToInput builds a document.UpsertInput for a kind=help file.
// Sibling story_cc5c67a9 owns the type=help discriminator + portal
// surface; the loader entry point lives here so the runner stays
// kind-agnostic.
func helpToInput(fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	slug := fm.String("slug")
	if slug == "" {
		return document.UpsertInput{}, fmt.Errorf("help: slug required")
	}
	title := fm.String("title")
	if title == "" {
		return document.UpsertInput{}, fmt.Errorf("help %q: title required", slug)
	}
	payload := map[string]any{
		"title": title,
		"slug":  slug,
		"order": intOf(fm["order"]),
	}
	structured, err := json.Marshal(payload)
	if err != nil {
		return document.UpsertInput{}, fmt.Errorf("help %q: marshal: %w", slug, err)
	}
	return document.UpsertInput{
		WorkspaceID: "",
		ProjectID:   nil,
		Type:        document.TypeHelp,
		Name:        slug,
		Body:        body,
		Structured:  structured,
		Scope:       document.ScopeSystem,
		Tags:        appendDistinct(fm.StringSlice("tags"), "seed", "configseed", "help"),
		Actor:       actor,
	}, nil
}

// structuredAgentSkipKeys is the frontmatter-key set already consumed
// by AgentSettings; mergeFrontmatterIntoJSON skips these to avoid
// double-encoding. "name" and "tags" are also out of band — they live
// on the Document, not the Structured payload.
var structuredAgentSkipKeys = map[string]struct{}{
	"name":                {},
	"tags":                {},
	"permission_patterns": {},
	"skill_refs":          {},
}

// mergeFrontmatterIntoJSON layers extra frontmatter keys on top of
// base. base is the canonical structured payload; non-skip frontmatter
// keys merge into it without overwriting existing values.
func mergeFrontmatterIntoJSON(base []byte, fm Frontmatter, skip map[string]struct{}) ([]byte, error) {
	out := map[string]any{}
	if len(base) > 0 {
		if err := json.Unmarshal(base, &out); err != nil {
			return nil, err
		}
	}
	for k, v := range fm {
		if _, omit := skip[k]; omit {
			continue
		}
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}
	return json.Marshal(out)
}

// pruneEmpty drops zero-value entries from m so the marshalled JSON
// stays free of empty fields (better diff signal in stored payloads).
func pruneEmpty(m map[string]any) {
	for k, v := range m {
		switch typed := v.(type) {
		case string:
			if typed == "" {
				delete(m, k)
			}
		case []string:
			if len(typed) == 0 {
				delete(m, k)
			}
		case nil:
			delete(m, k)
		}
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

// asMap normalises the various shapes YAML decoders use for nested
// objects (Frontmatter, map[string]any, map[any]any) into a single
// map[string]any. Returns nil when the value is not a map.
func asMap(v any) map[string]any {
	switch typed := v.(type) {
	case map[string]any:
		return typed
	case Frontmatter:
		return map[string]any(typed)
	case map[any]any:
		out := make(map[string]any, len(typed))
		for k, val := range typed {
			if ks, ok := k.(string); ok {
				out[ks] = val
			}
		}
		return out
	}
	return nil
}

func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// appendDistinct merges extras into seed without introducing duplicates.
func appendDistinct(seed []string, extras ...string) []string {
	seen := make(map[string]struct{}, len(seed))
	out := make([]string, 0, len(seed)+len(extras))
	for _, s := range seed {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, s := range extras {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
