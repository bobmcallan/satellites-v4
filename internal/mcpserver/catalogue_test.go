package mcpserver

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
)

// stubMCPServerWithTools constructs a fresh mcp-go MCPServer carrying
// a small canned tool set for the catalogue tests. Two primitives,
// stable parameter shapes, mixed required/optional flags.
func stubMCPServerWithTools() *mcpserver.MCPServer {
	srv := mcpserver.NewMCPServer("test", "0.0.0")
	srv.AddTool(mcpgo.NewTool("story_get",
		mcpgo.WithDescription("Return a story by id."),
		mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Story id (sty_<8hex>).")),
	), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	srv.AddTool(mcpgo.NewTool("task_walk",
		mcpgo.WithDescription("Return the task chain for a story."),
		mcpgo.WithString("story_id", mcpgo.Required(), mcpgo.Description("Story whose walk should be returned.")),
		mcpgo.WithBoolean("include_closed", mcpgo.Description("Include closed tasks in the walk.")),
	), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	})
	return srv
}

func TestBuildCatalogue_ProjectsRegisteredTools(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	cat := BuildCatalogue(stubMCPServerWithTools(), now)
	if !cat.SnapshotAt.Equal(now) {
		t.Errorf("SnapshotAt = %v, want %v", cat.SnapshotAt, now)
	}
	if len(cat.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(cat.Tools))
	}
	// Sorted by name → story_get first.
	if cat.Tools[0].Name != "story_get" || cat.Tools[1].Name != "task_walk" {
		t.Errorf("tools out of order: %s, %s", cat.Tools[0].Name, cat.Tools[1].Name)
	}
	storyGet := cat.Tools[0]
	if storyGet.Description == "" {
		t.Errorf("story_get description empty")
	}
	if len(storyGet.Parameters) != 1 {
		t.Fatalf("story_get params = %d, want 1", len(storyGet.Parameters))
	}
	id := storyGet.Parameters[0]
	if id.Name != "id" || id.Type != "string" || !id.Required {
		t.Errorf("id param = %+v, want {id string required}", id)
	}
	if id.Description == "" {
		t.Errorf("id param description empty")
	}
	taskWalk := cat.Tools[1]
	if len(taskWalk.Parameters) != 2 {
		t.Fatalf("task_walk params = %d, want 2", len(taskWalk.Parameters))
	}
	// Parameters sorted by name → include_closed before story_id.
	if taskWalk.Parameters[0].Name != "include_closed" || taskWalk.Parameters[1].Name != "story_id" {
		t.Errorf("task_walk params order = %s, %s", taskWalk.Parameters[0].Name, taskWalk.Parameters[1].Name)
	}
	if taskWalk.Parameters[0].Required {
		t.Errorf("include_closed should be optional")
	}
	if !taskWalk.Parameters[1].Required {
		t.Errorf("story_id should be required")
	}
}

func TestBuildCatalogue_NilServerReturnsEmpty(t *testing.T) {
	t.Parallel()
	cat := BuildCatalogue(nil, time.Now())
	if len(cat.Tools) != 0 {
		t.Errorf("nil server tools = %d, want 0", len(cat.Tools))
	}
}

// TestMaterialiseCatalogue_WritesArtifact asserts the boot writer
// lands the catalogue in the document store as a system-scope
// artifact tagged kind:mcp-catalogue with body matching what
// BuildCatalogue emits.
func TestMaterialiseCatalogue_WritesArtifact(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	s := &Server{
		mcp:     stubMCPServerWithTools(),
		docs:    docs,
		logger:  satarbor.New("info"),
		nowFunc: func() time.Time { return now },
	}
	if err := s.MaterialiseCatalogue(context.Background()); err != nil {
		t.Fatalf("MaterialiseCatalogue: %v", err)
	}
	doc, err := docs.GetByName(context.Background(), "", CatalogueArtifactName, nil)
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if doc.Type != document.TypeArtifact {
		t.Errorf("type = %s, want artifact", doc.Type)
	}
	if doc.Scope != document.ScopeSystem {
		t.Errorf("scope = %s, want system", doc.Scope)
	}
	foundTag := false
	for _, tg := range doc.Tags {
		if tg == CatalogueKindTag {
			foundTag = true
			break
		}
	}
	if !foundTag {
		t.Errorf("tag %q missing from %v", CatalogueKindTag, doc.Tags)
	}
	var cat Catalogue
	if err := json.Unmarshal([]byte(doc.Body), &cat); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(cat.Tools) != 2 {
		t.Errorf("tools in body = %d, want 2", len(cat.Tools))
	}
}

// TestMaterialiseCatalogue_IdempotentOnSameTools asserts a re-boot
// with the same tool set short-circuits the upsert (body hash
// matches) — no version bump, no duplicate row.
func TestMaterialiseCatalogue_IdempotentOnSameTools(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	s := &Server{
		mcp:     stubMCPServerWithTools(),
		docs:    docs,
		logger:  satarbor.New("info"),
		nowFunc: func() time.Time { return now },
	}
	if err := s.MaterialiseCatalogue(context.Background()); err != nil {
		t.Fatalf("first materialise: %v", err)
	}
	first, _ := docs.GetByName(context.Background(), "", CatalogueArtifactName, nil)
	if err := s.MaterialiseCatalogue(context.Background()); err != nil {
		t.Fatalf("second materialise: %v", err)
	}
	second, _ := docs.GetByName(context.Background(), "", CatalogueArtifactName, nil)
	if first.Version != second.Version {
		t.Errorf("idempotent re-boot bumped version: %d → %d", first.Version, second.Version)
	}
	if first.ID != second.ID {
		t.Errorf("idempotent re-boot replaced row: %s → %s", first.ID, second.ID)
	}
}

// TestSeedRunDoesNotProduceCatalogueArtifact pins the boundary: the
// boot snapshot is owned by the boot path. A configseed.RunAll pass
// against the real seed dir must not produce a kind:mcp-catalogue
// artifact (and so cannot duplicate or overwrite it on re-seed).
func TestSeedRunDoesNotProduceCatalogueArtifact(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	helpDir, err := filepath.Abs(filepath.Join("..", "..", "config", "help"))
	if err != nil {
		t.Fatalf("abs help dir: %v", err)
	}
	if _, err := configseed.RunAll(context.Background(), docs, seedDir, helpDir, "wksp_sys", "system", time.Now().UTC()); err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	rows, err := docs.List(context.Background(), document.ListOptions{
		Type: document.TypeArtifact,
		Tags: []string{CatalogueKindTag},
	}, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("seed produced %d kind:mcp-catalogue artifacts; the snapshot is boot-owned", len(rows))
	}
}
