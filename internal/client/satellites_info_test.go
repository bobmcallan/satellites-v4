package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
)

// TestSatellitesInfo_ServerBlock exercises the typed surface end-to-end:
// build-stamped vars + boot time land in the server block; caller block
// carries the identity fields the wire layer threaded in. Sty_0be97c3e
// reshape.
func TestSatellitesInfo_ServerBlock(t *testing.T) {
	bootTime := time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC)
	c := New(Deps{StartedAt: bootTime})
	out, err := c.SatellitesInfo(context.Background(),
		Caller{UserID: "u_alice", Email: "google:alice@example.com"},
		SatellitesInfoInput{AuthKind: "oauth:google"})
	require.NoError(t, err)

	assert.Equal(t, config.Version, out.Server.Version)
	assert.Equal(t, config.Build, out.Server.Build)
	assert.Equal(t, config.GitCommit, out.Server.Commit)
	assert.True(t, out.Server.StartedAt.Equal(bootTime), "StartedAt = %v, want %v", out.Server.StartedAt, bootTime)
	assert.Equal(t, "u_alice", out.Caller.UserID)
	assert.Equal(t, "google:alice@example.com", out.Caller.Email)
	assert.Equal(t, "oauth:google", out.Caller.AuthKind)
	assert.Empty(t, out.Caller.ProjectID)
	assert.Empty(t, out.RecentActivity.LedgerRowsLastN)
}

// TestSatellitesInfo_StartedAtCoercedToUTC locks the contract that the
// output always carries UTC regardless of how the wire layer stamped
// Deps.StartedAt.
func TestSatellitesInfo_StartedAtCoercedToUTC(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Sydney")
	require.NoError(t, err)
	localBoot := time.Date(2026, 5, 11, 20, 0, 0, 0, loc)
	c := New(Deps{StartedAt: localBoot})

	out, err := c.SatellitesInfo(context.Background(), Caller{}, SatellitesInfoInput{})
	require.NoError(t, err)
	assert.Equal(t, "UTC", out.Server.StartedAt.Location().String())
	assert.True(t, out.Server.StartedAt.Equal(localBoot), "UTC instant unchanged after coercion")
}

// TestSatellitesInfo_EmptyCaller tolerates a Caller without identity —
// the verb still returns the server block.
func TestSatellitesInfo_EmptyCaller(t *testing.T) {
	c := New(Deps{StartedAt: time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)})
	out, err := c.SatellitesInfo(context.Background(), Caller{}, SatellitesInfoInput{})
	require.NoError(t, err)
	assert.Equal(t, config.Version, out.Server.Version)
	assert.Empty(t, out.Caller.Email)
	assert.Empty(t, out.Caller.ProjectID)
}

// TestSatellitesInfo_BoundSessionResolvesProject regression-tests the
// session→project lookup. When the caller's session row has an active
// project, the output's caller block carries project_id + workspace_id
// and recent_activity surfaces the bound project's ledger rows.
// Sty_0be97c3e.
func TestSatellitesInfo_BoundSessionResolvesProject(t *testing.T) {
	bootTime := time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC)
	ledgerStore := ledger.NewMemoryStore()
	sessions := session.NewMemoryStore()
	projects := project.NewMemoryStore()

	const (
		userID      = "u_google:bob@example.com"
		sessionID   = "sess_abc"
		workspaceID = "wksp_test"
	)
	now := time.Now().UTC()

	// Project row with workspace pointer (Create stamps StatusActive +
	// timestamps; we then patch the id/owner to deterministic values).
	createdProj, err := projects.Create(context.Background(), userID, workspaceID, "test", now)
	require.NoError(t, err)
	// Session row with active project (use the created project id).
	_, err = sessions.Register(context.Background(), userID, sessionID, session.SourceSessionStart, now)
	require.NoError(t, err)
	_, err = sessions.SetActiveProject(context.Background(), userID, sessionID, createdProj.ID, now)
	require.NoError(t, err)
	// One ledger row in the bound project so recent_activity is non-empty.
	storyID := "sty_test"
	_, err = ledgerStore.Append(context.Background(), ledger.LedgerEntry{
		WorkspaceID: workspaceID, ProjectID: createdProj.ID, StoryID: &storyID,
		Type: ledger.TypeEvidence, Content: "test", Durability: ledger.DurabilityDurable,
		SourceType: ledger.SourceUser, Status: ledger.StatusActive, CreatedBy: userID,
	}, now)
	require.NoError(t, err)

	c := New(Deps{
		StartedAt: bootTime, Ledger: ledgerStore, Sessions: sessions, Projects: projects,
	})
	out, err := c.SatellitesInfo(context.Background(),
		Caller{UserID: userID, Email: "google:bob@example.com", Memberships: []string{workspaceID}},
		SatellitesInfoInput{SessionID: sessionID, AuthKind: "oauth:google", RecentActivityLimit: 5})
	require.NoError(t, err)

	assert.Equal(t, createdProj.ID, out.Caller.ProjectID)
	assert.Equal(t, workspaceID, out.Caller.WorkspaceID)
	assert.Equal(t, "oauth:google", out.Caller.AuthKind)
	assert.Len(t, out.RecentActivity.LedgerRowsLastN, 1)
	assert.Equal(t, storyID, out.RecentActivity.LedgerRowsLastN[0].StoryID)
}
