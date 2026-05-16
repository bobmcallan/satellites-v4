// Wire-shape coverage for the chain_* handlers (sty_4fb2d985). The
// router logic lives on *client.Client and is exercised in
// internal/client/chain_test.go; this file verifies the thin
// adapters: required-arg envelope, JSON round-trip, MCP-variant
// dispatched=false invariant.
package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// newChainTestServer builds a Server with MemoryStore-backed deps the
// chain handlers can resolve a story + tasks against.
func newChainTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	tasks := task.NewMemoryStore()
	projects := project.NewMemoryStore()
	wss := workspace.NewMemoryStore()
	return New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Projects:   projects,
			Ledger:     led,
			Stories:    stories,
			Workspaces: wss,
			Tasks:      tasks,
		},
	})
}

// seedChainAlice mints workspace + project + story + one published
// plan/work task, returning the story id and the task id.
func seedChainAlice(t *testing.T, s *Server) (string, string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	ws, err := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now); err != nil {
		t.Fatalf("addmember: %v", err)
	}
	proj, err := s.deps.Projects.Create(ctx, "u_alice", ws.ID, "alpha-1", now)
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	st, err := s.deps.Stories.Create(ctx, story.Story{
		WorkspaceID: ws.ID, ProjectID: proj.ID,
		Title: "chain wire", Category: "improvement", CreatedBy: "u_alice",
	}, now)
	if err != nil {
		t.Fatalf("story: %v", err)
	}
	plan, err := s.deps.Tasks.Enqueue(ctx, task.Task{
		WorkspaceID: ws.ID, ProjectID: proj.ID, StoryID: st.ID,
		Kind: task.KindWork, Action: task.ContractAction("plan"),
		Origin: task.OriginStoryStage, Status: task.StatusPublished, Priority: task.PriorityMedium,
	}, now)
	if err != nil {
		t.Fatalf("enqueue plan: %v", err)
	}
	return st.ID, plan.ID
}

// TestHandleChainAdvance_Wire round-trips the chain_advance envelope
// per plan §2 (story_id, next_task_id, dispatched=false, terminal,
// optional ack). MCP variant: Dispatched must always be false.
func TestHandleChainAdvance_Wire(t *testing.T) {
	t.Parallel()
	s := newChainTestServer(t)
	storyID, planID := seedChainAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	res, err := s.handleChainAdvance(ctx, newCallToolReq("chain_advance", map[string]any{
		"story_id": storyID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error envelope: %s", firstText(res))
	}
	var out client.ChainAdvanceOutput
	if err := json.Unmarshal([]byte(firstText(res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.StoryID != storyID {
		t.Errorf("story_id = %q, want %q", out.StoryID, storyID)
	}
	if out.NextTaskID != planID {
		t.Errorf("next_task_id = %q, want %q", out.NextTaskID, planID)
	}
	if out.Dispatched {
		t.Errorf("MCP variant must return dispatched=false, got true")
	}
	if out.Terminal {
		t.Errorf("terminal=true with a published plan/work task")
	}
}

// TestHandleChainStatus_Wire round-trips the snapshot envelope.
func TestHandleChainStatus_Wire(t *testing.T) {
	t.Parallel()
	s := newChainTestServer(t)
	storyID, planID := seedChainAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	res, err := s.handleChainStatus(ctx, newCallToolReq("chain_status", map[string]any{
		"story_id": storyID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error envelope: %s", firstText(res))
	}
	var out client.ChainStatusOutput
	if err := json.Unmarshal([]byte(firstText(res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.StoryID != storyID {
		t.Errorf("story_id = %q, want %q", out.StoryID, storyID)
	}
	if out.NextTaskID != planID {
		t.Errorf("next_task_id = %q, want %q", out.NextTaskID, planID)
	}
	if len(out.Phases) == 0 {
		t.Errorf("phases envelope empty")
	}
}

// TestHandleChainAdvance_RequiresStoryID verifies the missing-arg
// envelope is the wire-shape error.
func TestHandleChainAdvance_RequiresStoryID(t *testing.T) {
	t.Parallel()
	s := newChainTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleChainAdvance(ctx, newCallToolReq("chain_advance", map[string]any{}))
	if !res.IsError {
		t.Errorf("missing story_id should error, got: %s", firstText(res))
	}
}

// TestHandleChainStatus_RequiresStoryID locks the same envelope for the
// read-only verb.
func TestHandleChainStatus_RequiresStoryID(t *testing.T) {
	t.Parallel()
	s := newChainTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleChainStatus(ctx, newCallToolReq("chain_status", map[string]any{}))
	if !res.IsError {
		t.Errorf("missing story_id should error, got: %s", firstText(res))
	}
}
