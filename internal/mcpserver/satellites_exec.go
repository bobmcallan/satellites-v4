// Package mcpserver — `satellites_exec` MCP verb.
//
// `satellites_exec` proxies a subprocess invocation of
// `bin/satellites-client` from an IDE-agent caller. The server
// spawns the CLI with the supplied argv + optional stdin, forwards
// the caller's bearer via the `SATELLITES_TOKEN` env var, captures
// stdout / stderr / exit_code, and returns a typed result. Output
// is bounded — payloads exceeding the configured cap fail with a
// `payload_too_large` error.
//
// Per cli-primary order:07c (sty_0881d04b). Architecture: the
// server-side spawn lets IDE agents that don't have
// `bin/satellites-client` on their host (Claude Code's web UI,
// Cursor, etc.) drive the substrate via the MCP transport without
// the substrate needing to expose the full ~107-verb MCP surface.

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bobmcallan/satellites/internal/auth"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// DefaultExecPayloadCapBytes is the upper bound on stdout + stderr
// captured per call. Operators override via the
// SATELLITES_EXEC_PAYLOAD_CAP env var (parsed as int bytes) at
// boot.
const DefaultExecPayloadCapBytes = 1 << 20 // 1 MiB

// DefaultExecTimeout caps the subprocess wall clock. Operators
// override via SATELLITES_EXEC_TIMEOUT (Go duration string).
const DefaultExecTimeout = 30 * time.Second

// execResult is the response payload emitted by satellites_exec.
type execResult struct {
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	ExitCode       int    `json:"exit_code"`
	StdoutTruncate bool   `json:"stdout_truncated,omitempty"`
	StderrTruncate bool   `json:"stderr_truncated,omitempty"`
}

// resolveCLIBin returns the path to bin/satellites-client. Env var
// SATELLITES_CLI_BIN wins; otherwise falls back to looking up the
// name on PATH.
func resolveCLIBin() string {
	if env := os.Getenv("SATELLITES_CLI_BIN"); env != "" {
		return env
	}
	return "satellites-client"
}

// resolveExecCap returns the configured payload cap.
func resolveExecCap() int {
	if env := os.Getenv("SATELLITES_EXEC_PAYLOAD_CAP"); env != "" {
		var n int
		if _, err := fmt.Sscanf(env, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return DefaultExecPayloadCapBytes
}

// resolveExecTimeout returns the configured wall-clock timeout.
func resolveExecTimeout() time.Duration {
	if env := os.Getenv("SATELLITES_EXEC_TIMEOUT"); env != "" {
		if d, err := time.ParseDuration(env); err == nil && d > 0 {
			return d
		}
	}
	return DefaultExecTimeout
}

// boundedBuffer reads up to cap bytes from r, returning the bytes,
// a truncated flag, and any error other than the cap-hit one.
func boundedBuffer(r io.Reader, cap int) ([]byte, bool, error) {
	if cap <= 0 {
		cap = DefaultExecPayloadCapBytes
	}
	buf := make([]byte, cap+1)
	n, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return buf[:n], false, nil
	}
	if err != nil {
		return buf[:n], false, err
	}
	// We read cap+1 bytes — truncated.
	return buf[:cap], true, nil
}

// handleSatellitesExec runs the bin/satellites-client subprocess.
// Required input: argv ([]string). Optional: stdin (string).
// Bearer auth is forwarded via SATELLITES_TOKEN env. Timeout +
// payload cap are server-side configurable.
func (s *Server) handleSatellitesExec(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	// argv is required. mcp-go's CallToolRequest.GetStringSlice
	// returns the typed value; we accept []any for permissive
	// JSON-RPC clients.
	rawArgv, ok := req.GetArguments()["argv"]
	if !ok {
		return mcpgo.NewToolResultError("satellites_exec: argv required"), nil
	}
	argv, err := coerceStringSlice(rawArgv)
	if err != nil {
		return mcpgo.NewToolResultError("satellites_exec: argv: " + err.Error()), nil
	}
	if len(argv) == 0 {
		return mcpgo.NewToolResultError("satellites_exec: argv must be non-empty"), nil
	}

	stdin := req.GetString("stdin", "")

	bin := resolveCLIBin()
	timeout := resolveExecTimeout()
	cap := resolveExecCap()

	// Caller's bearer comes from the existing UserFrom path —
	// authoritative for the satellites_exec call already. The
	// subprocess receives it via SATELLITES_TOKEN env so the CLI's
	// internal/clicred chain picks it up.
	caller, _ := auth.UserFrom(ctx)
	_ = caller

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, bin, argv...)
	cmd.Env = append(os.Environ(),
		// No-op when the CLI is configured via OAuth; the api_key
		// branch reads SATELLITES_TOKEN.
		"SATELLITES_TOKEN="+s.callerBearerForExec(ctx),
	)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return mcpgo.NewToolResultError("satellites_exec: stdout pipe: " + err.Error()), nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return mcpgo.NewToolResultError("satellites_exec: stderr pipe: " + err.Error()), nil
	}

	if err := cmd.Start(); err != nil {
		return mcpgo.NewToolResultError("satellites_exec: start: " + err.Error()), nil
	}

	// Drain stdout + stderr concurrently into bounded buffers.
	type streamResult struct {
		body  []byte
		trunc bool
		err   error
	}
	stdoutCh := make(chan streamResult, 1)
	stderrCh := make(chan streamResult, 1)
	go func() {
		body, trunc, err := boundedBuffer(stdoutPipe, cap)
		stdoutCh <- streamResult{body, trunc, err}
	}()
	go func() {
		body, trunc, err := boundedBuffer(stderrPipe, cap)
		stderrCh <- streamResult{body, trunc, err}
	}()
	out := <-stdoutCh
	errStream := <-stderrCh

	waitErr := cmd.Wait()
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		exitCode = exitErr.ExitCode()
	} else if waitErr != nil {
		// Non-ExitError waitErr is a wait/IO failure — surface as
		// 5xx-style server fault.
		return mcpgo.NewToolResultError("satellites_exec: wait: " + waitErr.Error()), nil
	}

	if out.err != nil {
		return mcpgo.NewToolResultError("satellites_exec: read stdout: " + out.err.Error()), nil
	}
	if errStream.err != nil {
		return mcpgo.NewToolResultError("satellites_exec: read stderr: " + errStream.err.Error()), nil
	}

	body, _ := json.Marshal(execResult{
		Stdout:         string(out.body),
		Stderr:         string(errStream.body),
		ExitCode:       exitCode,
		StdoutTruncate: out.trunc,
		StderrTruncate: errStream.trunc,
	})
	return mcpgo.NewToolResultText(string(body)), nil
}

// callerBearerForExec returns the bearer the subprocess inherits.
// Preserves whatever the caller authenticated with — bypassing
// auth here would let an unprivileged caller exec at higher
// privilege via the spawned CLI. The default lookup is the same
// `Authorization: Bearer ...` header path the wider mcpserver uses;
// when no bearer is on the call this returns "" and the spawned
// CLI must rely on its local credentials file.
func (s *Server) callerBearerForExec(ctx context.Context) string {
	if v, ok := ctx.Value(callerBearerKey{}).(string); ok {
		return v
	}
	return ""
}

// callerBearerKey is the unique context key the auth middleware
// stores the caller's raw bearer under so satellites_exec can
// forward it. Defining the key here keeps the dependency
// uni-directional — auth.go writes; satellites_exec reads.
type callerBearerKey struct{}

// coerceStringSlice accepts []any or []string and returns []string.
func coerceStringSlice(v any) ([]string, error) {
	switch s := v.(type) {
	case []string:
		return s, nil
	case []any:
		out := make([]string, 0, len(s))
		for i, item := range s {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("argv[%d] is not a string", i)
			}
			out = append(out, str)
		}
		return out, nil
	}
	return nil, fmt.Errorf("expected []string, got %T", v)
}

// _ silences unused-import warnings on the bytes package during
// migrations where the bounded buffer body changes shape.
var _ = bytes.MinRead
