package auth_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoEscalationBypass asserts the verb-allowlist gate has no
// env-var bypass, no per-call escalation flag, no `--bypass`-style
// CLI knob anywhere in the Go tree. Cited in review-criteria.md AC6:
// pr_no_unrequested_compat forbids compat shims for the gate;
// operators who need broader access author a fresh story that
// updates pr_role_grid first. sty_056b68f6.
func TestNoEscalationBypass(t *testing.T) {
	t.Parallel()
	root := repoRootFromCWD(t)
	forbidden := []string{
		"SATELLITES_SKIP_VERB_GATE",
		"SKIP_VERB_ALLOWLIST",
		"allowed_verbs_override",
		"allowed_verbs_force",
		"--allowed-verbs-bypass",
		"verb_allowlist_disable",
	}
	skipDirs := map[string]struct{}{
		".git":         {},
		".satellites":  {},
		"node_modules": {},
	}
	skipFiles := map[string]struct{}{
		"no_escalation_lint_test.go": {},
	}
	type hit struct {
		path  string
		token string
		line  int
	}
	var hits []hit
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if _, skip := skipDirs[info.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		// Lint Go sources, markdown, and the satellites toml only.
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".md" && ext != ".toml" {
			return nil
		}
		if _, skip := skipFiles[info.Name()]; skip {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		lines := strings.Split(string(body), "\n")
		for i, line := range lines {
			for _, tok := range forbidden {
				if strings.Contains(line, tok) {
					hits = append(hits, hit{path: path, token: tok, line: i + 1})
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if len(hits) > 0 {
		for _, h := range hits {
			t.Errorf("forbidden bypass token %q at %s:%d", h.token, h.path, h.line)
		}
		t.Fatalf("verb-gate bypass token(s) introduced — see pr_no_unrequested_compat")
	}
}

// repoRootFromCWD walks up from the test cwd until it finds a
// go.mod file (the repo root).
func repoRootFromCWD(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			return cwd
		}
		cwd = filepath.Dir(cwd)
	}
	t.Fatal("repoRootFromCWD: go.mod not found within 8 ancestors")
	return ""
}
