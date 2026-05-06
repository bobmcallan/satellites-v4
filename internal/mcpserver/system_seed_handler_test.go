package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	satarbor "github.com/bobmcallan/satellites/internal/arbor"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/project"
	"github.com/bobmcallan/satellites/internal/session"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/workspace"
)

const sampleAgentForSeed = `---
name: test_agent
permission_patterns:
  - "Read:**"
tags: [test]
---
# Test Agent

Body for the test agent.
`

func newSystemSeedFixture(t *testing.T) (*Server, string) {
	t.Helper()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{Env: "dev"}
	docs := document.NewMemoryStore()
	led := ledger.NewMemoryStore()
	stories := story.NewMemoryStore(led)
	projects := project.NewMemoryStore()
	ws := workspace.NewMemoryStore()
	sessions := session.NewMemoryStore()

	server := New(cfg, satarbor.New("info"), now, Deps{
		DocStore:       docs,
		ProjectStore:   projects,
		LedgerStore:    led,
		StoryStore:     stories,
		WorkspaceStore: ws,
		SessionStore:   sessions,
	})

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "system", "agents"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "system", "agents", "test.md"), []byte(sampleAgentForSeed), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	t.Setenv(configseed.SeedDirEnv, dir)
	t.Setenv(configseed.HelpDirEnv, dir+"/_no_help_dir")

	return server, dir
}

// TestSystemSeedRun_ForbiddenForNonAdmin covers AC1: non-admin callers
// receive a structured forbidden error.
func TestSystemSeedRun_ForbiddenForNonAdmin(t *testing.T) {
	server, _ := newSystemSeedFixture(t)
	ctx := context.WithValue(context.Background(), userKey, CallerIdentity{
		UserID: "u_alice", Email: "alice@x.io", Source: "session", GlobalAdmin: false,
	})
	res, err := server.handleSystemSeedRun(ctx, newCallToolReq("system_seed_run", map[string]any{}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected isError; got body=%s", firstText(res))
	}
	body := firstText(res)
	if body == "" || !strings.Contains(body, "forbidden") {
		t.Errorf("expected forbidden error; got %q", body)
	}
}

// TestSystemSeedRun_AdminSucceedsAndWritesLedger covers AC1 + AC2: an
// admin caller runs the seed, gets a summary back, and a
// kind:system-seed-run ledger row is written.
func TestSystemSeedRun_AdminSucceedsAndWritesLedger(t *testing.T) {
	server, _ := newSystemSeedFixture(t)
	ctx := context.WithValue(context.Background(), userKey, CallerIdentity{
		UserID: "u_bob", Email: "bob@x.io", Source: "session", GlobalAdmin: true,
	})
	res, err := server.handleSystemSeedRun(ctx, newCallToolReq("system_seed_run", map[string]any{}))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected isError: %s", firstText(res))
	}
	var summary SystemSeedRunResult
	if err := json.Unmarshal([]byte(firstText(res)), &summary); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if summary.Loaded != 1 || summary.Created != 1 {
		t.Errorf("loaded=%d created=%d, want 1/1", summary.Loaded, summary.Created)
	}
	rows, err := server.ledger.List(context.Background(), "", ledger.ListOptions{
		Type: ledger.TypeDecision,
		Tags: []string{"kind:system-seed-run"},
	}, nil)
	if err != nil {
		t.Fatalf("list ledger: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("ledger rows = %d, want 1", len(rows))
	}
	if rows[0].CreatedBy != "u_bob" {
		t.Errorf("CreatedBy = %q, want %q", rows[0].CreatedBy, "u_bob")
	}
}
