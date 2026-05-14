package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// substrateAuditFixture wires the memory-backed substrate the
// SubstrateAudit table cases need: workspace + a seeded
// substrate_auditor agent + task + story + ledger stores.
type substrateAuditFixture struct {
	t          *testing.T
	now        time.Time
	c          *Client
	wsID       string
	projectID  string
	caller     Caller
	auditorID  string
	taskStore  task.Store
	storyStore story.Store
	ledStore   ledger.Store
}

// newSubstrateAuditFixture seeds the substrate with the
// substrate_auditor agent declaring delivers: contract:substrate_audit
// so the TaskAdd capability check passes.
func newSubstrateAuditFixture(t *testing.T) *substrateAuditFixture {
	t.Helper()
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	taskStore := task.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "u_dev", "alpha", now)
	require.NoError(t, err)
	require.NoError(t, wsStore.AddMember(ctx, ws.ID, "u_dev", workspace.RoleAdmin, "system", now))

	// Seed substrate_auditor agent at system scope with the canonical
	// delivers list so TaskAdd's capability check passes.
	auditorDoc, err := docStore.Create(ctx, document.Document{
		Type:       document.TypeAgent,
		Scope:      document.ScopeSystem,
		Name:       "substrate_auditor",
		Body:       "stub substrate_auditor body",
		Status:     document.StatusActive,
		Structured: []byte(`{"delivers":["contract:substrate_audit"],"reviews":[],"permission_patterns":["Read:**"]}`),
	}, now)
	require.NoError(t, err)

	c := New(Deps{
		Documents:        docStore,
		Workspaces:       wsStore,
		Ledger:           ledStore,
		Stories:          storyStore,
		Tasks:            taskStore,
		DefaultProjectID: "proj_test",
	})

	return &substrateAuditFixture{
		t:          t,
		now:        now,
		c:          c,
		wsID:       ws.ID,
		projectID:  "proj_test",
		caller:     Caller{UserID: "u_dev", Memberships: []string{ws.ID}},
		auditorID:  auditorDoc.ID,
		taskStore:  taskStore,
		storyStore: storyStore,
		ledStore:   ledStore,
	}
}

// TestSubstrateAudit_MintsTaskWithCorrectShape asserts the verb
// mints a kind=work action=contract:substrate_audit task against the
// seeded substrate_auditor agent, returns the new task_id, story_id,
// the auditor's resolved doc id, and the scope label.
func TestSubstrateAudit_MintsTaskWithCorrectShape(t *testing.T) {
	f := newSubstrateAuditFixture(t)
	out, err := f.c.SubstrateAudit(context.Background(), f.caller, SubstrateAuditInput{
		ProjectID:   f.projectID,
		Memberships: f.caller.Memberships,
		Resolve: TaskAddResolveDeps{
			DefaultProjectID: f.projectID,
			ResolveProjectWorkspaceID: func(ctx context.Context, projectID string) string {
				return f.wsID
			},
		},
	})
	require.NoError(t, err)

	assert.NotEmpty(t, out.TaskID)
	assert.NotEmpty(t, out.StoryID)
	assert.Equal(t, f.auditorID, out.AgentID)
	assert.Equal(t, "project:"+f.projectID, out.Scope)

	tk, err := f.taskStore.GetByID(context.Background(), out.TaskID, f.caller.Memberships)
	require.NoError(t, err)
	assert.Equal(t, task.KindWork, tk.Kind)
	assert.Equal(t, substrateAuditAction, tk.Action)
	assert.Equal(t, f.auditorID, tk.AgentID)
	assert.Equal(t, task.StatusPublished, tk.Status)
}

// TestSubstrateAudit_ScopeFallback asserts the scope label degrades
// from project → workspace → system as inputs drop off.
func TestSubstrateAudit_ScopeFallback(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		wsID      string
		want      string
	}{
		{name: "project_wins", projectID: "proj_x", wsID: "wksp_y", want: "project:proj_x"},
		{name: "workspace_wins", projectID: "", wsID: "wksp_y", want: "workspace:wksp_y"},
		{name: "system_default", projectID: "", wsID: "", want: "system"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveSubstrateAuditScope(document.ScopeSystem, tc.wsID, tc.projectID))
		})
	}
}

// TestSubstrateAudit_AgentNotSeededFails asserts the verb returns a
// surfaceable error when the substrate_auditor agent is missing.
func TestSubstrateAudit_AgentNotSeededFails(t *testing.T) {
	ctx := context.Background()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	taskStore := task.NewMemoryStore()
	c := New(Deps{
		Documents:        docStore,
		Ledger:           ledStore,
		Stories:          storyStore,
		Tasks:            taskStore,
		DefaultProjectID: "proj_test",
	})

	_, err := c.SubstrateAudit(ctx, Caller{}, SubstrateAuditInput{
		ProjectID: "proj_test",
		Resolve: TaskAddResolveDeps{
			DefaultProjectID: "proj_test",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "substrate_auditor")
}
