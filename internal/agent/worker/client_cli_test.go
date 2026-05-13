package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ternarybob/arbor"

	"github.com/bobmcallan/satellites/internal/config"
)

// buildStubCLI compiles a tiny stub program that the cliClient can
// run as if it were satellites-client. The stub inspects argv +
// stdin and emits canned output. Each test points cfg.CLIBinaryPath
// at the temp binary.
func buildStubCLI(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "stub.go")
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	bin := filepath.Join(dir, "stub")
	build := exec.Command("go", "build", "-o", bin, src)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, out)
	}
	return bin
}

const stubClaimHappy = `package main
import (
	"fmt"
	"os"
	"strings"
)
func main() {
	args := os.Args[1:]
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "task claim") {
		fmt.Fprintf(os.Stderr, "unexpected argv: %v\n", args)
		os.Exit(2)
	}
	fmt.Print(` + "`" + `{"id":"task_x","workspace_id":"wksp_a","project_id":"proj_q","origin":"scheduled","ledger_root_id":""}` + "`" + `)
}
`

const stubClaimEmpty = `package main
import "fmt"
func main() { fmt.Print("null") }
`

const stubExitNotFound = `package main
import "os"
func main() { os.Exit(3) }
`

func TestCLIClient_ClaimHappy(t *testing.T) {
	bin := buildStubCLI(t, stubClaimHappy)
	cfg := config.AgentConfig{
		SpawnMCPURL:   "http://stub",
		AuthToken:     "fake",
		CLIBinaryPath: bin,
	}
	client := NewCLIClient(cfg, arbor.GetLogger())
	env, err := client.Claim(context.Background(), "worker-x", []string{"wksp_a"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if env == nil || env.ID != "task_x" || env.WorkspaceID != "wksp_a" {
		t.Fatalf("wrong envelope: %+v", env)
	}
}

func TestCLIClient_ClaimEmptyQueue(t *testing.T) {
	bin := buildStubCLI(t, stubClaimEmpty)
	cfg := config.AgentConfig{SpawnMCPURL: "http://stub", CLIBinaryPath: bin}
	client := NewCLIClient(cfg, arbor.GetLogger())
	env, err := client.Claim(context.Background(), "worker-x", nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if env != nil {
		t.Fatalf("expected nil envelope on empty queue, got %+v", env)
	}
}

func TestCLIClient_ClaimExitNotFound(t *testing.T) {
	bin := buildStubCLI(t, stubExitNotFound)
	cfg := config.AgentConfig{SpawnMCPURL: "http://stub", CLIBinaryPath: bin}
	client := NewCLIClient(cfg, arbor.GetLogger())
	_, err := client.Claim(context.Background(), "worker-x", nil)
	if err == nil {
		t.Fatal("expected error from exit 3")
	}
	if !strings.Contains(err.Error(), "exited 3") {
		t.Fatalf("error did not surface exit code 3: %v", err)
	}
}

func TestCLIClient_CloseEmptyTaskID(t *testing.T) {
	cfg := config.AgentConfig{SpawnMCPURL: "http://stub", CLIBinaryPath: "/does/not/exist"}
	client := NewCLIClient(cfg, arbor.GetLogger())
	err := client.Close(context.Background(), "", "worker-x", OutcomeSuccess)
	if err == nil {
		t.Fatal("expected error for empty task id")
	}
}

func TestCLIClient_HeartbeatRunsLedgerAppend(t *testing.T) {
	const stub = `package main
import (
	"fmt"
	"os"
	"strings"
)
func main() {
	if !strings.Contains(strings.Join(os.Args[1:], " "), "ledger append") {
		fmt.Fprintln(os.Stderr, "unexpected argv")
		os.Exit(2)
	}
	fmt.Print(` + "`" + `{"id":"ldg_x"}` + "`" + `)
}
`
	bin := buildStubCLI(t, stub)
	cfg := config.AgentConfig{SpawnMCPURL: "http://stub", CLIBinaryPath: bin}
	client := NewCLIClient(cfg, arbor.GetLogger())
	if err := client.Heartbeat(context.Background(), "worker-x", "task_x", "wksp_a"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
}
