package client

import (
	"context"
	"time"

	"github.com/bobmcallan/satellites/internal/config"
)

// SatellitesInfoInput is the input for SatellitesInfo. The verb takes
// no caller-supplied parameters today; the struct exists for shape
// parity with the rest of the typed surface.
type SatellitesInfoInput struct{}

// SatellitesInfoOutput mirrors the wire payload of satellites_info.
// Empty UserEmail is omitted by the caller at the wire boundary.
type SatellitesInfoOutput struct {
	Version   string    `json:"version"`
	Build     string    `json:"build"`
	Commit    string    `json:"commit"`
	UserEmail string    `json:"user_email,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

// SatellitesInfo returns the build-stamped identity of the satellites
// server plus the caller's resolved email and the server's boot time.
// Pure projection of package-level config + Caller.Email +
// Deps.StartedAt. No store dependency; cannot fail.
func (c *Client) SatellitesInfo(ctx context.Context, caller Caller, in SatellitesInfoInput) (SatellitesInfoOutput, error) {
	return SatellitesInfoOutput{
		Version:   config.Version,
		Build:     config.Build,
		Commit:    config.GitCommit,
		UserEmail: caller.Email,
		StartedAt: c.deps.StartedAt.UTC(),
	}, nil
}
