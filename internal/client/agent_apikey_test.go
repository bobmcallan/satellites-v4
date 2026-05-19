package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// agentAPIKeyFixture wires every store AgentAPIKeyCreate's task-
// scoped path touches: a workspace, a project, an agent doc with
// `role:` populated, a story, and a task that names the agent.
// Returns the Client + caller + the freshly-minted task id so the
// test can pass it as in.TaskID.
type agentAPIKeyFixture struct {
	t       *testing.T
	now     time.Time
	c       *Client
	caller  Caller
	wsID    string
	projID  string
	agentID string
	storyID string
	taskID  string
}

func newAgentAPIKeyFixture(t *testing.T, role string) *agentAPIKeyFixture {
	t.Helper()
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	taskStore := task.NewMemoryStore()
	projStore := project.NewMemoryStore()
	keyStore := auth.NewMemoryAgentAPIKeyStore()

	ws, err := wsStore.Create(ctx, "u_alice", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(ctx, ws.ID, "u_alice", workspace.RoleAdmin, "system", now))

	proj, err := projStore.Create(ctx, "u_alice", ws.ID, "test", now)
	require.NoError(t, err)

	// Synthesise structured payload carrying `role:`. The seed loader
	// merges frontmatter into the agent's structured JSON; here we
	// emit the same shape directly.
	structured, err := json.Marshal(map[string]any{"role": role})
	require.NoError(t, err)
	devDoc, err := docStore.Create(ctx, document.Document{
		Type:        document.TypeAgent,
		Scope:       document.ScopeWorkspace,
		WorkspaceID: ws.ID,
		Name:        "test_agent",
		Body:        "agent body",
		Status:      document.StatusActive,
		Structured:  structured,
	}, now)
	require.NoError(t, err)

	st, err := storyStore.Create(ctx, story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Title:       "test story",
	}, now)
	require.NoError(t, err)

	tsk, err := taskStore.Enqueue(ctx, task.Task{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		StoryID:     st.ID,
		AgentID:     devDoc.ID,
		Description: "test task",
		Status:      task.StatusEnqueued,
		Priority:    task.PriorityMedium,
		Kind:        task.KindWork,
		Origin:      task.OriginStoryStage,
	}, now)
	require.NoError(t, err)

	c := New(Deps{
		Documents:  docStore,
		Stories:    storyStore,
		Ledger:     ledStore,
		Tasks:      taskStore,
		Workspaces: wsStore,
		Projects:   projStore,
		APIKeys:    keyStore,
		StartedAt:  now,
	})

	return &agentAPIKeyFixture{
		t:      t,
		now:    now,
		c:      c,
		caller: Caller{UserID: "u_alice", Email: "alice@example.com", Memberships: []string{ws.ID}},
		wsID:   ws.ID,
		projID: proj.ID,
		agentID: devDoc.ID,
		storyID: st.ID,
		taskID: tsk.ID,
	}
}

// TestAgentAPIKeyCreate_TaskScopedAppliesRoleDefaults asserts a
// task-scoped mint with no explicit allowed_verbs derives the role
// default and clamps ExpiresAt to now+DefaultTaskScopedKeyTTL.
func TestAgentAPIKeyCreate_TaskScopedAppliesRoleDefaults(t *testing.T) {
	t.Parallel()
	f := newAgentAPIKeyFixture(t, auth.RoleExecution)
	out, err := f.c.AgentAPIKeyCreate(context.Background(), f.caller, AgentAPIKeyCreateInput{
		Name:      "run:" + f.taskID,
		ProjectID: f.projID,
		TaskID:    f.taskID,
		Now:       f.now,
	})
	if err != nil {
		t.Fatalf("AgentAPIKeyCreate: %v", err)
	}
	if out.TaskID != f.taskID {
		t.Errorf("out.TaskID = %q, want %q", out.TaskID, f.taskID)
	}
	if len(out.AllowedVerbs) == 0 {
		t.Fatalf("AllowedVerbs empty — role defaults missing")
	}
	// AllowedVerbs must be a subset of the execution role's defaults.
	for _, v := range out.AllowedVerbs {
		if !auth.IsAllowedForRole(auth.RoleExecution, v) {
			t.Errorf("AllowedVerbs[%q] not in execution defaults", v)
		}
	}
	if out.ExpiresAt == nil {
		t.Fatal("ExpiresAt nil — task-scoped key must default to now+6h")
	}
	wantTTL := f.now.Add(DefaultTaskScopedKeyTTL)
	if !out.ExpiresAt.Equal(wantTTL) {
		t.Errorf("ExpiresAt = %v, want %v (now+%s)", out.ExpiresAt, wantTTL, DefaultTaskScopedKeyTTL)
	}
}

// TestAgentAPIKeyCreate_TaskScopedShrinkAllowed asserts an explicit
// subset of the role default is accepted at mint time.
func TestAgentAPIKeyCreate_TaskScopedShrinkAllowed(t *testing.T) {
	t.Parallel()
	f := newAgentAPIKeyFixture(t, auth.RoleExecution)
	subset := []string{"task_update", "ledger_append"}
	out, err := f.c.AgentAPIKeyCreate(context.Background(), f.caller, AgentAPIKeyCreateInput{
		Name:         "shrunk",
		ProjectID:    f.projID,
		TaskID:       f.taskID,
		AllowedVerbs: subset,
		Now:          f.now,
	})
	if err != nil {
		t.Fatalf("AgentAPIKeyCreate: %v", err)
	}
	if len(out.AllowedVerbs) != 2 {
		t.Errorf("AllowedVerbs = %v, want 2 entries", out.AllowedVerbs)
	}
}

// TestAgentAPIKeyCreate_TaskScopedRejectSuperset asserts an explicit
// superset of the role default is rejected with
// allowed_verbs_not_subset (AC6 / pr_no_unrequested_compat).
func TestAgentAPIKeyCreate_TaskScopedRejectSuperset(t *testing.T) {
	t.Parallel()
	f := newAgentAPIKeyFixture(t, auth.RoleExecution)
	superset := []string{"task_update", "story_add"}
	_, err := f.c.AgentAPIKeyCreate(context.Background(), f.caller, AgentAPIKeyCreateInput{
		Name:         "escalation",
		ProjectID:    f.projID,
		TaskID:       f.taskID,
		AllowedVerbs: superset,
		Now:          f.now,
	})
	if err == nil {
		t.Fatal("expected allowed_verbs_not_subset, got nil")
	}
	var envErr *AgentAPIKeyError
	if !errors.As(err, &envErr) {
		t.Fatalf("err = %v (%T), want *AgentAPIKeyError", err, err)
	}
	if envErr.Code != "allowed_verbs_not_subset" {
		t.Errorf("envErr.Code = %q, want allowed_verbs_not_subset", envErr.Code)
	}
}

// TestAgentAPIKeyCreate_UnknownTaskRejected asserts task_not_found
// surfaces when the supplied task_id is absent.
func TestAgentAPIKeyCreate_UnknownTaskRejected(t *testing.T) {
	t.Parallel()
	f := newAgentAPIKeyFixture(t, auth.RoleExecution)
	_, err := f.c.AgentAPIKeyCreate(context.Background(), f.caller, AgentAPIKeyCreateInput{
		Name:      "ghost",
		ProjectID: f.projID,
		TaskID:    "tsk_ghost",
		Now:       f.now,
	})
	if err == nil {
		t.Fatal("expected task_not_found, got nil")
	}
	var envErr *AgentAPIKeyError
	if !errors.As(err, &envErr) || envErr.Code != "task_not_found" {
		t.Errorf("err = %v, want task_not_found envelope", err)
	}
}

// TestAgentAPIKeyCreate_ProjectScopedUnchanged asserts the legacy
// project-scoped path still works: TaskID is empty, AllowedVerbs is
// nil, ExpiresAt is nil → no clamping.
func TestAgentAPIKeyCreate_ProjectScopedUnchanged(t *testing.T) {
	t.Parallel()
	f := newAgentAPIKeyFixture(t, auth.RoleExecution)
	out, err := f.c.AgentAPIKeyCreate(context.Background(), f.caller, AgentAPIKeyCreateInput{
		Name:      "legacy",
		ProjectID: f.projID,
		Now:       f.now,
	})
	if err != nil {
		t.Fatalf("AgentAPIKeyCreate: %v", err)
	}
	if out.TaskID != "" {
		t.Errorf("out.TaskID = %q, want \"\"", out.TaskID)
	}
	if out.AllowedVerbs != nil {
		t.Errorf("out.AllowedVerbs = %v, want nil", out.AllowedVerbs)
	}
	if out.ExpiresAt != nil {
		t.Errorf("out.ExpiresAt = %v, want nil (project-scoped key has no default TTL)", out.ExpiresAt)
	}
}
