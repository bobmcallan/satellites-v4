package mcpserver

import (
	"context"
	"github.com/bobmcallan/satellites/internal/auth"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
	"github.com/bobmcallan/satellites/internal/workspace"
)

// contractFixture is the shared mcpserver test harness — wired memory
// stores + one workspace + one project + the five default contract
// docs + one parent story. Survives sty_c6d76a5b checkpoint 13 (which
// retired the legacy CI lifecycle handlers) so agent_compose,
// task_submit, and other tests that need a populated server can
// keep using it.
type contractFixture struct {
	t         *testing.T
	ctx       context.Context
	server    *Server
	caller    auth.CallerIdentity
	projectID string
	wsID      string
	storyID   string
	now       time.Time
}

func newContractFixture(t *testing.T) *contractFixture {
	t.Helper()
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	cfg := &config.Config{Env: "dev"}
	wsStore := workspace.NewMemoryStore()
	docStore := document.NewMemoryStore()
	ledStore := ledger.NewMemoryStore()
	storyStore := story.NewMemoryStore(ledStore)
	projStore := project.NewMemoryStore()

	ws, err := wsStore.Create(ctx, "user_alice", "alpha", now)
	if err != nil {
		t.Fatalf("ws create: %v", err)
	}
	_ = wsStore.AddMember(ctx, ws.ID, "user_alice", workspace.RoleAdmin, "system", now)

	proj, err := projStore.Create(ctx, "user_alice", ws.ID, "p1", now)
	if err != nil {
		t.Fatalf("project create: %v", err)
	}

	for _, name := range []string{"plan", "develop", "push", "merge_to_main", "story_review"} {
		if _, err := docStore.Create(ctx, document.Document{
			Type:   document.TypeContract,
			Scope:  document.ScopeSystem,
			Name:   name,
			Body:   "body-" + name,
			Status: document.StatusActive,
		}, now); err != nil {
			t.Fatalf("seed contract %q: %v", name, err)
		}
	}

	parent, err := storyStore.Create(ctx, story.Story{
		WorkspaceID: ws.ID,
		ProjectID:   proj.ID,
		Title:       "parent",
	}, now)
	if err != nil {
		t.Fatalf("parent story: %v", err)
	}

	server := New(cfg, satarbor.New("info"), now, Deps{
		Client: client.Deps{
			Documents:        docStore,
			Projects:         projStore,
			DefaultProjectID: proj.ID,
			Ledger:           ledStore,
			Stories:          storyStore,
			Workspaces:       wsStore,
		},
	})

	return &contractFixture{
		t:         t,
		ctx:       ctx,
		server:    server,
		caller:    auth.CallerIdentity{UserID: "user_alice", Source: "session"},
		projectID: proj.ID,
		wsID:      ws.ID,
		storyID:   parent.ID,
		now:       now,
	}
}

func (f *contractFixture) callerCtx() context.Context {
	return withCaller(f.ctx, f.caller)
}

// orchestratorFixture extends the contract fixture with a task store
// and the system workflow + agent docs the task_submit / agent
// capability tests rely on.
type orchestratorFixture struct {
	*contractFixture
	taskStore task.Store
}

func newOrchestratorFixture(t *testing.T) *orchestratorFixture {
	t.Helper()
	f := newContractFixture(t)

	taskStore := task.NewMemoryStore()
	f.server.deps.Tasks = taskStore

	systemSrc := document.Document{
		Type:   document.TypeWorkflow,
		Scope:  document.ScopeSystem,
		Name:   "default",
		Body:   "system workflow",
		Status: document.StatusActive,
		Structured: []byte(`{"required_slots":[
			{"contract_name":"plan","required":true,"min_count":1,"max_count":1},
			{"contract_name":"develop","required":true,"min_count":1,"max_count":10},
			{"contract_name":"story_review","required":true,"min_count":1,"max_count":1}
		]}`),
	}
	if _, err := f.server.deps.Documents.Create(context.Background(), systemSrc, f.now); err != nil {
		t.Fatalf("seed system workflow: %v", err)
	}

	type seedAgent struct {
		Name     string
		Delivers []string
		Reviews  []string
	}
	for _, sa := range []seedAgent{
		{Name: "developer_agent", Delivers: []string{
			task.ContractAction("plan"), task.ContractAction("develop"),
		}},
		{Name: "releaser_agent", Delivers: []string{
			task.ContractAction("push"), task.ContractAction("merge_to_main"),
		}},
		{Name: "story_reviewer", Reviews: []string{
			task.ContractAction("plan"),
			task.ContractAction("push"),
			task.ContractAction("merge_to_main"),
			task.ContractAction("story_review"),
		}},
		{Name: "development_reviewer", Reviews: []string{
			task.ContractAction("develop"),
		}},
	} {
		settings, _ := document.MarshalAgentSettings(document.AgentSettings{
			PermissionPatterns: []string{"Read:**"},
			Delivers:           sa.Delivers,
			Reviews:            sa.Reviews,
		})
		if _, err := f.server.deps.Documents.Create(context.Background(), document.Document{
			Type:       document.TypeAgent,
			Scope:      document.ScopeSystem,
			Name:       sa.Name,
			Body:       "agent body for " + sa.Name,
			Status:     document.StatusActive,
			Structured: settings,
		}, f.now); err != nil {
			t.Fatalf("seed agent %q: %v", sa.Name, err)
		}
	}

	return &orchestratorFixture{contractFixture: f, taskStore: taskStore}
}
