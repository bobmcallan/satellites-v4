// boot_drift.go — `satellites-client` PersistentPreRunE drift check.
//
// On every boot, after credentials resolve and `resolvedClientConfig`
// caches the TOML overlay, the binary calls the remote
// `system_version` verb, compares its ldflag-stamped patch version
// against the manifest's published patch version, and exits with
// code 9 + a structured JSON stderr line when the drift exceeds the
// operator-configured tolerance.
//
// Skipped when:
//   - `config.Version == "dev"` (covers `go build` without ldflags),
//   - `cliconfig.UpdateCheckDisabled == true`,
//   - env `SATELLITES_CLIENT_SKIP_UPDATE_CHECK=1` is set,
//   - the manifest fetch / parse fails (fail-open per AC3).
//
// Implemented as a standalone function so the smoke test can drive
// it without re-entering Cobra. sty_64e69db8.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/cliconfig"
	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// driftEnvSkip is the env-var operators set to short-circuit the
// boot drift check. Mirrors the `update_check_disabled` TOML key
// without requiring a TOML edit.
const driftEnvSkip = "SATELLITES_CLIENT_SKIP_UPDATE_CHECK"

// driftExitCode is the exit code emitted when the local stamped
// version trails the remote manifest by more than the configured
// patch tolerance. Documented in docs/cli-primary-design.md §3
// alongside the typed exit-code map.
const driftExitCode = 9

// driftEvent is the JSON line written to stderr when a drift is
// detected. One line, parseable with json.Unmarshal — the
// orchestrator key-greps on `event="version_drift"`.
type driftEvent struct {
	Event     string `json:"event"`
	Local     string `json:"local"`
	Remote    string `json:"remote"`
	Tolerance int    `json:"tolerance"`
}

// driftFetcher is the function shape the drift check calls to fetch
// the remote manifest stamp. Production wires a cliremote.Client;
// the smoke test injects a stub.
type driftFetcher func(ctx context.Context) (cliremote.SystemVersionOutput, error)

// runBootDriftCheck drives the drift check. Returns an error keyed
// to cliexit.Code = driftExitCode when drift exceeds tolerance.
// Returns nil on skip and on fail-open paths.
func runBootDriftCheck(ctx context.Context, localVersion string, cfg *cliconfig.Config, fetch driftFetcher, stderr io.Writer, env func(string) string) error {
	if cfg == nil {
		return nil
	}
	if strings.TrimSpace(localVersion) == "" || localVersion == "dev" {
		return nil
	}
	if cfg.UpdateCheckDisabled {
		return nil
	}
	if v := env(driftEnvSkip); v == "1" || strings.EqualFold(v, "true") {
		return nil
	}
	if fetch == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := fetch(ctx)
	if err != nil {
		// Fail-open: never block a verb on a flaky upstream. The
		// integration test asserts clean stderr on the
		// manifest-unreachable branch.
		return nil
	}

	localPatch, localOK := parsePatchVersion(localVersion)
	remotePatch, remoteOK := parsePatchVersion(out.Version)
	if !localOK || !remoteOK {
		// Either side can't be parsed (non-semver stamps in
		// dev/test paths) — fail open rather than mis-judging.
		return nil
	}
	if remotePatch <= localPatch+cfg.UpdateTolerancePatches {
		return nil
	}

	event := driftEvent{
		Event:     "version_drift",
		Local:     localVersion,
		Remote:    out.Version,
		Tolerance: cfg.UpdateTolerancePatches,
	}
	payload, marshalErr := json.Marshal(event)
	if marshalErr != nil {
		// Marshalling a static struct shouldn't fail; keep the
		// fail-open contract anyway.
		return nil
	}
	fmt.Fprintf(stderr, "%s\n", payload)
	return cliexit.Wrap(cliexit.Code(driftExitCode), errors.New("version_drift"))
}

// parsePatchVersion parses a `MAJOR.MINOR.PATCH` (or a leading-v
// variant) and returns the patch component plus a true ok flag.
// Returns (0, false) on any form it does not recognise so the
// drift-check can fail-open.
func parsePatchVersion(raw string) (int, bool) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "v")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, false
	}
	// Strip pre-release / metadata from the patch part if present.
	patchRaw := parts[2]
	if idx := strings.IndexAny(patchRaw, "-+"); idx >= 0 {
		patchRaw = patchRaw[:idx]
	}
	if _, err := strconv.Atoi(parts[0]); err != nil {
		return 0, false
	}
	if _, err := strconv.Atoi(parts[1]); err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(patchRaw)
	if err != nil {
		return 0, false
	}
	return p, true
}

// envFunc is the indirection over os.Getenv the smoke test swaps to
// drive the SATELLITES_CLIENT_SKIP_UPDATE_CHECK branch without
// touching the process env.
func envFunc(key string) string { return os.Getenv(key) }
