package configseed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/document"
)

// SeedDirEnv is the env var that overrides the default seed location.
// Tests set it via t.Setenv; production callers usually let the binary
// resolve the default `./config/seed`.
const SeedDirEnv = "SATELLITES_SEED_DIR"

// HelpDirEnv is the analogous override for the help corpus.
// story_cc5c67a9.
const HelpDirEnv = "SATELLITES_HELP_DIR"

// DefaultSeedDir is the path the binary resolves when the env var is
// unset. Resolved relative to the working directory.
const DefaultSeedDir = "./config/seed"

// DefaultHelpDir is the analogous default for help.
const DefaultHelpDir = "./config/help"

// SystemSubdir is the dirname under <seedDir> that holds the system
// tier's per-kind subdirectories. Sty_8868eaf4 follow-up: separating
// system-tier directories from project-tier ones (which live under
// <seedDir>/proj_*/) keeps the two surfaces visually distinct and
// removes any chance of a future kind name colliding with a
// project_id prefix.
const SystemSubdir = "system"

// ResolveSeedDir returns the absolute path of the seed directory,
// honouring SATELLITES_SEED_DIR.
func ResolveSeedDir() string {
	if v := strings.TrimSpace(os.Getenv(SeedDirEnv)); v != "" {
		return v
	}
	return DefaultSeedDir
}

// ResolveHelpDir returns the absolute path of the help directory,
// honouring SATELLITES_HELP_DIR.
func ResolveHelpDir() string {
	if v := strings.TrimSpace(os.Getenv(HelpDirEnv)); v != "" {
		return v
	}
	return DefaultHelpDir
}

// Run executes the system-tier seed: agents + contracts + workflows,
// then a fourth phase loads configurations (story_764726d3) which
// reference contracts/skills/principles by name and need the prior
// phases' IDs to resolve. Help is run via RunHelp. Returns a Summary
// with the per-pass counts. Errors per file are surfaced via
// Summary.Errors; the function returns a non-nil error only on a
// structural failure (e.g. missing root dir when the caller demands
// strict mode).
func Run(ctx context.Context, docs document.Store, seedDir, workspaceID, actor string, now time.Time) (Summary, error) {
	if docs == nil {
		return Summary{}, fmt.Errorf("configseed: doc store is nil")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}
	systemRoot := filepath.Join(seedDir, SystemSubdir)
	summary := Summary{}
	for _, kind := range []Kind{KindAgent, KindContract, KindWorkflow, KindStoryTemplate, KindReplicateVocabulary, KindArtifact} {
		inputs, errs := LoadDir(systemRoot, kind, workspaceID, actor)
		summary.Errors = append(summary.Errors, errs...)
		for _, in := range inputs {
			summary.Loaded++
			res, err := docs.Upsert(ctx, in, now)
			if err != nil {
				summary.Errors = append(summary.Errors, ErrorEntry{
					Path:   string(kind) + "/" + in.Name,
					Reason: err.Error(),
				})
				continue
			}
			switch {
			case res.Created:
				summary.Created++
			case res.Changed:
				summary.Updated++
			default:
				summary.Skipped++
			}
		}
	}
	// Principle phase — runs after the agents/contracts/workflows main
	// loop. Principles have no refs of their own; the standard
	// LoadDir+Upsert path carries the work. story_ac3dc4d0.
	prSummary := runPrinciplePhase(ctx, docs, seedDir, workspaceID, actor, now)
	summary.Add(prSummary)
	// sty_08fc8d20 — WS event-kind registry lint. Walks
	// internal/wshandler/translate.go and asserts every emit literal
	// participates in internal/wsbus's registry. Runs at the end of
	// the system-tier pass; the lint depends on no seed contents (it
	// reads the source tree, not the document store), so its position
	// in the runner is governed only by the principle "fail boot
	// before substrate emits a frame a panel cannot route".
	if lintErrs := lintEventKinds(); len(lintErrs) > 0 {
		summary.Errors = append(summary.Errors, lintErrs...)
	}
	return summary, nil
}

// runPrinciplePhase loads `principles/*.md` and upserts each as a
// scope=system type=principle document. Mirrors the
// agents/contracts/workflows path through LoadDir+docs.Upsert; uses a
// dedicated function (rather than appending KindPrinciple to the main
// kinds slice) so the call site in Run sequences principles
// explicitly between the main loop and runConfigurationPhase.
// story_ac3dc4d0.
func runPrinciplePhase(ctx context.Context, docs document.Store, seedDir, workspaceID, actor string, now time.Time) Summary {
	summary := Summary{}
	inputs, errs := LoadDir(filepath.Join(seedDir, SystemSubdir), KindPrinciple, workspaceID, actor)
	summary.Errors = append(summary.Errors, errs...)
	for _, in := range inputs {
		summary.Loaded++
		res, err := docs.Upsert(ctx, in, now)
		if err != nil {
			summary.Errors = append(summary.Errors, ErrorEntry{
				Path:   string(KindPrinciple) + "/" + in.Name,
				Reason: err.Error(),
			})
			continue
		}
		switch {
		case res.Created:
			summary.Created++
		case res.Changed:
			summary.Updated++
		default:
			summary.Skipped++
		}
	}
	return summary
}

// RunWorkspace loads <seedDir>/<workspaceID>/<kind>/*.md and upserts each
// as a scope=workspace document stamped with the supplied workspaceID
// and project_id=nil. Mirrors RunProject's structure but at one tier
// up: the path layout is workspace-only, never enumerating proj_*
// children (those are handled by RunProject in a separate pass).
//
// projectKind subdirectories (proj_*) directly under the workspace dir
// are skipped — discovery here is by kind subdirectory name only, and
// the workspaceKinds list excludes the project-tier prefix.
//
// Idempotent on body hash via docs.Upsert. Missing workspace dir is
// not an error (the caller may discover via DiscoverWorkspaceDirs and
// invoke per entry; cold-boot tests pass an empty seed dir).
//
// Sty_92271886.
func RunWorkspace(ctx context.Context, docs document.Store, seedDir, workspaceID, actor string, now time.Time) (Summary, error) {
	if docs == nil {
		return Summary{}, fmt.Errorf("configseed: doc store is nil")
	}
	if workspaceID == "" {
		return Summary{}, fmt.Errorf("configseed: workspace_id required")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}
	workspaceRoot := filepath.Join(seedDir, workspaceID)
	if _, err := os.Stat(workspaceRoot); err != nil {
		if os.IsNotExist(err) {
			return Summary{}, nil
		}
		return Summary{}, fmt.Errorf("configseed: stat workspace seed dir: %w", err)
	}

	summary := Summary{}
	for _, kind := range workspaceKinds {
		inputs, errs := LoadDir(workspaceRoot, kind, workspaceID, actor)
		summary.Errors = append(summary.Errors, prefixedErrors(errs, workspaceID)...)
		for _, in := range inputs {
			// Per-kind parsers stamp scope=system + WorkspaceID="" by
			// default (sty_e2512dbd). Re-target to the workspace tier
			// here so the same parsers cover system, workspace, and
			// project surfaces without per-tier duplicates.
			in.Scope = document.ScopeWorkspace
			in.WorkspaceID = workspaceID
			in.ProjectID = nil
			summary.Loaded++
			res, err := docs.Upsert(ctx, in, now)
			if err != nil {
				summary.Errors = append(summary.Errors, ErrorEntry{
					Path:   workspaceID + "/" + string(kind) + "/" + in.Name,
					Reason: err.Error(),
				})
				continue
			}
			switch {
			case res.Created:
				summary.Created++
			case res.Changed:
				summary.Updated++
			default:
				summary.Skipped++
			}
		}
	}
	return summary, nil
}

// RunHelp executes the help-tier seed: walks helpDir/*.md and upserts
// each as a type=help document. story_cc5c67a9.
func RunHelp(ctx context.Context, docs document.Store, helpDir, workspaceID, actor string, now time.Time) (Summary, error) {
	if docs == nil {
		return Summary{}, fmt.Errorf("configseed: doc store is nil")
	}
	if helpDir == "" {
		helpDir = DefaultHelpDir
	}
	summary := Summary{}
	entries, err := os.ReadDir(helpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return summary, nil
		}
		return summary, fmt.Errorf("configseed: read help dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(helpDir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			summary.Errors = append(summary.Errors, ErrorEntry{Path: path, Reason: fmt.Sprintf("read: %v", err)})
			continue
		}
		fm, body, err := Parse(content)
		if err != nil {
			summary.Errors = append(summary.Errors, ErrorEntry{Path: path, Reason: fmt.Sprintf("parse: %v", err)})
			continue
		}
		input, err := helpToInput(fm, body, workspaceID, actor)
		if err != nil {
			summary.Errors = append(summary.Errors, ErrorEntry{Path: path, Reason: err.Error()})
			continue
		}
		summary.Loaded++
		res, err := docs.Upsert(ctx, input, now)
		if err != nil {
			summary.Errors = append(summary.Errors, ErrorEntry{Path: path, Reason: err.Error()})
			continue
		}
		switch {
		case res.Created:
			summary.Created++
		case res.Changed:
			summary.Updated++
		default:
			summary.Skipped++
		}
	}
	return summary, nil
}

// RunAll runs Run + RunHelp and merges the summaries. Convenience
// entry point for the boot wiring + system_seed_run MCP verb.
func RunAll(ctx context.Context, docs document.Store, seedDir, helpDir, workspaceID, actor string, now time.Time) (Summary, error) {
	combined := Summary{}
	sys, err := Run(ctx, docs, seedDir, workspaceID, actor, now)
	if err != nil {
		return combined, err
	}
	combined.Add(sys)
	help, err := RunHelp(ctx, docs, helpDir, workspaceID, actor, now)
	if err != nil {
		return combined, err
	}
	combined.Add(help)
	return combined, nil
}
