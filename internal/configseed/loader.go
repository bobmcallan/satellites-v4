package configseed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bobmcallan/satellites/internal/document"
	"github.com/bobmcallan/satellites/internal/kvtemplate"
)

// LoadDir walks dir/<kindSubdir>/*.md, parses each file, and returns
// the upsert inputs ready for Run to apply. Errors per file go into
// the second return; one bad file does not abort the others. Caller
// supplies workspaceID + actor for the resulting Documents.
//
// LoadDir renders no templates. Files with `template: true` in
// frontmatter are loaded with their literal body. To enable
// {{key}} substitution, call LoadDirWithResolver instead.
func LoadDir(rootDir string, kind Kind, workspaceID, actor string) ([]document.UpsertInput, []ErrorEntry) {
	return LoadDirWithResolver(context.Background(), rootDir, kind, workspaceID, actor, nil)
}

// LoadDirWithResolver is the templated-load variant. When resolver is
// non-nil and a file's frontmatter sets `template: true`, the body is
// rendered through kvtemplate.Render before buildInput. Unresolved keys
// produce an ErrorEntry naming the file and the missing keys, and the
// file is skipped (per pr_evidence — fail loud, not silent). story_6593bb8c.
func LoadDirWithResolver(ctx context.Context, rootDir string, kind Kind, workspaceID, actor string, resolver kvtemplate.Resolver) ([]document.UpsertInput, []ErrorEntry) {
	subdir := filepath.Join(rootDir, kindSubdir(kind))
	entries, err := os.ReadDir(subdir)
	if err != nil {
		// Missing subdir is fine — the seed simply has no files of
		// this kind today. Other errors surface as one entry.
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []ErrorEntry{{Path: subdir, Reason: fmt.Sprintf("read dir: %v", err)}}
	}
	// Stable order for deterministic boot logs / tests.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	out := make([]document.UpsertInput, 0, len(entries))
	errs := make([]ErrorEntry, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(subdir, entry.Name())
		content, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, ErrorEntry{Path: path, Reason: fmt.Sprintf("read: %v", err)})
			continue
		}
		fm, body, err := Parse(content)
		if err != nil {
			errs = append(errs, ErrorEntry{Path: path, Reason: fmt.Sprintf("parse: %v", err)})
			continue
		}
		if fm.Bool("template") && resolver != nil {
			rendered, rerr := kvtemplate.Render(ctx, string(body), resolver)
			if rerr != nil {
				errs = append(errs, ErrorEntry{Path: path, Reason: fmt.Sprintf("template render: %v", rerr)})
				continue
			}
			if len(rendered.Unresolved) > 0 {
				errs = append(errs, ErrorEntry{Path: path, Reason: fmt.Sprintf("template unresolved keys: %s (searched system,workspace,project,user tiers)", strings.Join(rendered.Unresolved, ","))})
				continue
			}
			body = []byte(rendered.Text)
		}
		input, err := buildInput(kind, fm, body, workspaceID, actor)
		if err != nil {
			errs = append(errs, ErrorEntry{Path: path, Reason: err.Error()})
			continue
		}
		out = append(out, input)
	}
	return out, errs
}

// kindSubdir returns the canonical subdirectory name for a kind.
// The plural form mirrors the directory layout users expect.
func kindSubdir(kind Kind) string {
	switch kind {
	case KindAgent:
		return "agents"
	case KindContract:
		return "contracts"
	case KindWorkflow:
		return "workflows"
	case KindPrinciple:
		return "principles"
	case KindStoryTemplate:
		return "story_templates"
	case KindReplicateVocabulary:
		return "replicate_vocabulary"
	case KindArtifact:
		return "artifacts"
	case KindSkill:
		return "skills"
	case KindReviewer:
		return "reviewers"
	case KindHelp:
		// Help docs live at the seed-dir root rather than under a
		// subdirectory — see HelpDir wiring in runner.go.
		return ""
	}
	return string(kind)
}

// buildInput dispatches to the per-kind parser.
func buildInput(kind Kind, fm Frontmatter, body []byte, workspaceID, actor string) (document.UpsertInput, error) {
	switch kind {
	case KindAgent:
		return agentToInput(fm, body, workspaceID, actor)
	case KindContract:
		return contractToInput(fm, body, workspaceID, actor)
	case KindWorkflow:
		return workflowToInput(fm, body, workspaceID, actor)
	case KindPrinciple:
		return principleToInput(fm, body, workspaceID, actor)
	case KindStoryTemplate:
		return storyTemplateToInput(fm, body, workspaceID, actor)
	case KindReplicateVocabulary:
		return replicateVocabularyToInput(fm, body, workspaceID, actor)
	case KindArtifact:
		return artifactToInput(fm, body, workspaceID, actor)
	case KindSkill:
		return skillToInput(fm, body, workspaceID, actor)
	case KindReviewer:
		return reviewerToInput(fm, body, workspaceID, actor)
	case KindHelp:
		return helpToInput(fm, body, workspaceID, actor)
	}
	return document.UpsertInput{}, fmt.Errorf("configseed: unknown kind %q", kind)
}
