package agentprocess

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/configseed"
	"github.com/bobmcallan/satellites/internal/document"
)

// TestConfigseedRunsArtifactsIdempotent — sty_6c3f8091 wires the
// `default_agent_process` artifact through configseed (KindArtifact).
// A second Run pass against the same on-disk seed must not duplicate
// the row.
func TestConfigseedRunsArtifactsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := document.NewMemoryStore()
	seedDir := absSeedDir(t)
	now := time.Now().UTC()

	if _, err := configseed.Run(ctx, store, seedDir, "wksp_a", "system", now); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if _, err := configseed.Run(ctx, store, seedDir, "wksp_a", "system", now.Add(time.Minute)); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	rows, err := store.List(ctx, document.ListOptions{
		Type:  document.TypeArtifact,
		Scope: document.ScopeSystem,
		Tags:  []string{KindTag},
	}, nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	matched := 0
	for _, r := range rows {
		if r.Name == SystemDefaultName {
			matched++
		}
	}
	if matched != 1 {
		t.Errorf("seeded default_agent_process count = %d, want 1 (idempotent re-seed must not duplicate)", matched)
	}
}

func TestResolve_ProjectOverrideWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := document.NewMemoryStore()
	now := time.Now().UTC()

	seedSystemDefault(t, ctx, store, "wksp_a", "SYSTEM_DEFAULT_BODY", now)

	pid := "proj_alpha"
	if _, err := store.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		ProjectID:   &pid,
		Type:        document.TypeArtifact,
		Scope:       document.ScopeProject,
		Name:        ProjectOverrideName,
		Body:        "PROJECT_OVERRIDE_BODY",
		Status:      document.StatusActive,
		Tags:        []string{KindTag},
	}, now); err != nil {
		t.Fatalf("seed override: %v", err)
	}
	body := Resolve(ctx, store, pid, nil)
	if body != "PROJECT_OVERRIDE_BODY" {
		t.Errorf("Resolve(project=set) = %q, want PROJECT_OVERRIDE_BODY", body)
	}
}

func TestResolve_FallsBackToSystemDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := document.NewMemoryStore()
	now := time.Now().UTC()
	const seeded = "SYSTEM_DEFAULT_BODY"
	seedSystemDefault(t, ctx, store, "wksp_a", seeded, now)
	if got := Resolve(ctx, store, "proj_no_override", nil); got != seeded {
		t.Errorf("Resolve fell through with no project override = %q, want %q", got, seeded)
	}
}

func TestResolve_EmptyWhenNeitherSeeded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := document.NewMemoryStore()
	body := Resolve(ctx, store, "proj_anything", nil)
	if body != "" {
		t.Errorf("Resolve with no seeds = %q, want empty", body)
	}
}

func TestResolve_NilStore(t *testing.T) {
	t.Parallel()
	if got := Resolve(context.Background(), nil, "proj", nil); got != "" {
		t.Errorf("Resolve(nil store) = %q, want empty", got)
	}
}

func TestResolve_DenyAllMembershipsReturnsEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := document.NewMemoryStore()
	now := time.Now().UTC()
	seedSystemDefault(t, ctx, store, "wksp_a", "SOME_BODY", now)
	if got := Resolve(ctx, store, "proj_x", []string{}); got != "" {
		t.Errorf("Resolve(deny-all memberships) = %q, want empty", got)
	}
}

func TestResolve_RejectsWrongTypeOrInactive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := document.NewMemoryStore()
	now := time.Now().UTC()
	// Right name, but type=principle — must not be returned.
	if _, err := store.Create(ctx, document.Document{
		WorkspaceID: "wksp_a",
		ProjectID:   nil,
		Type:        document.TypePrinciple,
		Scope:       document.ScopeSystem,
		Name:        SystemDefaultName,
		Body:        "WRONG_TYPE",
		Status:      document.StatusActive,
		Tags:        []string{KindTag},
	}, now); err != nil {
		t.Fatalf("seed wrong-type: %v", err)
	}
	if got := Resolve(ctx, store, "", nil); got == "WRONG_TYPE" {
		t.Errorf("Resolve served a non-artifact row with the right name")
	}
}

// seedSystemDefault is a test helper that creates a system-scope
// default_agent_process artifact directly with the supplied body.
// Production seeding goes through configseed.Run.
func seedSystemDefault(t *testing.T, ctx context.Context, store document.Store, ws, body string, now time.Time) {
	t.Helper()
	if _, err := store.Create(ctx, document.Document{
		WorkspaceID: ws,
		ProjectID:   nil,
		Type:        document.TypeArtifact,
		Scope:       document.ScopeSystem,
		Name:        SystemDefaultName,
		Body:        body,
		Status:      document.StatusActive,
		Tags:        []string{KindTag, "v4", "seed"},
		CreatedBy:   "system",
	}, now); err != nil {
		t.Fatalf("seed system default: %v", err)
	}
}

func absSeedDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}
	return dir
}
