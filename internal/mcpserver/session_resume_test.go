package mcpserver

import (
	"context"
	"encoding/json"
	"github.com/bobmcallan/satellites/internal/auth"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/client"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/session"
)

// resumeTestServer wires the minimal Server surface session_register
// needs for resume tests: a session store + a config. No docs/grants
// (the resume path runs before role grant minting).
func resumeTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg:  &config.Config{GrantsEnforced: false},
		deps: client.Deps{Sessions: session.NewMemoryStore()},
	}
}

func registerWithProject(t *testing.T, s *Server, userID, sessionID, projectID string) map[string]any {
	t.Helper()
	ctx := auth.WithCaller(context.Background(), auth.CallerIdentity{UserID: userID, Email: userID + "@example.com", Source: "apikey"})
	args := map[string]any{}
	if sessionID != "" {
		args["session_id"] = sessionID
	}
	if projectID != "" {
		args["project_id"] = projectID
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleSessionRegister(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "register: %+v", res)
	text := res.Content[0].(mcpgo.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out
}

// TestSessionRegister_Resume_FreshSessionForProject covers the happy
// path: a second session_register({project_id}) within the staleness
// window resolves to the same session row as the first
// (story_cef068fe).
func TestSessionRegister_Resume_FreshSessionForProject(t *testing.T) {
	t.Parallel()
	s := resumeTestServer(t)
	first := registerWithProject(t, s, "u1", "sess_orig", "proj_x")
	firstID, _ := first["session_id"].(string)
	require.Equal(t, "sess_orig", firstID)
	assert.Equal(t, false, first["resumed"], "first register should not report a resume")

	// Second call with project_id but NO session_id → resumes the prior.
	second := registerWithProject(t, s, "u1", "", "proj_x")
	resumedID, _ := second["session_id"].(string)
	assert.Equal(t, "sess_orig", resumedID, "resume must reuse the prior session id")
	assert.Equal(t, true, second["resumed"], "second register should report resumed=true")
	assert.Equal(t, "proj_x", second["active_project_id"])
}

// TestSessionRegister_Resume_DistinctProjectsStaySeparate ensures two
// projects under the same user don't collapse: each project resumes
// independently.
func TestSessionRegister_Resume_DistinctProjectsStaySeparate(t *testing.T) {
	t.Parallel()
	s := resumeTestServer(t)
	registerWithProject(t, s, "u1", "sess_a", "proj_a")
	registerWithProject(t, s, "u1", "sess_b", "proj_b")

	resumeA := registerWithProject(t, s, "u1", "", "proj_a")
	resumeB := registerWithProject(t, s, "u1", "", "proj_b")

	assert.Equal(t, "sess_a", resumeA["session_id"])
	assert.Equal(t, "sess_b", resumeB["session_id"])
	assert.Equal(t, true, resumeA["resumed"])
	assert.Equal(t, true, resumeB["resumed"])
}

// TestSessionRegister_Resume_StaleSessionMintsFresh proves a session
// older than SATELLITES_SESSION_STALENESS is not resumed; a new id is
// minted instead.
func TestSessionRegister_Resume_StaleSessionMintsFresh(t *testing.T) {
	t.Parallel()
	s := resumeTestServer(t)
	// Plant a stale session manually so we don't have to wait the
	// staleness window in tests.
	stale := time.Now().UTC().Add(-2 * session.StalenessDefault)
	_, err := s.deps.Sessions.Register(context.Background(), "u1", "sess_stale", session.SourceSessionStart, stale)
	require.NoError(t, err)
	_, err = s.deps.Sessions.SetActiveProject(context.Background(), "u1", "sess_stale", "proj_x", stale)
	require.NoError(t, err)

	out := registerWithProject(t, s, "u1", "", "proj_x")
	gotID, _ := out["session_id"].(string)
	require.NotEmpty(t, gotID)
	assert.NotEqual(t, "sess_stale", gotID, "stale session must not resume; a fresh id is minted")
	assert.Equal(t, false, out["resumed"], "stale resume must not flip resumed=true")
}

// TestSessionRegister_Resume_NoProjectIDMintsFresh ensures resume only
// triggers when project_id is supplied. Without project_id, every
// register without a session_id mints a fresh one.
func TestSessionRegister_Resume_NoProjectIDMintsFresh(t *testing.T) {
	t.Parallel()
	s := resumeTestServer(t)
	registerWithProject(t, s, "u1", "sess_orig", "proj_x")

	out := registerWithProject(t, s, "u1", "", "")
	gotID, _ := out["session_id"].(string)
	require.NotEmpty(t, gotID)
	assert.NotEqual(t, "sess_orig", gotID, "no project_id → no resume; fresh id minted")
	assert.Equal(t, false, out["resumed"])
}

// TestSessionRegister_Resume_ExplicitSessionIDOverridesResume proves
// that an explicit session_id always wins over the resume lookup.
func TestSessionRegister_Resume_ExplicitSessionIDOverridesResume(t *testing.T) {
	t.Parallel()
	s := resumeTestServer(t)
	registerWithProject(t, s, "u1", "sess_orig", "proj_x")

	out := registerWithProject(t, s, "u1", "sess_other", "proj_x")
	assert.Equal(t, "sess_other", out["session_id"], "explicit session_id must override resume")
	assert.Equal(t, false, out["resumed"], "explicit session_id path is not a resume")
}
