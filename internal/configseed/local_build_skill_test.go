package configseed

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// TestRunWorkspace_RealSeedLoadsLocalBuildSkill is the sty_6dffa6cf
// regression anchor. RunWorkspace on the real config/seed tree MUST
// produce:
//
//  1. A type=skill row named "local_build" at scope=workspace,
//     workspace_id=wksp_5b3257d1.
//  2. A type=agent row named "developer_agent" whose
//     AgentSettings.SkillRefs contains "local_build".
//
// Either part regressing means a self-modifying slice can ship without
// the build-and-swap evidence the substrate is supposed to teach the
// developer_agent.
func TestRunWorkspace_RealSeedLoadsLocalBuildSkill(t *testing.T) {
	t.Parallel()

	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	const ws = "wksp_5b3257d1"

	docs := document.NewMemoryStore()
	ctx := context.Background()
	now := time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC)

	if _, err := RunWorkspace(ctx, docs, seedDir, ws, "system", now); err != nil {
		t.Fatalf("RunWorkspace: %v", err)
	}

	// (1) skill is seed-loaded at workspace scope.
	skillRows, err := docs.List(ctx, document.ListOptions{
		Type:  document.TypeSkill,
		Scope: document.ScopeWorkspace,
	}, nil)
	if err != nil {
		t.Fatalf("List skills: %v", err)
	}
	var found document.Document
	for _, r := range skillRows {
		if r.Name == "local_build" {
			found = r
			break
		}
	}
	if found.Name == "" {
		t.Fatalf("skill %q not seed-loaded at scope=workspace; got rows=%+v", "local_build", skillRows)
	}
	if found.WorkspaceID != ws {
		t.Errorf("skill local_build WorkspaceID = %q, want %q", found.WorkspaceID, ws)
	}
	if found.ProjectID != nil {
		t.Errorf("skill local_build ProjectID = %v, want nil", found.ProjectID)
	}

	// (2) developer_agent's AgentSettings.SkillRefs contains
	// "local_build".
	agentDoc, err := docs.GetByName(ctx, "", "developer_agent", nil)
	if err != nil {
		t.Fatalf("GetByName developer_agent: %v", err)
	}
	settings, err := document.UnmarshalAgentSettings(agentDoc.Structured)
	if err != nil {
		t.Fatalf("UnmarshalAgentSettings: %v", err)
	}
	var hasRef bool
	for _, ref := range settings.SkillRefs {
		if ref == "local_build" {
			hasRef = true
			break
		}
	}
	if !hasRef {
		t.Errorf("developer_agent.SkillRefs = %v, want to contain %q", settings.SkillRefs, "local_build")
	}
}
