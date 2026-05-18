package clientdaemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/agent/worker"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/config"
)

// TestWorkerShape (sty_5aa20f1b AC4) asserts the daemon's runOne
// invokes Dispatch with cfg + env identical to what the synchronous
// CLI assembles, AND opens a per-task log file at logsDir/<task>.log.
func TestWorkerShape(t *testing.T) {
	stub := newStub(t)
	stub.addTask("task_w", map[string]any{
		"workspace_id": "wksp_x",
		"project_id":   "proj_x",
		"story_id":     "sty_x",
		"origin":       "story_stage",
	})

	type capture struct {
		cfg      config.AgentConfig
		env      worker.TaskEnvelope
		stdoutOK bool
		stderrOK bool
	}
	captureCh := make(chan capture, 1)

	d := newTestDaemon(t, stub, func(o *Options) {
		o.Parallelism = 1
		o.Heartbeat = time.Hour
		o.AgentConfig = config.AgentConfig{
			RepoPath:         "/repo/path",
			BranchTemplate:   "client-{task_id}-from-{base_sha}",
			WorktreeRoot:     "/repo/path/.satellites/worktree",
			ClaudeBinaryPath: "claude",
			SpawnMCPURL:      "https://test.example/mcp",
			AuthToken:        "test-bearer",
			ExecuteTimeout:   30 * time.Minute,
			LogLevel:         "info",
		}
		o.Dispatch = func(_ context.Context, cfg config.AgentConfig, _ arbor.ILogger, _ *cliremote.Client, env worker.TaskEnvelope, stdout, stderr io.Writer) (worker.Outcome, error) {
			n1, _ := stdout.Write([]byte("hello\n"))
			n2, _ := stderr.Write([]byte("warn\n"))
			captureCh <- capture{cfg: cfg, env: env, stdoutOK: n1 > 0, stderrOK: n2 > 0}
			return worker.OutcomeSuccess, nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); d.WaitInflight() }()
	slots := make(chan struct{}, d.opts.Parallelism)
	go d.runScheduler(ctx, slots)

	if _, err := d.Enqueue(ctx, "task_w"); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	var got capture
	select {
	case got = <-captureCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("dispatch never invoked")
	}

	want := config.AgentConfig{
		RepoPath:         "/repo/path",
		BranchTemplate:   "client-{task_id}-from-{base_sha}",
		WorktreeRoot:     "/repo/path/.satellites/worktree",
		ClaudeBinaryPath: "claude",
		SpawnMCPURL:      "https://test.example/mcp",
		AuthToken:        "test-bearer",
		ExecuteTimeout:   30 * time.Minute,
		LogLevel:         "info",
	}
	if got.cfg.RepoPath != want.RepoPath {
		t.Errorf("cfg.RepoPath = %q, want %q", got.cfg.RepoPath, want.RepoPath)
	}
	if got.cfg.BranchTemplate != want.BranchTemplate {
		t.Errorf("cfg.BranchTemplate = %q, want %q", got.cfg.BranchTemplate, want.BranchTemplate)
	}
	if got.cfg.WorktreeRoot != want.WorktreeRoot {
		t.Errorf("cfg.WorktreeRoot = %q, want %q", got.cfg.WorktreeRoot, want.WorktreeRoot)
	}
	if got.cfg.ClaudeBinaryPath != want.ClaudeBinaryPath {
		t.Errorf("cfg.ClaudeBinaryPath = %q, want %q", got.cfg.ClaudeBinaryPath, want.ClaudeBinaryPath)
	}
	if got.cfg.SpawnMCPURL != want.SpawnMCPURL {
		t.Errorf("cfg.SpawnMCPURL = %q, want %q", got.cfg.SpawnMCPURL, want.SpawnMCPURL)
	}
	if got.cfg.AuthToken != want.AuthToken {
		t.Errorf("cfg.AuthToken = %q, want %q", got.cfg.AuthToken, want.AuthToken)
	}

	if got.env.ID != "task_w" {
		t.Errorf("env.ID = %q, want task_w", got.env.ID)
	}
	if got.env.WorkspaceID != "wksp_x" || got.env.ProjectID != "proj_x" || got.env.Origin != "story_stage" {
		t.Errorf("env mismatch: %+v", got.env)
	}
	if !got.stdoutOK || !got.stderrOK {
		t.Errorf("stdout/stderr writers were nil-safe but did not accept bytes")
	}

	// Per-task log file at logsDir/<task>.log received the dispatched
	// stdout/stderr bytes.
	logPath := filepath.Join(d.opts.LogsDir, "task_w.log")
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(logPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("per-task log %s missing: %v", logPath, err)
	}
	if !strings.Contains(string(body), "hello") || !strings.Contains(string(body), "warn") {
		t.Errorf("per-task log missing dispatch bytes: %q", body)
	}
}
