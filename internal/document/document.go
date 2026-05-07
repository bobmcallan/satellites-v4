// Package document is the satellites-v4 document primitive: a unified
// row in SurrealDB discriminated by `type` per docs/architecture.md §2.
// One schema covers artifacts, contracts, skills, principles, and
// reviewers; per-type behaviour layers on top of the shared store.
package document

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Type enum values per docs/architecture.md §2 Documents sub-section.
const (
	TypeArtifact  = "artifact"
	TypeContract  = "contract"
	TypeSkill     = "skill"
	TypePrinciple = "principle"
	TypeReviewer  = "reviewer"
	TypeAgent     = "agent"
	TypeRole      = "role"
	// TypeWorkflow names a system-scope document that defines the
	// required-slot shape for a workflow (e.g. plan → develop → push
	// → merge_to_main → story_close). Seeded from
	// config/seed/system/workflows/*.md by configseed. story_7bfd629c.
	TypeWorkflow = "workflow"
	// TypeHelp names a system-scope help page rendered by the portal
	// /help routes. Seeded from config/help/*.md by configseed.
	// story_cc5c67a9.
	TypeHelp = "help"
	// TypeStoryTemplate names a system-scope document declaring the
	// shape, lifecycle hooks, and natural-language field prompts for a
	// story category (bug, feature, improvement, infrastructure,
	// documentation). One template per category. Seeded from
	// config/seed/story_templates/*.md by configseed. The template's
	// structured payload carries `category`, `fields[]`, and
	// `hooks{<status>: { structured: [...], natural_language: [...]}}`.
	// Story_d2a03cea.
	TypeStoryTemplate = "story_template"
	// TypeReplicateVocabulary names a system-scope document mapping
	// natural-language aliases to portal_replicate canonical action
	// types (navigate, wait_visible, click, dom_snapshot, ...). One
	// document per installation; multiple documents merge in load
	// order. Seeded from config/seed/replicate_vocabulary/*.md.
	// Sty_088f6d5c.
	TypeReplicateVocabulary = "replicate_vocabulary"
)

// Scope enum values per docs/architecture.md §2.
const (
	ScopeSystem    = "system"
	ScopeProject   = "project"
	ScopeWorkspace = "workspace"
	// ScopeUser names a per-user override row. Story_f0a78759 (S5)
	// adds the tier to support user-level workflow markdowns. The
	// owning user is carried by `CreatedBy`; WorkspaceID is required;
	// ProjectID must be nil.
	ScopeUser = "user"
)

// Status enum values.
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

var validTypes = map[string]struct{}{
	TypeArtifact:            {},
	TypeContract:            {},
	TypeSkill:               {},
	TypePrinciple:           {},
	TypeReviewer:            {},
	TypeAgent:               {},
	TypeRole:                {},
	TypeWorkflow:            {},
	TypeHelp:                {},
	TypeStoryTemplate:       {},
	TypeReplicateVocabulary: {},
}

var validScopes = map[string]struct{}{
	ScopeSystem:    {},
	ScopeProject:   {},
	ScopeWorkspace: {},
	ScopeUser:      {},
}

// Document is the unified, type-discriminated row backing every authored
// content kind in satellites-v4. Every row scopes to exactly one workspace
// (per docs/architecture.md §8) and — when Scope=ScopeProject — to exactly
// one project (per principle pr_7ade92ae). Scope=ScopeSystem rows have nil
// ProjectID and are globally readable inside their workspace.
type Document struct {
	ID              string    `json:"id"`
	WorkspaceID     string    `json:"workspace_id"`
	ProjectID       *string   `json:"project_id,omitempty"`
	Type            string    `json:"type"`
	Name            string    `json:"name"`
	Body            string    `json:"body"`
	Structured      []byte    `json:"structured,omitempty"`
	ContractBinding *string   `json:"contract_binding,omitempty"`
	Scope           string    `json:"scope"`
	Tags            []string  `json:"tags,omitempty"`
	Status          string    `json:"status"`
	BodyHash        string    `json:"body_hash"`
	Version         int       `json:"version"`
	CreatedAt       time.Time `json:"created_at"`
	CreatedBy       string    `json:"created_by,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
	UpdatedBy       string    `json:"updated_by,omitempty"`

	// BestChunkScore is populated transiently by SearchSemantic with the
	// cosine similarity of the highest-scoring chunk that backed this
	// match. Not persisted; nil on rows that didn't come through a
	// semantic search path.
	BestChunkScore *float32 `json:"best_chunk_score,omitempty"`
}

// Validate returns the first invariant violation on d, or nil if d is
// well-formed. Validate covers shape only; FK existence on
// ContractBinding is enforced by the store at write time.
func (d Document) Validate() error {
	if _, ok := validTypes[d.Type]; !ok {
		return fmt.Errorf("document: invalid type %q", d.Type)
	}
	if _, ok := validScopes[d.Scope]; !ok {
		return fmt.Errorf("document: invalid scope %q", d.Scope)
	}
	switch d.Scope {
	case ScopeProject:
		if d.ProjectID == nil || *d.ProjectID == "" {
			return errors.New("document: project_id required when scope=project")
		}
	case ScopeSystem:
		// sty_e2512dbd: system tier is non-tenant — neither workspace
		// nor project. Stamping a workspace makes the row look
		// tenant-owned and pulls downstream readers (e.g. task_add)
		// into that tenancy when they shouldn't.
		if d.ProjectID != nil && *d.ProjectID != "" {
			return errors.New("document: project_id must be nil when scope=system")
		}
		if d.WorkspaceID != "" {
			return errors.New("document: workspace_id must be empty when scope=system")
		}
	case ScopeWorkspace:
		// sty_92271886: workspace tier hosts the shared work docs that
		// span every project in a workspace (developer_agent /
		// releaser_agent + the develop/push/merge_to_main contracts and
		// the skills/reviewers they bind). Roles + workflows were the
		// initial set; agent/contract/skill/reviewer joined when the
		// substrate gained a workspace seed phase.
		switch d.Type {
		case TypeRole, TypeWorkflow, TypeAgent, TypeContract, TypeSkill, TypeReviewer:
			// allowed
		default:
			return fmt.Errorf("document: scope=workspace not valid for type=%s", d.Type)
		}
		if d.WorkspaceID == "" {
			return errors.New("document: workspace_id required when scope=workspace")
		}
		if d.ProjectID != nil && *d.ProjectID != "" {
			return errors.New("document: project_id must be nil when scope=workspace")
		}
	case ScopeUser:
		if d.Type != TypeWorkflow {
			return fmt.Errorf("document: scope=user only valid for type=workflow, got type=%s", d.Type)
		}
		if d.WorkspaceID == "" {
			return errors.New("document: workspace_id required when scope=user")
		}
		if d.ProjectID != nil && *d.ProjectID != "" {
			return errors.New("document: project_id must be nil when scope=user")
		}
		if d.CreatedBy == "" {
			return errors.New("document: created_by (user_id) required when scope=user")
		}
	}
	switch d.Type {
	case TypeReviewer:
		if d.ContractBinding == nil || *d.ContractBinding == "" {
			return fmt.Errorf("document: contract_binding required for type=%s", d.Type)
		}
	case TypeSkill:
		// story_b1108d4a: skill no longer requires contract_binding.
		// Skills bind to AGENTS via agent.skill_refs; the binding field
		// is preserved on the type for legacy rows but is optional.
	case TypeAgent:
		// agent documents may optionally pin to a contract via contract_binding
		// (mirrors reviewer); the field is permitted but not required.
		// story_b39b393f: Validate the AgentSettings shape so
		// permission_patterns / skill_refs are typed and unknown
		// fields surface as errors at write time.
		if err := validateAgentStructured(d.Structured); err != nil {
			return err
		}
	case TypeHelp:
		// Help docs are seed-driven system content. The body IS the
		// rendered page; an empty body would render a blank help page,
		// which is never useful. story_cc5c67a9.
		if strings.TrimSpace(d.Body) == "" {
			return errors.New("document: type=help requires non-empty body")
		}
		if d.ContractBinding != nil && *d.ContractBinding != "" {
			return errors.New("document: contract_binding allowed only for type=skill, type=reviewer, or type=agent")
		}
	default:
		if d.ContractBinding != nil && *d.ContractBinding != "" {
			return errors.New("document: contract_binding allowed only for type=skill, type=reviewer, or type=agent")
		}
	}
	return nil
}

// DocumentVersion captures a single prior body of a document. Rows are
// appended on every body-changing Update or Upsert; identical-body
// re-saves dedup via BodyHash and append no row. ListVersions returns
// these in Version DESC order. The live document.Document holds the
// current version; DocumentVersion holds historical snapshots.
type DocumentVersion struct {
	DocumentID string    `json:"document_id"`
	Version    int       `json:"version"`
	BodyHash   string    `json:"body_hash"`
	Body       string    `json:"body"`
	Structured []byte    `json:"structured,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	UpdatedBy  string    `json:"updated_by,omitempty"`
}

// versionFromDocument freezes a document's current state into a
// DocumentVersion suitable for append. Callers invoke this *before*
// mutating the live row so the returned version is the "prior" state.
func versionFromDocument(d Document) DocumentVersion {
	return DocumentVersion{
		DocumentID: d.ID,
		Version:    d.Version,
		BodyHash:   d.BodyHash,
		Body:       d.Body,
		Structured: d.Structured,
		UpdatedAt:  d.UpdatedAt,
		UpdatedBy:  d.UpdatedBy,
	}
}

// HashBody returns a sha256 content hash prefixed with "sha256:"; used as
// the equality test for Upsert's idempotence check.
func HashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// StringPtr returns nil for the empty string, otherwise a pointer to a
// fresh copy of s. Callers building Documents use it to honour the
// "ProjectID nil when scope=system" invariant without sprinkling
// conditional pointer construction across the call sites.
func StringPtr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}
