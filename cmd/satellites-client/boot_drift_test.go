package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/internal/cliconfig"
	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/cliremote"
)

// noEnv returns "" for any key — used to short-circuit the
// env-skip branch when the test wants the drift logic to evaluate.
func noEnv(string) string { return "" }

// staticFetch returns the supplied output / error pair as the
// drift-fetcher stub.
func staticFetch(out cliremote.SystemVersionOutput, err error) driftFetcher {
	return func(ctx context.Context) (cliremote.SystemVersionOutput, error) {
		return out, err
	}
}

// TestBootDrift_BelowToleranceExitsNine fires when the remote
// manifest's patch version is more than UpdateTolerancePatches
// ahead of the local stamp. Exits driftExitCode (9) and writes a
// single parseable JSON line to stderr.
func TestBootDrift_BelowToleranceExitsNine(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &cliconfig.Config{UpdateTolerancePatches: 0}
	fetch := staticFetch(cliremote.SystemVersionOutput{Version: "0.0.269"}, nil)

	err := runBootDriftCheck(context.Background(), "0.0.1", cfg, fetch, &stderr, noEnv)
	if err == nil {
		t.Fatal("expected drift error, got nil")
	}
	var typed *cliexit.Error
	if !errors.As(err, &typed) {
		t.Fatalf("expected *cliexit.Error, got %T (%v)", err, err)
	}
	if typed.Code != driftExitCode {
		t.Fatalf("exit code = %d, want %d", typed.Code, driftExitCode)
	}

	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 stderr line, got %d: %q", len(lines), stderr.String())
	}
	var ev driftEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("stderr line not parseable JSON: %v: %q", err, lines[0])
	}
	if ev.Event != "version_drift" {
		t.Errorf("event = %q, want version_drift", ev.Event)
	}
	if ev.Local != "0.0.1" || ev.Remote != "0.0.269" || ev.Tolerance != 0 {
		t.Errorf("event payload = %+v", ev)
	}
}

// TestBootDrift_WithinToleranceNoExit covers the case where the
// drift is within UpdateTolerancePatches — the binary keeps
// running.
func TestBootDrift_WithinToleranceNoExit(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &cliconfig.Config{UpdateTolerancePatches: 5}
	fetch := staticFetch(cliremote.SystemVersionOutput{Version: "0.0.4"}, nil)

	err := runBootDriftCheck(context.Background(), "0.0.1", cfg, fetch, &stderr, noEnv)
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected clean stderr, got %q", stderr.String())
	}
}

// TestBootDrift_ManifestUnreachableNoExit fails open when the
// fetcher returns an error (server unreachable, malformed manifest
// upstream). The integration test asserts this branch
// end-to-end against the real testcontainers harness.
func TestBootDrift_ManifestUnreachableNoExit(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &cliconfig.Config{UpdateTolerancePatches: 0}
	fetch := staticFetch(cliremote.SystemVersionOutput{}, errors.New("manifest fetch boom"))

	err := runBootDriftCheck(context.Background(), "0.0.1", cfg, fetch, &stderr, noEnv)
	if err != nil {
		t.Fatalf("expected nil on network error, got %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected clean stderr on fail-open, got %q", stderr.String())
	}
}

// TestBootDrift_DevVersionSkipped ensures `go build` (no ldflag
// stamp) leaves the binary at version=dev and the check never fires.
func TestBootDrift_DevVersionSkipped(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &cliconfig.Config{UpdateTolerancePatches: 0}
	called := false
	fetch := func(ctx context.Context) (cliremote.SystemVersionOutput, error) {
		called = true
		return cliremote.SystemVersionOutput{Version: "99.0.0"}, nil
	}
	err := runBootDriftCheck(context.Background(), "dev", cfg, fetch, &stderr, noEnv)
	if err != nil {
		t.Fatalf("expected nil under dev stamp, got %v", err)
	}
	if called {
		t.Errorf("fetcher should not be invoked when local version = dev")
	}
}

// TestBootDrift_UpdateCheckDisabledSkipped honours the TOML knob.
func TestBootDrift_UpdateCheckDisabledSkipped(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &cliconfig.Config{UpdateCheckDisabled: true}
	called := false
	fetch := func(ctx context.Context) (cliremote.SystemVersionOutput, error) {
		called = true
		return cliremote.SystemVersionOutput{Version: "99.0.0"}, nil
	}
	err := runBootDriftCheck(context.Background(), "0.0.1", cfg, fetch, &stderr, noEnv)
	if err != nil {
		t.Fatalf("expected nil under update_check_disabled, got %v", err)
	}
	if called {
		t.Errorf("fetcher should not be invoked when UpdateCheckDisabled=true")
	}
}

// TestBootDrift_EnvSkipShortCircuits asserts the
// SATELLITES_CLIENT_SKIP_UPDATE_CHECK env var disables the check.
// task_run.go relies on this branch to keep dispatched
// subprocesses from re-running the check.
func TestBootDrift_EnvSkipShortCircuits(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &cliconfig.Config{UpdateTolerancePatches: 0}
	called := false
	fetch := func(ctx context.Context) (cliremote.SystemVersionOutput, error) {
		called = true
		return cliremote.SystemVersionOutput{Version: "99.0.0"}, nil
	}
	envSkip := func(key string) string {
		if key == driftEnvSkip {
			return "1"
		}
		return ""
	}
	err := runBootDriftCheck(context.Background(), "0.0.1", cfg, fetch, &stderr, envSkip)
	if err != nil {
		t.Fatalf("expected nil under env skip, got %v", err)
	}
	if called {
		t.Errorf("fetcher should not be invoked when env skip set")
	}
}

// TestParsePatchVersion exercises the semver-patch parser for the
// branches the drift check depends on.
func TestParsePatchVersion(t *testing.T) {
	cases := []struct {
		in    string
		want  int
		wantK bool
	}{
		{"0.0.1", 1, true},
		{"v0.0.269", 269, true},
		{"1.2.3-rc1", 3, true},
		{"1.2", 0, false},
		{"abc", 0, false},
		{"", 0, false},
		{"1.2.x", 0, false},
	}
	for _, tc := range cases {
		got, ok := parsePatchVersion(tc.in)
		if ok != tc.wantK || (ok && got != tc.want) {
			t.Errorf("parsePatchVersion(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.wantK)
		}
	}
}
