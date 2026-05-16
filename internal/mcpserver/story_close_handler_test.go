// Wire-shape coverage for handleStoryClose (sty_b97dda00 slice 1).
// The substrate-side gate logic lives in internal/client/story_close.go
// and is exercised in story_close_test.go; this test verifies the thin
// wire adapter: required-arg envelope, JSON round-trip on PASS, gap
// pass-through on FAIL.
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
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

func newStoryCloseTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Env: "dev"}
	now := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	led := ledger.NewMemoryStore()
	docs := document.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	tasks := task.NewMemoryStore()
	projects := project.NewMemoryStore()
	wss := workspace.NewMemoryStore()
	sessions := session.NewMemoryStore()
	return New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:  docs,
			Projects:   projects,
			Ledger:     led,
			Stories:    stories,
			Workspaces: wss,
			Sessions:   sessions,
			Tasks:      tasks,
		},
	})
}

// seedStoryCloseAlice mints a workspace + project + story owned by
// u_alice plus the closed work + closed review tasks and a
// verdict:pass ledger row so handleStoryClose sees a chain that
// passes every gate.
func seedStoryCloseAlice(t *testing.T, s *Server) string {
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
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Title:       "wire-shape close",
		Category:    "improvement",
		CreatedBy:   "u_alice",
	}, now)
	if err != nil {
		t.Fatalf("story: %v", err)
	}
	work, err := s.deps.Tasks.Enqueue(ctx, task.Task{
		WorkspaceID: ws.ID, ProjectID: proj.ID, StoryID: st.ID,
		Kind: task.KindWork, Action: "contract:develop",
		Origin: task.OriginStoryStage, Status: task.StatusPublished, Priority: task.PriorityMedium,
	}, now)
	if err != nil {
		t.Fatalf("enqueue work: %v", err)
	}
	if _, err := s.deps.Tasks.Close(ctx, work.ID, task.OutcomeSuccess, now.Add(time.Minute), []string{ws.ID}); err != nil {
		t.Fatalf("close work: %v", err)
	}
	review, err := s.deps.Tasks.Enqueue(ctx, task.Task{
		WorkspaceID: ws.ID, ProjectID: proj.ID, StoryID: st.ID,
		Kind: task.KindReview, Action: "contract:story_review",
		Origin: task.OriginStoryStage, Status: task.StatusPublished, Priority: task.PriorityMedium,
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("enqueue review: %v", err)
	}
	if _, err := s.deps.Tasks.Close(ctx, review.ID, task.OutcomeSuccess, now.Add(3*time.Minute), []string{ws.ID}); err != nil {
		t.Fatalf("close review: %v", err)
	}
	if _, err := s.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: ws.ID, ProjectID: proj.ID,
		StoryID: ledger.StringPtr(st.ID),
		Type:    ledger.TypeDecision,
		Tags:    []string{"kind:verdict", "task_id:" + review.ID, "verdict:pass"},
		Content: `{"rationale":"stub"}`,
		Durability: ledger.DurabilityDurable, SourceType: ledger.SourceAgent,
		Status: ledger.StatusActive, CreatedBy: "u_alice",
	}, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("verdict row: %v", err)
	}
	// AC5 — the deploy:behind gate requires a kind:release-evidence row
	// whose pushed_sha matches pprod's reported commit. SatellitesInfo
	// at test time returns config.GitCommit = "unknown"; seed a matching
	// release-evidence row so the close passes the gate.
	if _, err := s.deps.Ledger.Append(ctx, ledger.LedgerEntry{
		WorkspaceID: ws.ID, ProjectID: proj.ID,
		StoryID: ledger.StringPtr(st.ID),
		Type:    ledger.TypeEvidence,
		Tags:    []string{"kind:release-evidence", "phase:merge_to_main", "pushed_sha:unknown"},
		Content: "## release-evidence stub for wire-shape close",
		Durability: ledger.DurabilityDurable, SourceType: ledger.SourceAgent,
		Status: ledger.StatusActive, CreatedBy: "u_alice",
	}, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("release-evidence row: %v", err)
	}
	return st.ID
}

// TestHandleStoryClose_PassRoundTrip verifies the wire-shape PASS
// envelope: handler returns success, body decodes to a StoryCloseOutput
// with status:"pass" + an evidence id, story walked to done.
func TestHandleStoryClose_PassRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStoryCloseTestServer(t)
	storyID := seedStoryCloseAlice(t, s)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})

	// Pass pprod_commit matching the seeded release-evidence pushed_sha
	// so the multi-tenant deploy:behind resolver (sty_224774f0)
	// short-circuits to the override on a project that is not the
	// satellites-self project. The test cfg has SelfProjectID empty,
	// so without the override the gate would emit
	// release-evidence:no-deploy-endpoint.
	res, err := s.handleStoryClose(ctx, newCallToolReq("story_close", map[string]any{
		"story_id":     storyID,
		"pprod_commit": "unknown",
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected pass, got error: %s", firstText(res))
	}
	var out client.StoryCloseOutput
	if err := json.Unmarshal([]byte(firstText(res)), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status != "pass" {
		t.Errorf("status = %q, want pass", out.Status)
	}
	if out.EvidenceID == "" {
		t.Errorf("evidence_id missing on pass: %+v", out)
	}
	if out.StoryStatus != story.StatusDone {
		t.Errorf("story_status = %q, want done", out.StoryStatus)
	}
}

// TestHandleStoryClose_FailReviewAbsentNoMutation drives the FAIL
// envelope through the wire layer. Story_review:absent path: the
// gap list comes back without mutation.
func TestHandleStoryClose_FailReviewAbsentNoMutation(t *testing.T) {
	t.Parallel()
	s := newStoryCloseTestServer(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	ws, _ := s.deps.Workspaces.Create(ctx, "u_alice", "alpha", now)
	_ = s.deps.Workspaces.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "u_alice", now)
	proj, _ := s.deps.Projects.Create(ctx, "u_alice", ws.ID, "alpha-1", now)
	st, err := s.deps.Stories.Create(ctx, story.Story{
		WorkspaceID: ws.ID, ProjectID: proj.ID, Title: "no review", Category: "improvement",
		CreatedBy: "u_alice",
	}, now)
	if err != nil {
		t.Fatalf("story: %v", err)
	}
	ctxA := withCaller(ctx, auth.CallerIdentity{UserID: "u_alice"})

	res, err := s.handleStoryClose(ctxA, newCallToolReq("story_close", map[string]any{
		"story_id": st.ID,
	}))
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected non-error fail envelope, got error: %s", firstText(res))
	}
	var out client.StoryCloseOutput
	_ = json.Unmarshal([]byte(firstText(res)), &out)
	if out.Status != "fail" {
		t.Errorf("status = %q, want fail", out.Status)
	}
	found := false
	for _, g := range out.Gaps {
		if g.Code == "story_review:absent" {
			found = true
		}
	}
	if !found {
		t.Errorf("story_review:absent gap missing: %+v", out.Gaps)
	}
	got, _ := s.deps.Stories.GetByID(ctx, st.ID, []string{ws.ID})
	if got.Status == story.StatusDone {
		t.Errorf("story mutated on FAIL: status=%q", got.Status)
	}
}

// TestHandleStoryClose_RequiresStoryID verifies the missing-arg
// envelope is the wire-shape "story_id is required" error.
func TestHandleStoryClose_RequiresStoryID(t *testing.T) {
	t.Parallel()
	s := newStoryCloseTestServer(t)
	ctx := withCaller(context.Background(), auth.CallerIdentity{UserID: "u_alice"})
	res, _ := s.handleStoryClose(ctx, newCallToolReq("story_close", map[string]any{}))
	if !res.IsError {
		t.Errorf("missing story_id should error, got: %s", firstText(res))
	}
}
