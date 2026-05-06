package portal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/auth"
	"github.com/bobmcallan/satellites/internal/config"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
)

const fixtureSeedAgent = `---
name: fixture_agent
permission_patterns:
  - "Read:**"
tags: [fixture]
---
# Fixture Agent

A fixture agent seeded in the integration test. Visible only inside
the test process.
`

const fixtureSeedContract = `---
name: fixture_contract
category: develop
required_role: role_orchestrator
required_categories: [develop]
validation_mode: llm
permitted_actions:
  - "Read:**"
evidence_required: |
  Fixture evidence for the integration test.
tags: [fixture]
---
# Fixture Contract

Fixture body.
`

const fixtureSeedWorkflow = `---
name: fixture_workflow
required_slots:
  - { contract_name: plan, required: true, min_count: 1, max_count: 1 }
  - { contract_name: develop, required: true, min_count: 1, max_count: 5 }
tags: [fixture]
---
# Fixture Workflow

Fixture body.
`

// writeFixtureSeed places the three sample markdown files in
// dir/system/{agents,contracts,workflows}/.
func writeFixtureSeed(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"system/agents/fixture_agent.md":       fixtureSeedAgent,
		"system/contracts/fixture_contract.md": fixtureSeedContract,
		"system/workflows/fixture_workflow.md": fixtureSeedWorkflow,
	}
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// TestConfigPage_SeededDocsExistInStore covers AC1+AC2 of story_3af8985d:
// the configseed loader produces documents that the document store sees,
// proving the boot path works end-to-end.
func TestConfigPage_SeededDocsExistInStore(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixtureSeed(t, dir)

	docs := document.NewMemoryStore()
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	summary, err := configseed.Run(context.Background(), docs, dir, "wksp_sys", "system", now)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.Created != 3 {
		t.Fatalf("created = %d, want 3 (one each: agent, contract, workflow); errors=%v", summary.Created, summary.Errors)
	}

	// Each seeded type lands as scope=system with the file's `name`.
	cases := []struct {
		name    string
		typ     string
		docName string
	}{
		{name: "agent", typ: document.TypeAgent, docName: "fixture_agent"},
		{name: "contract", typ: document.TypeContract, docName: "fixture_contract"},
		{name: "workflow", typ: document.TypeWorkflow, docName: "fixture_workflow"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := docs.GetByName(context.Background(), "", tc.docName, nil)
			if err != nil {
				t.Fatalf("GetByName(%s): %v", tc.docName, err)
			}
			if got.Type != tc.typ {
				t.Errorf("type = %q, want %q", got.Type, tc.typ)
			}
			if got.Scope != document.ScopeSystem {
				t.Errorf("scope = %q, want system", got.Scope)
			}
			if got.Body == "" {
				t.Errorf("body empty")
			}
		})
	}
}

// TestConfigPage_RendersSeededItems is the regression-pin for the
// shipped bug originally tracked under story_7992c382: even though the
// configseed loader writes documents into the store at boot, the
// /config page rendered them as if absent. story_7992c382's page fix
// (System Content panels via SystemContracts / SystemAgents /
// SystemWorkflows on the composite) makes this assertion pass.
//
// Keeping the test parallel-tagged is fine — it only mutates an env
// var via t.Setenv, which Go scopes to the subtest.
func TestConfigPage_RendersSeededItems(t *testing.T) {
	t.Setenv(auth.GlobalAdminEmailsEnv, "alice@x.io")

	p, users, sessions, _, _, _, docs, _ := newTestPortalWithContracts(t, &config.Config{Env: "dev"})
	mux := http.NewServeMux()
	p.Register(mux)

	dir := t.TempDir()
	writeFixtureSeed(t, dir)
	if _, err := configseed.Run(context.Background(), docs, dir, "wksp_sys", "system", time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	user := auth.User{ID: "u_alice", Email: "alice@x.io"}
	users.Add(user)
	sess, _ := sessions.Create(user.ID, auth.DefaultSessionTTL)

	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: sess.ID})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"fixture_contract",
		"fixture_agent",
		"fixture_workflow",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered /config missing seeded name %q — story_7992c382 page fix needed", want)
		}
	}
}
