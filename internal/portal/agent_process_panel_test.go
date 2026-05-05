// sty_e1ab884d — the /projects/{id}/configuration page renders the
// resolved agent-process markdown under data-testid="config-agent-process".
// Empty-state shows the system default body inline + the not-configured
// copy. Override populates a project-specific body and an edit link.
package portal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/agentprocess"
	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
)

func seedAgentProcessDefault(t *testing.T, ctx context.Context, docs *document.MemoryStore, now time.Time) {
	t.Helper()
	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	if _, err := configseed.Run(ctx, docs, seedDir, "wksp_a", "system", now); err != nil {
		t.Fatalf("configseed Run: %v", err)
	}
}

func TestAgentProcessPanel_EmptyStateRendersSystemDefault(t *testing.T) {
	t.Parallel()
	rec := renderConfiguration(t, func(ctx context.Context, projectID string, docs *document.MemoryStore) {
		// Seed only the system default — no project-scope override.
		seedAgentProcessDefault(t, ctx, docs, time.Now().UTC())
	})
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="config-agent-process"`,
		`data-testid="config-agent-process-empty"`,
		// The system default body is rendered inline so the user sees
		// what the agent receives.
		`data-testid="config-agent-process-body"`,
		// Scope pill shows "system default".
		"system default",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Override-only markers must not appear.
	for _, mustNot := range []string{
		`data-testid="config-agent-process-edit"`,
		"project override",
	} {
		if strings.Contains(body, mustNot) {
			t.Errorf("body should not contain %q on empty-state path", mustNot)
		}
	}
}

func TestAgentProcessPanel_ProjectOverrideRendersCustomBody(t *testing.T) {
	t.Parallel()
	const customBody = "PROJECT_CUSTOM_BODY_42"
	rec := renderConfiguration(t, func(ctx context.Context, projectID string, docs *document.MemoryStore) {
		now := time.Now().UTC()
		seedAgentProcessDefault(t, ctx, docs, now)
		pid := projectID
		_, err := docs.Create(ctx, document.Document{
			Type:      document.TypeArtifact,
			Scope:     document.ScopeProject,
			ProjectID: &pid,
			Name:      agentprocess.ProjectOverrideName,
			Body:      customBody,
			Status:    document.StatusActive,
			Tags:      []string{agentprocess.KindTag},
		}, now)
		if err != nil {
			t.Fatalf("seed override: %v", err)
		}
	})
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="config-agent-process"`,
		`data-testid="config-agent-process-edit"`,
		"project override",
		customBody,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
	// Empty-state markers must not appear when an override is active.
	if strings.Contains(body, `data-testid="config-agent-process-empty"`) {
		t.Errorf("override path still rendered the empty-state copy")
	}
}
