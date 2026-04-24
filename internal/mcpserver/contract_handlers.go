package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/bobmcallan/satellites/internal/contract"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
)

// specKeyTag is the tag convention the workflow_spec kv row uses.
// Kept low-cardinality so KVProjection (derivations slice 7.3) can
// collapse versions without a secondary index.
const specKeyTag = "key:workflow_spec"

// handleProjectWorkflowSpecGet loads the project's workflow_spec from
// the latest kv ledger row tagged key:workflow_spec. Falls back to
// DefaultWorkflowSpec when no row exists.
func (s *Server) handleProjectWorkflowSpecGet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	spec, err := s.loadWorkflowSpec(ctx, resolvedID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{"project_id": resolvedID, "spec": spec})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_workflow_spec_get").
		Str("project_id", resolvedID).
		Int("slot_count", len(spec.Slots)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleProjectWorkflowSpecSet persists a WorkflowSpec by appending a
// new kv row. Older rows stay in the audit chain; KVProjection reads
// the latest per key.
func (s *Server) handleProjectWorkflowSpecSet(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	projectID, err := req.RequireString("project_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	raw := req.GetString("slots", "")
	if raw == "" {
		return mcpgo.NewToolResultError("slots is required (JSON array)"), nil
	}
	var slots []contract.Slot
	if err := json.Unmarshal([]byte(raw), &slots); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("slots parse error: %v", err)), nil
	}
	if len(slots) == 0 {
		return mcpgo.NewToolResultError("slots must contain at least one entry"), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	resolvedID, err := s.resolveProjectID(ctx, projectID, caller, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	wsID := s.resolveProjectWorkspaceID(ctx, resolvedID)
	structured, _ := json.Marshal(contract.WorkflowSpec{Slots: slots})
	row, err := s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: wsID,
		ProjectID:   resolvedID,
		Type:        ledger.TypeKV,
		Tags:        []string{specKeyTag},
		Content:     "workflow_spec",
		Structured:  structured,
		CreatedBy:   caller.UserID,
	}, time.Now().UTC())
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	body, _ := json.Marshal(map[string]any{
		"project_id": resolvedID,
		"ledger_id":  row.ID,
		"spec":       contract.WorkflowSpec{Slots: slots},
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "project_workflow_spec_set").
		Str("project_id", resolvedID).
		Int("slot_count", len(slots)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleStoryWorkflowClaim validates proposed against the project's
// spec, resolves each contract_name to its document, and creates one
// ContractInstance per slot. Idempotent on re-claim — returns the
// existing CIs if a kind:workflow-claim row already exists for the
// story.
func (s *Server) handleStoryWorkflowClaim(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	claimMarkdown := req.GetString("claim_markdown", "")
	proposed := req.GetStringSlice("proposed_contracts", nil)

	memberships := s.resolveCallerMemberships(ctx, caller)
	st, err := s.stories.GetByID(ctx, storyID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}

	spec, err := s.loadWorkflowSpec(ctx, st.ProjectID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	if len(proposed) == 0 {
		proposed = expandDefaultProposed(spec)
	}
	if err := spec.Validate(proposed); err != nil {
		return mcpgo.NewToolResultText(marshalSpecError(err)), nil
	}

	// Idempotence — return existing CIs if the story already has a
	// workflow-claim row.
	existing, _ := s.contracts.List(ctx, storyID, memberships)
	if len(existing) > 0 {
		body, _ := json.Marshal(map[string]any{
			"story_id":           storyID,
			"claim_ledger_id":    "",
			"contract_instances": existing,
			"idempotent":         true,
		})
		return mcpgo.NewToolResultText(string(body)), nil
	}

	// Resolve each contract_name → document id. Prefer scope=system.
	resolved := make([]resolvedSlot, 0, len(proposed))
	for _, name := range proposed {
		doc, err := s.findContractDocByName(ctx, name, st.WorkspaceID)
		if err != nil {
			errBody, _ := json.Marshal(map[string]any{
				"error":         "unknown_contract",
				"contract_name": name,
				"message":       "no active document{type=contract} with this name",
			})
			return mcpgo.NewToolResultError(string(errBody)), nil
		}
		resolved = append(resolved, resolvedSlot{name: name, docID: doc.ID, required: specSlotRequired(spec, name)})
	}

	// Write the workflow-claim ledger row first so the CIs have a
	// parent audit row to reference.
	payload, _ := json.Marshal(map[string]any{"proposed_contracts": proposed})
	now := time.Now().UTC()
	claim, err := s.ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: st.WorkspaceID,
		ProjectID:   st.ProjectID,
		StoryID:     ledger.StringPtr(storyID),
		Type:        ledger.TypeWorkflowClaim,
		Tags:        []string{"kind:workflow-claim", "phase:pre-plan"},
		Content:     claimMarkdown,
		Structured:  payload,
		CreatedBy:   caller.UserID,
	}, now)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	cis := make([]contract.ContractInstance, 0, len(resolved))
	for i, slot := range resolved {
		ci, err := s.contracts.Create(ctx, contract.ContractInstance{
			StoryID:          storyID,
			ContractID:       slot.docID,
			ContractName:     slot.name,
			Phase:            slot.name,
			Sequence:         i,
			RequiredForClose: slot.required,
			Status:           contract.StatusReady,
		}, now)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("create CI %q: %v", slot.name, err)), nil
		}
		cis = append(cis, ci)
	}

	body, _ := json.Marshal(map[string]any{
		"story_id":           storyID,
		"claim_ledger_id":    claim.ID,
		"contract_instances": cis,
	})
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_workflow_claim").
		Str("story_id", storyID).
		Int("ci_count", len(cis)).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// handleStoryContractNext is read-only: returns the lowest-sequence
// ready CI plus skills bound to its contract document.
func (s *Server) handleStoryContractNext(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	start := time.Now()
	caller, _ := UserFrom(ctx)
	storyID, err := req.RequireString("story_id")
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	memberships := s.resolveCallerMemberships(ctx, caller)
	if _, err := s.stories.GetByID(ctx, storyID, memberships); err != nil {
		return mcpgo.NewToolResultError("story not found"), nil
	}
	cis, err := s.contracts.List(ctx, storyID, memberships)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	var next *contract.ContractInstance
	for i := range cis {
		if cis[i].Status == contract.StatusReady {
			next = &cis[i]
			break
		}
	}
	resp := map[string]any{"story_id": storyID}
	if next == nil {
		resp["contract_instance"] = nil
		resp["skills"] = nil
		body, _ := json.Marshal(resp)
		return mcpgo.NewToolResultText(string(body)), nil
	}
	resp["contract_instance"] = next
	// Skills: documents with type=skill + contract_binding matching.
	skills, _ := s.docs.List(ctx, document.ListOptions{
		Type:            document.TypeSkill,
		ContractBinding: next.ContractID,
	}, memberships)
	resp["skills"] = skills
	body, _ := json.Marshal(resp)
	s.logger.Info().
		Str("method", "tools/call").
		Str("tool", "story_contract_next").
		Str("story_id", storyID).
		Str("ci_id", next.ID).
		Int64("duration_ms", time.Since(start).Milliseconds()).
		Msg("mcp tool call")
	return mcpgo.NewToolResultText(string(body)), nil
}

// loadWorkflowSpec reads the project's latest kv row tagged
// key:workflow_spec and decodes its Structured payload. Falls back to
// DefaultWorkflowSpec when no row exists or decode fails.
func (s *Server) loadWorkflowSpec(ctx context.Context, projectID string, memberships []string) (contract.WorkflowSpec, error) {
	if s.ledger == nil {
		return contract.DefaultWorkflowSpec(), nil
	}
	rows, err := s.ledger.List(ctx, projectID, ledger.ListOptions{
		Type:  ledger.TypeKV,
		Tags:  []string{specKeyTag},
		Limit: 1,
	}, memberships)
	if err != nil {
		return contract.WorkflowSpec{}, fmt.Errorf("spec load: %w", err)
	}
	if len(rows) == 0 {
		return contract.DefaultWorkflowSpec(), nil
	}
	var spec contract.WorkflowSpec
	if err := json.Unmarshal(rows[0].Structured, &spec); err != nil || len(spec.Slots) == 0 {
		return contract.DefaultWorkflowSpec(), nil
	}
	return spec, nil
}

// findContractDocByName resolves a contract_name to a document{type=contract}.
// System-scope rows are preferred; workspace-scoped rows are the
// fallback so projects can override.
func (s *Server) findContractDocByName(ctx context.Context, name, workspaceID string) (document.Document, error) {
	candidates, err := s.docs.List(ctx, document.ListOptions{Type: document.TypeContract}, nil)
	if err != nil {
		return document.Document{}, err
	}
	var systemMatch, wsMatch *document.Document
	for i := range candidates {
		d := candidates[i]
		if d.Name != name || d.Status != document.StatusActive {
			continue
		}
		switch d.Scope {
		case document.ScopeSystem:
			if systemMatch == nil {
				systemMatch = &d
			}
		case document.ScopeProject:
			if d.WorkspaceID == workspaceID && wsMatch == nil {
				wsMatch = &d
			}
		}
	}
	if systemMatch != nil {
		return *systemMatch, nil
	}
	if wsMatch != nil {
		return *wsMatch, nil
	}
	return document.Document{}, errors.New("contract document not found")
}

type resolvedSlot struct {
	name     string
	docID    string
	required bool
}

func specSlotRequired(spec contract.WorkflowSpec, name string) bool {
	for _, slot := range spec.Slots {
		if slot.ContractName == name {
			return slot.Required
		}
	}
	return false
}

// expandDefaultProposed produces a proposed list from a spec using
// each required slot's MinCount.
func expandDefaultProposed(spec contract.WorkflowSpec) []string {
	out := make([]string, 0, len(spec.Slots))
	for _, slot := range spec.Slots {
		if !slot.Required {
			continue
		}
		n := slot.MinCount
		if n <= 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, slot.ContractName)
		}
	}
	return out
}

// marshalSpecError renders a *contract.SpecError as a JSON tool-result
// text. Non-spec errors are wrapped with a generic shape so callers can
// still parse them.
func marshalSpecError(err error) string {
	var se *contract.SpecError
	if errors.As(err, &se) {
		b, _ := json.Marshal(map[string]any{
			"error":         se.Kind,
			"contract_name": se.ContractName,
			"count":         se.Count,
			"min":           se.Min,
			"max":           se.Max,
			"message":       se.Error(),
		})
		return string(b)
	}
	b, _ := json.Marshal(map[string]any{
		"error":   "invalid_spec",
		"message": err.Error(),
	})
	return string(b)
}

// anySubstring is a tiny helper used by tests to match structured
// error bodies without importing strings at every call site.
func anySubstring(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}
