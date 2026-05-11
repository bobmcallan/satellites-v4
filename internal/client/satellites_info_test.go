package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/config"
)

// TestSatellitesInfo_ProjectsBuildStampAndCaller exercises the typed
// surface end-to-end: build-stamped vars, caller email, deps started_at.
// The verb has no store dependency; a nil-deps Client is sufficient.
func TestSatellitesInfo_ProjectsBuildStampAndCaller(t *testing.T) {
	bootTime := time.Date(2026, 5, 11, 10, 0, 0, 0, time.UTC)
	c := New(Deps{StartedAt: bootTime})

	out, err := c.SatellitesInfo(context.Background(),
		Caller{UserID: "u_alice", Email: "google:alice@example.com"},
		SatellitesInfoInput{})
	require.NoError(t, err)

	assert.Equal(t, config.Version, out.Version)
	assert.Equal(t, config.Build, out.Build)
	assert.Equal(t, config.GitCommit, out.Commit)
	assert.Equal(t, "google:alice@example.com", out.UserEmail)
	assert.True(t, out.StartedAt.Equal(bootTime), "StartedAt = %v, want %v", out.StartedAt, bootTime)
}

// TestSatellitesInfo_StartedAtCoercedToUTC locks the contract that the
// output always carries UTC regardless of how the wire layer stamped
// Deps.StartedAt. Sty_df1cb227 review-criteria.
func TestSatellitesInfo_StartedAtCoercedToUTC(t *testing.T) {
	loc, err := time.LoadLocation("Australia/Sydney")
	require.NoError(t, err)
	localBoot := time.Date(2026, 5, 11, 20, 0, 0, 0, loc)
	c := New(Deps{StartedAt: localBoot})

	out, err := c.SatellitesInfo(context.Background(), Caller{}, SatellitesInfoInput{})
	require.NoError(t, err)
	assert.Equal(t, "UTC", out.StartedAt.Location().String())
	assert.True(t, out.StartedAt.Equal(localBoot), "UTC instant unchanged after coercion")
}

// TestSatellitesInfo_EmptyEmail tolerates a Caller without an email
// stamp — the v3-parity wire payload omits the field rather than
// returning an error.
func TestSatellitesInfo_EmptyEmail(t *testing.T) {
	c := New(Deps{StartedAt: time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)})
	out, err := c.SatellitesInfo(context.Background(), Caller{UserID: "u_bob"}, SatellitesInfoInput{})
	require.NoError(t, err)
	assert.Equal(t, "", out.UserEmail)
}
