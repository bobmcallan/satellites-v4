// home_test.go — unit table for daemon-home resolution (sty_517a7db3).
//
// Five branches matching plan.md test strategy + AC1:
//
//  1. SATELLITES_DAEMON_HOME wins (env override is highest precedence).
//  2. Install convention: <exe_dir> ends in `.satellites/`, so daemon
//     home lands at <repo>/.satellites/daemon/ regardless of CWD.
//  3. .git walk-up: exe lives outside the install convention, but the
//     CWD has a .git ancestor; daemon home anchors there.
//  4. User-home fallback: no env, no install convention, no .git
//     ancestor; resolver falls back to ~/.satellites/daemon/.
//  5. $TMPDIR fallback: same as (4) but UserHomeDir errors; resolver
//     falls back to $TMPDIR/satellites-daemon.
//
// All cases use the homeResolver function-field seams — no real os.*
// calls leak into the unit suite.

package clientdaemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonHomeEnvWins(t *testing.T) {
	called := map[string]bool{}
	r := &homeResolver{
		getenv: func(k string) string {
			called["getenv"] = true
			if k == "SATELLITES_DAEMON_HOME" {
				return "/forced/path"
			}
			return ""
		},
		executable: func() (string, error) {
			called["executable"] = true
			return "", errors.New("should not be called")
		},
		getwd: func() (string, error) {
			called["getwd"] = true
			return "", errors.New("should not be called")
		},
		userHomeDir: func() (string, error) {
			called["userHomeDir"] = true
			return "", errors.New("should not be called")
		},
		tempDir: func() string {
			called["tempDir"] = true
			return "/should/not/use"
		},
		stat: func(string) (os.FileInfo, error) {
			called["stat"] = true
			return nil, errors.New("should not be called")
		},
	}
	got := r.resolve()
	if got != "/forced/path" {
		t.Fatalf("resolve = %q, want %q", got, "/forced/path")
	}
	for _, k := range []string{"executable", "getwd", "userHomeDir", "tempDir", "stat"} {
		if called[k] {
			t.Errorf("env-wins branch must short-circuit; %s was called", k)
		}
	}
	if !called["getenv"] {
		t.Errorf("env-wins branch must consult getenv")
	}
}

func TestDaemonHomeInstallConvention(t *testing.T) {
	r := &homeResolver{
		getenv: func(string) string { return "" },
		executable: func() (string, error) {
			return "/tmp/repoA/.satellites/satellites-client", nil
		},
		// getwd is only consulted when install-convention fails. If
		// the resolver gets here, this stub keeps the chain inert.
		getwd:       func() (string, error) { return "/", nil },
		userHomeDir: func() (string, error) { return "", errors.New("unused") },
		tempDir:     func() string { return "/tmp" },
		stat: func(string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		},
	}
	got := r.resolve()
	want := filepath.Join("/tmp/repoA", ".satellites", "daemon")
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}

func TestDaemonHomeGitWalkUp(t *testing.T) {
	// Build a real temp dir tree so EvalSymlinks/Stat behave honestly
	// for the .git walk-up. The .git here is a real directory so
	// fi.IsDir() returns true under the real os.Stat seam.
	repoRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	deep := filepath.Join(repoRoot, "sub", "sub2")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir deep: %v", err)
	}

	r := &homeResolver{
		getenv: func(string) string { return "" },
		executable: func() (string, error) {
			// Exe lives outside any `.satellites/` directory, so the
			// install-convention branch must miss.
			return "/usr/local/bin/satellites-client", nil
		},
		getwd:       func() (string, error) { return deep, nil },
		userHomeDir: func() (string, error) { return "/home/u", nil },
		tempDir:     func() string { return "/tmp" },
		stat:        os.Stat,
	}
	got := r.resolve()
	want := filepath.Join(repoRoot, ".satellites", "daemon")
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}

func TestDaemonHomeUserHomeFallback(t *testing.T) {
	// No env, exe outside the install convention, no .git ancestor.
	// getwd returns an isolated tempdir with no ancestors holding
	// `.git` (the walk eventually hits root and returns false).
	isolated := t.TempDir()

	r := &homeResolver{
		getenv:      func(string) string { return "" },
		executable:  func() (string, error) { return "/usr/local/bin/satellites-client", nil },
		getwd:       func() (string, error) { return isolated, nil },
		userHomeDir: func() (string, error) { return "/home/u", nil },
		tempDir:     func() string { return "/tmp" },
		stat: func(path string) (os.FileInfo, error) {
			// No `.git` anywhere on the walk-up — every Stat misses.
			return nil, os.ErrNotExist
		},
	}
	got := r.resolve()
	want := filepath.Join("/home/u", ".satellites", "daemon")
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}

func TestDaemonHomeTempDirFallback(t *testing.T) {
	isolated := t.TempDir()

	r := &homeResolver{
		getenv:      func(string) string { return "" },
		executable:  func() (string, error) { return "/usr/local/bin/satellites-client", nil },
		getwd:       func() (string, error) { return isolated, nil },
		userHomeDir: func() (string, error) { return "", errors.New("no home") },
		tempDir:     func() string { return "/tmp" },
		stat: func(path string) (os.FileInfo, error) {
			return nil, os.ErrNotExist
		},
	}
	got := r.resolve()
	want := filepath.Join("/tmp", "satellites-daemon")
	if got != want {
		t.Fatalf("resolve = %q, want %q", got, want)
	}
}
