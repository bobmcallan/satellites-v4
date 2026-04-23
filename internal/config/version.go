// Package config exposes the build-stamped version metadata for satellites-v4
// binaries. The three exported vars are overridable via `-ldflags -X` at build
// time; their dev defaults keep a plain `go build ./...` runnable without any
// extra flags.
//
// The preferred stamp pattern (see v3 reference artifact ldg_4754ee2e) is:
//
//	Version    — parsed from the appropriate section of .version
//	Build      — generated at build time via `date -u +"%Y-%m-%d-%H-%M-%S"`
//	GitCommit  — `git rev-parse --short HEAD` at build time
//
// Both cmd/satellites and cmd/satellites-agent import this package; each binary
// is stamped independently by script/build.sh using its own .version section.
package config

import "fmt"

// Version is the semantic version string. Set at link time via:
//
//	-X 'github.com/bobmcallan/satellites/internal/config.Version=...'
var Version = "dev"

// Build is the UTC build timestamp (format: YYYY-MM-DD-HH-MM-SS).
var Build = "unknown"

// GitCommit is the short commit SHA of the source tree at build time.
var GitCommit = "unknown"

// GetFullVersion returns a human-readable boot-line fragment combining the
// three stamped fields: `<version> (build: <build>, commit: <commit>)`.
func GetFullVersion() string {
	return fmt.Sprintf("%s (build: %s, commit: %s)", Version, Build, GitCommit)
}
