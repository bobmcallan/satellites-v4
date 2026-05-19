// home.go — daemon-home resolution for the `satellites-client serve`
// daemon. The resolver implements a four-step chain:
//
//  1. SATELLITES_DAEMON_HOME (env override; absolute or operator-relative).
//  2. <repo-root>/.satellites/daemon/ when a repo root is detectable
//     (install-convention via the binary's location OR a CWD-anchored
//     .git walk-up).
//  3. ~/.satellites/daemon/ as the no-project fallback.
//  4. $TMPDIR/satellites-daemon/ when even the user home is unavailable.
//
// The home branch is reached ONLY after resolveRepoRoot() returns false —
// no os.Stat of the legacy path, no compat probe, no migration adapter
// (pr_no_unrequested_compat, doc_8a677faf).
//
// Function-field seams (getenv / executable / getwd / userHomeDir) keep
// the resolver injectable from home_test.go without touching real os.*
// state. The production constructor defaultHomeResolver() wires the
// fields to the real os calls.

package clientdaemon

import (
	"os"
	"path/filepath"
)

// homeResolver carries the four os-shim seams the resolver depends on.
// Tests construct a literal with their own closures; production code
// uses defaultHomeResolver() which wires the real os.* implementations.
type homeResolver struct {
	getenv      func(string) string
	executable  func() (string, error)
	getwd       func() (string, error)
	userHomeDir func() (string, error)
	tempDir     func() string
	args        []string
	stat        func(string) (os.FileInfo, error)
}

// defaultHomeResolver returns a resolver wired to the real os.* calls.
func defaultHomeResolver() *homeResolver {
	return &homeResolver{
		getenv:      os.Getenv,
		executable:  os.Executable,
		getwd:       os.Getwd,
		userHomeDir: os.UserHomeDir,
		tempDir:     os.TempDir,
		args:        os.Args,
		stat:        os.Stat,
	}
}

// resolve walks the four-step chain documented at the top of this file
// and returns the absolute daemon-home directory the caller should use.
func (r *homeResolver) resolve() string {
	if v := r.getenv("SATELLITES_DAEMON_HOME"); v != "" {
		return v
	}
	if root, ok := r.resolveRepoRoot(); ok {
		return filepath.Join(root, ".satellites", "daemon")
	}
	if home, err := r.userHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".satellites", "daemon")
	}
	return filepath.Join(r.tempDir(), "satellites-daemon")
}

// resolveRepoRoot returns the project repo root when one is detectable.
// Two ordered detection strategies:
//
//   - Install convention: when the binary lives at
//     <repo>/.satellites/satellites-client (the cliconfig.RepoPath
//     convention from sty_796b8fe1), the repo root is filepath.Dir of
//     the exe directory.
//   - .git walk-up: from the CWD upward, the first directory that has
//     a `.git` subdirectory is treated as the repo root. Lets developers
//     run `go run ./cmd/satellites-client …` from any subdirectory of
//     the repo and still have the daemon home land beside the repo.
//
// Returns ("", false) when neither strategy fires — the caller then
// falls through to the user-home / $TMPDIR fallback chain.
func (r *homeResolver) resolveRepoRoot() (string, bool) {
	if exeDir := r.evalExeDir(); exeDir != "" && filepath.Base(exeDir) == ".satellites" {
		return filepath.Dir(exeDir), true
	}
	if cwd, err := r.getwd(); err == nil && cwd != "" {
		dir := cwd
		for {
			if fi, err := r.stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
				return dir, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", false
}

// evalExeDir mirrors cliconfig.defaultExeDir's resolution chain
// (os.Executable → EvalSymlinks → os.Args[0]) without importing
// cliconfig — layering_test.go pins the no-substrate-import rule on
// this package, so the chain is re-implemented here. Trivial
// duplication; no cross-package coupling.
func (r *homeResolver) evalExeDir() string {
	if exe, err := r.executable(); err == nil && exe != "" {
		if real, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			return filepath.Dir(real)
		}
		return filepath.Dir(exe)
	}
	if len(r.args) > 0 && r.args[0] != "" {
		return filepath.Dir(r.args[0])
	}
	return ""
}
