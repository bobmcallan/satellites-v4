package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// buildExecStub compiles a tiny Go program at runtime and returns
// its path. The stub plays the role of bin/satellites-client for
// satellites_exec tests.
func buildExecStub(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "stub.go")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	bin := filepath.Join(dir, "stub")
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, out)
	}
	return bin
}

const stubExecHappy = `package main
import (
	"fmt"
	"io"
	"os"
	"strings"
)
func main() {
	args := strings.Join(os.Args[1:], " ")
	fmt.Printf("argv=%s\n", args)
	if data, _ := io.ReadAll(os.Stdin); len(data) > 0 {
		fmt.Printf("stdin=%s\n", string(data))
	}
	if env := os.Getenv("SATELLITES_TOKEN"); env != "" {
		fmt.Fprintln(os.Stderr, "saw-token")
	}
}
`

const stubExecExit = `package main
import "os"
func main() { os.Exit(3) }
`

const stubExecLargeStdout = `package main
import (
	"fmt"
	"strings"
)
func main() { fmt.Print(strings.Repeat("x", 1<<21)) } // 2 MiB > default 1 MiB
`

func TestSatellitesExec_HappyPath(t *testing.T) {
	bin := buildExecStub(t, stubExecHappy)
	t.Setenv("SATELLITES_CLI_BIN", bin)

	s := &Server{}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"argv":  []any{"task", "get", "task_x"},
		"stdin": "hello",
	}
	res, err := s.handleSatellitesExec(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSatellitesExec: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
	var got execResult
	if err := json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(got.Stdout, "argv=task get task_x") {
		t.Errorf("stdout missing argv echo: %q", got.Stdout)
	}
	if !strings.Contains(got.Stdout, "stdin=hello") {
		t.Errorf("stdout missing stdin echo: %q", got.Stdout)
	}
	if got.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", got.ExitCode)
	}
}

func TestSatellitesExec_ExitCodeForwarded(t *testing.T) {
	bin := buildExecStub(t, stubExecExit)
	t.Setenv("SATELLITES_CLI_BIN", bin)

	s := &Server{}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"argv": []any{"task", "get", "missing"}}
	res, err := s.handleSatellitesExec(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("unexpected: err=%v res=%+v", err, res)
	}
	var got execResult
	_ = json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &got)
	if got.ExitCode != 3 {
		t.Errorf("exit_code = %d, want 3", got.ExitCode)
	}
}

func TestSatellitesExec_PayloadCapTruncates(t *testing.T) {
	bin := buildExecStub(t, stubExecLargeStdout)
	t.Setenv("SATELLITES_CLI_BIN", bin)
	t.Setenv("SATELLITES_EXEC_PAYLOAD_CAP", "1024")

	s := &Server{}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"argv": []any{"long"}}
	res, err := s.handleSatellitesExec(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("unexpected: err=%v res=%+v", err, res)
	}
	var got execResult
	_ = json.Unmarshal([]byte(res.Content[0].(mcpgo.TextContent).Text), &got)
	if !got.StdoutTruncate {
		t.Errorf("expected stdout_truncated=true, got false")
	}
	if len(got.Stdout) != 1024 {
		t.Errorf("stdout length = %d, want 1024", len(got.Stdout))
	}
}

func TestSatellitesExec_MissingArgv(t *testing.T) {
	s := &Server{}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	res, _ := s.handleSatellitesExec(context.Background(), req)
	if !res.IsError {
		t.Fatalf("expected error result for missing argv")
	}
	if !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "argv required") {
		t.Errorf("expected 'argv required' marker, got: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
}

func TestSatellitesExec_EmptyArgv(t *testing.T) {
	s := &Server{}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"argv": []any{}}
	res, _ := s.handleSatellitesExec(context.Background(), req)
	if !res.IsError {
		t.Fatalf("expected error for empty argv")
	}
	if !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "non-empty") {
		t.Errorf("expected 'non-empty' marker: %s", res.Content[0].(mcpgo.TextContent).Text)
	}
}

func TestSatellitesExec_BinaryMissing(t *testing.T) {
	t.Setenv("SATELLITES_CLI_BIN", "/no/such/binary")

	s := &Server{}
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{"argv": []any{"info"}}
	res, _ := s.handleSatellitesExec(context.Background(), req)
	if !res.IsError {
		t.Fatalf("expected error for missing binary")
	}
}
