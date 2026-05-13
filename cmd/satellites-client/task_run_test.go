package main

import (
	"os"
	"strings"
	"testing"
)

// TestTaskRunSetsSkipUpdateCheckEnv asserts the literal env wiring
// task_run.go uses to suppress the boot drift check in dispatched
// subprocesses (sty_64e69db8). The dispatcher reads os.Environ()
// when composing the spawned cmd.Env (see
// internal/agent/worker/client_claude.go), so setting the env on
// the parent before RunDispatched is the canonical mechanism.
//
// The check is source-level rather than a true exec because the
// real RunDispatched needs claude + a worktree + an HTTP server —
// out of scope for this unit test. The dispatched-subprocess path
// is exercised end-to-end by tests/integration/system_version_test.go.
func TestTaskRunSetsSkipUpdateCheckEnv(t *testing.T) {
	body, err := os.ReadFile("task_run.go")
	if err != nil {
		t.Fatalf("read task_run.go: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, `os.Setenv(driftEnvSkip, "1")`) {
		t.Fatalf(`task_run.go does not set the SATELLITES_CLIENT_SKIP_UPDATE_CHECK env via os.Setenv(driftEnvSkip, "1")`)
	}
}
