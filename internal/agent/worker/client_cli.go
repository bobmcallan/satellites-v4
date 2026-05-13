// cliClient is the satellites-agent's CLI-shelling worker Client.
// Per cli-primary order:06 (sty_9aa8c2c2): the agent's transport
// switches from MCP JSON-RPC HTTP to subprocess execution of
// `bin/satellites-client`. Same wire payloads (task_claim,
// ledger_append, task_update) reach the substrate via the CLI's
// auto-JSON-when-piped output instead of an in-process HTTP client.
//
// Post sty_4db0e025 slice E1 cliClient is the agent daemon's sole
// Client implementation — the legacy MCP-rooted transport + the
// `transport` config knob's "mcp" enum value were retired together
// with the placeholderClient shim.

package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/cliexit"
	"github.com/bobmcallan/satellites/internal/config"
)

// cliClient implements the worker.Client interface by shelling out
// to bin/satellites-client. Each method builds an argv that the CLI
// parses; stdout is the JSON envelope auto-emitted when stdout is
// not a tty.
type cliClient struct {
	cfg    config.AgentConfig
	logger arbor.ILogger
	bin    string // resolved path to satellites-client (or bare name → PATH lookup at exec time)
}

// NewCLIClient constructs the CLI-shelling worker Client. Falls back
// to "satellites-client" on PATH when cfg.CLIBinaryPath is empty.
func NewCLIClient(cfg config.AgentConfig, logger arbor.ILogger) Client {
	bin := cfg.CLIBinaryPath
	if bin == "" {
		bin = "satellites-client"
	}
	return &cliClient{
		cfg:    cfg,
		logger: logger,
		bin:    bin,
	}
}

// runCLI invokes the configured CLI binary with the supplied argv.
// Stdout is captured for caller-side decoding; non-zero exit codes
// surface as typed errors keyed off cliexit.Code (3 → not found,
// 4 → auth, 5 → server). Always passes --server / --token /
// --json inherited from cfg.
func (c *cliClient) runCLI(ctx context.Context, argv []string) ([]byte, error) {
	full := []string{}
	if c.cfg.MCPURL != "" {
		full = append(full, "--server", c.cfg.MCPURL)
	}
	if c.cfg.AuthToken != "" {
		full = append(full, "--token", c.cfg.AuthToken)
	}
	full = append(full, "--json")
	full = append(full, argv...)

	cmd := exec.CommandContext(ctx, c.bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := cliexit.Code(exitErr.ExitCode())
			return nil, fmt.Errorf("cliClient: %s exited %d: %s", argv[0], int(code), strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("cliClient: %s: %w (stderr: %s)", argv[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Claim shells `satellites-client task claim --workspace-id <id>`.
// Returns nil on empty queue (the CLI emits {} or null), the task
// envelope otherwise.
func (c *cliClient) Claim(ctx context.Context, workerID string, workspaceIDs []string) (*TaskEnvelope, error) {
	argv := []string{"task", "claim"}
	if len(workspaceIDs) > 0 {
		argv = append(argv, "--workspace-id", workspaceIDs[0])
	}
	if workerID != "" {
		argv = append(argv, "--worker-id", workerID)
	}
	out, err := c.runCLI(ctx, argv)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 || string(trimmed) == "null" || string(trimmed) == "{}" {
		return nil, nil
	}
	var env struct {
		ID           string `json:"id"`
		WorkspaceID  string `json:"workspace_id"`
		ProjectID    string `json:"project_id"`
		Origin       string `json:"origin"`
		LedgerRootID string `json:"ledger_root_id"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, fmt.Errorf("cliClient: decode task_claim envelope: %w", err)
	}
	if env.ID == "" {
		return nil, nil
	}
	return &TaskEnvelope{
		ID:           env.ID,
		WorkspaceID:  env.WorkspaceID,
		ProjectID:    env.ProjectID,
		Origin:       env.Origin,
		LedgerRootID: env.LedgerRootID,
	}, nil
}

// Execute writes a single kind:agent-noop ledger row tagged with the
// task id. Mirrors the placeholder's Execute body so the rollback
// path is byte-identical.
func (c *cliClient) Execute(ctx context.Context, task TaskEnvelope) (Outcome, error) {
	content := fmt.Sprintf("# agent-noop\n\nPlaceholder execution for task %s on workspace %s.\n", task.ID, task.WorkspaceID)
	argv := []string{"ledger", "append",
		"--project-id", task.ProjectID,
		"--type", "evidence",
		"--tags", "kind:agent-noop,task_id:" + task.ID,
		"--content", content,
	}
	if _, err := c.runCLI(ctx, argv); err != nil {
		return OutcomeFailure, err
	}
	return OutcomeSuccess, nil
}

// Close shells `satellites-client task update --id <id> --status closed --outcome <o>`.
func (c *cliClient) Close(ctx context.Context, taskID, workerID string, outcome Outcome) error {
	if taskID == "" {
		return errors.New("cliClient: close: empty task_id")
	}
	argv := []string{"task", "update",
		"--id", taskID,
		"--status", "closed",
		"--outcome", string(outcome),
	}
	_, err := c.runCLI(ctx, argv)
	return err
}

// Heartbeat appends one kind:worker-heartbeat ledger row.
func (c *cliClient) Heartbeat(ctx context.Context, workerID, taskID, workspaceID string) error {
	content := fmt.Sprintf("# worker-heartbeat\n\nworker=%s task=%s workspace=%s\n", workerID, taskID, workspaceID)
	argv := []string{"ledger", "append",
		"--type", "evidence",
		"--tags", "kind:worker-heartbeat,task_id:" + taskID + ",worker_id:" + workerID,
		"--content", content,
	}
	// project_id is omitted — the CLI verb may reject the call when
	// project_id is required and missing; tracked as a follow-up
	// (sty_4db0e025 slice E1 retired the parallel MCP transport that
	// previously enforced the same omission).
	_, err := c.runCLI(ctx, argv)
	return err
}

// Shutdown appends one kind:worker-shutdown ledger row.
func (c *cliClient) Shutdown(ctx context.Context, workerID, reason string) error {
	content := fmt.Sprintf("# worker-shutdown\n\nworker=%s reason=%s\n", workerID, reason)
	argv := []string{"ledger", "append",
		"--type", "evidence",
		"--tags", "kind:worker-shutdown,worker_id:" + workerID,
		"--content", content,
	}
	_, err := c.runCLI(ctx, argv)
	return err
}
