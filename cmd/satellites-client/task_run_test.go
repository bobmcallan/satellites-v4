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
// the parent before dispatch is the canonical mechanism.
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

// TestTaskRunSetsDisableTelemetryEnv asserts task_run sets the
// SATELLITES_CLIENT_DISABLE_TELEMETRY env before dispatch so the
// spawned subprocess inherits it (sty_8c17b89d recursion guard).
// The literal env-var name now lives in internal/agent/dispatchteam
// (sty_5aa20f1b extraction); the CLI references it via the local
// telemetryEnvSkip alias.
func TestTaskRunSetsDisableTelemetryEnv(t *testing.T) {
	body, err := os.ReadFile("task_run.go")
	if err != nil {
		t.Fatalf("read task_run.go: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, `os.Setenv(telemetryEnvSkip, "1")`) {
		t.Fatalf(`task_run.go does not set the SATELLITES_CLIENT_DISABLE_TELEMETRY env via os.Setenv(telemetryEnvSkip, "1")`)
	}
	if !strings.Contains(src, `disableTelemetry := os.Getenv(telemetryEnvSkip) == "1"`) {
		t.Fatalf(`task_run.go does not read os.Getenv(telemetryEnvSkip) at the top of runTaskCmd`)
	}
}

// TestTaskRun_RecursionGuardDisablesTelemetryWhenEnvSet asserts the
// recursion-guard from sty_8c17b89d is still wired into runTaskCmd
// after the sty_5aa20f1b extraction. The literal env-var name lives
// in internal/agent/dispatchteam.TelemetryEnvSkip; the CLI uses the
// local telemetryEnvSkip alias to keep the call sites legible.
func TestTaskRun_RecursionGuardDisablesTelemetryWhenEnvSet(t *testing.T) {
	body, err := os.ReadFile("task_run.go")
	if err != nil {
		t.Fatalf("read task_run.go: %v", err)
	}
	src := string(body)

	if !strings.Contains(src, `const telemetryEnvSkip = dispatchteam.TelemetryEnvSkip`) {
		t.Fatalf(`task_run.go does not bind telemetryEnvSkip to dispatchteam.TelemetryEnvSkip`)
	}
	if !strings.Contains(src, `os.Setenv(telemetryEnvSkip, "1")`) {
		t.Fatalf(`task_run.go missing os.Setenv(telemetryEnvSkip, "1")`)
	}
	if !strings.Contains(src, `disableTelemetry := os.Getenv(telemetryEnvSkip) == "1"`) {
		t.Fatalf(`task_run.go missing disableTelemetry := os.Getenv(telemetryEnvSkip) == "1"`)
	}
	if !strings.Contains(src, "if !disableTelemetry") && !strings.Contains(src, "DisableTelemetry: disableTelemetry") {
		t.Fatalf(`task_run.go does not branch on the disableTelemetry flag`)
	}
}

// TestTaskRun_UsesDispatchteamRun is the post-extraction parity guard:
// the CLI must funnel its dispatch through the shared shell so the
// daemon (sty_5aa20f1b) and CLI emit identical telemetry frames.
func TestTaskRun_UsesDispatchteamRun(t *testing.T) {
	body, err := os.ReadFile("task_run.go")
	if err != nil {
		t.Fatalf("read task_run.go: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, `dispatchteam.Run(`) {
		t.Fatalf(`task_run.go does not call dispatchteam.Run — sty_5aa20f1b extraction would be incomplete`)
	}
	if !strings.Contains(src, `"github.com/bobmcallan/satellites/internal/agent/dispatchteam"`) {
		t.Fatalf(`task_run.go does not import internal/agent/dispatchteam`)
	}
}
