package mcpserver

import (
	"context"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/rolegrant"
)

// fakeNext returns a "ok" text result; the middleware is expected to
// either (a) invoke it untouched for pass-through / bootstrap paths, or
// (b) short-circuit with a grant_required error.
var fakeNext = func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return mcpgo.NewToolResultText("ok"), nil
}

func TestGrantMiddleware_PassThroughWhenDisabled(t *testing.T) {
	t.Parallel()
	s := &Server{
		cfg:    &config.Config{GrantsEnforced: false},
		docs:   document.NewMemoryStore(),
		grants: rolegrant.NewMemoryStore(document.NewMemoryStore()),
	}
	handler := s.grantMiddleware()(fakeNext)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "document_get"
	res, err := handler(context.Background(), req)
	require.NoError(t, err)
	assert.False(t, res.IsError, "middleware should pass through when GrantsEnforced=false")
}

func TestGrantMiddleware_BootstrapAllowlistAlwaysPasses(t *testing.T) {
	t.Parallel()
	s := &Server{
		cfg:    &config.Config{GrantsEnforced: true},
		docs:   document.NewMemoryStore(),
		grants: rolegrant.NewMemoryStore(document.NewMemoryStore()),
	}
	handler := s.grantMiddleware()(fakeNext)
	for bootstrap := range bootstrapVerbs {
		req := mcpgo.CallToolRequest{}
		req.Params.Name = bootstrap
		res, err := handler(context.Background(), req)
		require.NoError(t, err)
		assert.False(t, res.IsError, "bootstrap verb %q should pass through even under enforcement", bootstrap)
	}
}

func TestGrantMiddleware_RejectsWhenNoGrantAndEnforcementOn(t *testing.T) {
	t.Parallel()
	s := &Server{
		cfg:    &config.Config{GrantsEnforced: true},
		docs:   document.NewMemoryStore(),
		grants: rolegrant.NewMemoryStore(document.NewMemoryStore()),
	}
	handler := s.grantMiddleware()(fakeNext)
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "document_get"
	// authenticated caller, but no grant row.
	ctx := context.WithValue(context.Background(), userKey, CallerIdentity{
		Email:  "agent@example.com",
		UserID: "user_agent",
		Source: "apikey",
	})
	res, err := handler(ctx, req)
	require.NoError(t, err)
	assert.True(t, res.IsError, "middleware must reject when no active grant covers the verb")
}

func TestGrantMiddleware_AcceptsWhenGrantCoversVerb(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	grants := rolegrant.NewMemoryStore(docs)
	s := &Server{
		cfg:    &config.Config{GrantsEnforced: true},
		docs:   docs,
		grants: grants,
		ledger: ledger.NewMemoryStore(),
	}
	ctx := context.Background()
	now := time.Now().UTC()

	role, err := docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		Type:        document.TypeRole,
		Name:        "role_reader",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"allowed_mcp_verbs":["document_get"]}`),
	}, now)
	require.NoError(t, err)
	agent, err := docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		Type:        document.TypeAgent,
		Name:        "agent_reader",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"permitted_roles":["` + role.ID + `"],"tool_ceiling":["document_*"]}`),
	}, now)
	require.NoError(t, err)
	_, err = grants.Create(ctx, rolegrant.RoleGrant{
		WorkspaceID: "wksp_a",
		RoleID:      role.ID,
		AgentID:     agent.ID,
		GranteeKind: rolegrant.GranteeSession,
		GranteeID:   "user_agent",
		Status:      rolegrant.StatusActive,
	}, now)
	require.NoError(t, err)

	handler := s.grantMiddleware()(fakeNext)
	authedCtx := context.WithValue(ctx, userKey, CallerIdentity{
		Email:  "agent@example.com",
		UserID: "user_agent",
		Source: "apikey",
	})

	// Allowed verb — grant covers document_get via the document_* ceiling intersected with document_get allowed list.
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "document_get"
	res, err := handler(authedCtx, req)
	require.NoError(t, err)
	assert.False(t, res.IsError, "grant should cover document_get; got error result: %+v", res)

	// Disallowed verb — grant does not cover story_create.
	req2 := mcpgo.CallToolRequest{}
	req2.Params.Name = "story_create"
	res2, err := handler(authedCtx, req2)
	require.NoError(t, err)
	assert.True(t, res2.IsError, "grant should NOT cover story_create; got non-error result")
}

func TestGrantMiddleware_OnlyActiveGrantsCount(t *testing.T) {
	t.Parallel()
	docs := document.NewMemoryStore()
	grants := rolegrant.NewMemoryStore(docs)
	s := &Server{
		cfg:    &config.Config{GrantsEnforced: true},
		docs:   docs,
		grants: grants,
		ledger: ledger.NewMemoryStore(),
	}
	ctx := context.Background()
	now := time.Now().UTC()

	role, err := docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		Type:        document.TypeRole,
		Name:        "role_reader",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"allowed_mcp_verbs":["document_get"]}`),
	}, now)
	require.NoError(t, err)
	agent, err := docs.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		Type:        document.TypeAgent,
		Name:        "agent_reader",
		Scope:       document.ScopeSystem,
		Status:      document.StatusActive,
		Structured:  []byte(`{"permitted_roles":["` + role.ID + `"],"tool_ceiling":["*"]}`),
	}, now)
	require.NoError(t, err)
	g, err := grants.Create(ctx, rolegrant.RoleGrant{
		WorkspaceID: "wksp_a",
		RoleID:      role.ID,
		AgentID:     agent.ID,
		GranteeKind: rolegrant.GranteeSession,
		GranteeID:   "user_agent",
	}, now)
	require.NoError(t, err)
	_, err = grants.Release(ctx, g.ID, "session_end", now.Add(time.Minute), nil)
	require.NoError(t, err)

	handler := s.grantMiddleware()(fakeNext)
	authedCtx := context.WithValue(ctx, userKey, CallerIdentity{
		Email:  "agent@example.com",
		UserID: "user_agent",
		Source: "apikey",
	})
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "document_get"
	res, err := handler(authedCtx, req)
	require.NoError(t, err)
	assert.True(t, res.IsError, "released grants should not satisfy the middleware")
}
