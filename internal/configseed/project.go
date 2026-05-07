// Project-scoped seed loader (sty_8868eaf4 → sty_87e203c1). Parallel
// to Run, but the produced documents are scope=project,
// project_id=<resolved>. Layout:
//
//	<seedDir>/<workspace_id>/<project_id>/<kind>/*.md
//
// The workspace prefix mirrors the (workspace_id, project_id) primary
// key on every project-scope row. Sty_87e203c1 introduced the prefix;
// before that, project seeds lived at <seedDir>/<project_id>/. Two
// workspaces could in principle issue projects whose dirs would clash
// on disk under the older shape, even though the rows are isolated by
// workspace_id at the substrate layer.
//
// System tier stays at <seedDir>/system/<kind>/ — system rows are
// global-by-design (the workspace stamp at runtime is bookkeeping, not
// identity), so the path is workspace-less.
//
// Discovery is two-pass: enumerate <seedDir>/wksp_*/ entries, then
// proj_* entries within each. The loader runs once per discovered pair
// at boot (as a goroutine, non-blocking) and on demand via the
// project_seed_run MCP verb.
//
// Strict isolation: this loader produces only scope=project rows. The
// document store's Validate path rejects scope=project documents that
// lack a project_id, and rejects scope=system documents that carry a
// project_id, so a misuse here surfaces as a write-time error rather
// than silent cross-tier contamination. configseed.Run produces only
// scope=system rows; the two paths never overlap.
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

// ProjectDirPrefix is the canonical prefix every project-id directory
// carries under <seedDir>/<workspace_id>/. Discovery uses it to
// distinguish project dirs from any other entries a future feature
// might place under a workspace dir.
const ProjectDirPrefix = "proj_"

// WorkspaceDirPrefix is the canonical prefix every workspace directory
// carries directly under <seedDir>. Discovery uses it to enumerate
// workspaces before walking project_ids beneath them.
const WorkspaceDirPrefix = "wksp_"

// DiscoveredProject pairs the workspace_id and project_id parsed from
// a <seedDir>/wksp_*/proj_*/ path. Discovery returns these so callers
// can either run the loader directly with both ids in hand or feed the
// project_id into the project_seed_run verb (which independently
// resolves the canonical workspace from the project store).
type DiscoveredProject struct {
	WorkspaceID string
	ProjectID   string
}

// projectKinds is the ordered list of Kinds the project loader walks
// per project_id directory. Mirrors the system loader's list so the
// directory convention is symmetric: any kind that exists at system
// scope can also be seeded at project scope by dropping a file under
// <seedDir>/<workspace_id>/<project_id>/<kind>/. An empty subdir (or
// one missing on disk) loads zero rows for that kind — no error, no
// warning.
//
// Kinds that don't make sense at project scope (story_template,
// replicate_vocabulary as currently shipped) are still walked here,
// but the on-disk seed is expected to be empty. If a future story
// excludes a kind from the project tier, drop it from this list and
// document the choice on the change.
var projectKinds = []Kind{
	KindAgent,
	KindContract,
	KindWorkflow,
	KindStoryTemplate,
	KindReplicateVocabulary,
	KindArtifact,
	KindPrinciple,
}

// DiscoverProjectDirs walks <seedDir>/wksp_*/proj_*/ and returns the
// (workspace_id, project_id) pairs found on disk, sorted by
// workspace_id then project_id. Returns nil + nil when the seed dir is
// missing (cold-boot test fixture). A workspace dir with no proj_*
// children contributes zero rows and is not an error. Per-workspace
// read failures are logged via the returned error slice; callers
// typically log and proceed.
//
// The system tier (<seedDir>/system/) is excluded by the wksp_ prefix
// filter — discovery only cares about workspace-scoped subtrees.
func DiscoverProjectDirs(seedDir string) ([]DiscoveredProject, error) {
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}
	entries, err := os.ReadDir(seedDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("configseed: read seed dir for project discovery: %w", err)
	}
	var out []DiscoveredProject
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wsName := e.Name()
		if !strings.HasPrefix(wsName, WorkspaceDirPrefix) {
			continue
		}
		wsRoot := filepath.Join(seedDir, wsName)
		projEntries, err := os.ReadDir(wsRoot)
		if err != nil {
			return out, fmt.Errorf("configseed: read workspace dir %s: %w", wsName, err)
		}
		for _, pe := range projEntries {
			if !pe.IsDir() {
				continue
			}
			pName := pe.Name()
			if !strings.HasPrefix(pName, ProjectDirPrefix) {
				continue
			}
			out = append(out, DiscoveredProject{WorkspaceID: wsName, ProjectID: pName})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkspaceID != out[j].WorkspaceID {
			return out[i].WorkspaceID < out[j].WorkspaceID
		}
		return out[i].ProjectID < out[j].ProjectID
	})
	return out, nil
}

// RunProject loads <seedDir>/<workspaceID>/<projectID>/<kind>/*.md and
// upserts each as a scope=project, project_id=projectID document.
// workspaceID is stamped on every produced row AND used as a path
// component, so workspace-scoping is consistent on disk and at the row
// layer. Idempotent on body hash via docs.Upsert.
//
// projectID + workspaceID resolution is the caller's responsibility —
// the loader trusts the supplied ids. When invoked from the boot path,
// main.go enumerates (workspace_id, project_id) pairs from disk and
// hands them to RunProjectSeed, which independently looks up the
// project's canonical workspace from the project store before reading
// the seed dir.
func RunProject(ctx context.Context, docs document.Store, seedDir, projectID, workspaceID, actor string, now time.Time) (Summary, error) {
	if docs == nil {
		return Summary{}, fmt.Errorf("configseed: doc store is nil")
	}
	if projectID == "" {
		return Summary{}, fmt.Errorf("configseed: project_id required")
	}
	if workspaceID == "" {
		return Summary{}, fmt.Errorf("configseed: workspace_id required")
	}
	if seedDir == "" {
		seedDir = DefaultSeedDir
	}
	projectRoot := filepath.Join(seedDir, workspaceID, projectID)
	if _, err := os.Stat(projectRoot); err != nil {
		if os.IsNotExist(err) {
			return Summary{}, nil
		}
		return Summary{}, fmt.Errorf("configseed: stat project seed dir: %w", err)
	}

	pid := projectID
	summary := Summary{}
	for _, kind := range projectKinds {
		inputs, errs := LoadDir(projectRoot, kind, workspaceID, actor)
		summary.Errors = append(summary.Errors, prefixedErrors(errs, projectID)...)
		for _, in := range inputs {
			// Override scope + project_id + workspace_id. The per-kind
			// parsers stamp scope=system + WorkspaceID="" unconditionally
			// (sty_e2512dbd: system tier is non-tenant); we re-target
			// the input to the project tier here so the same parsers +
			// frontmatter validation cover both surfaces.
			in.Scope = document.ScopeProject
			in.ProjectID = &pid
			in.WorkspaceID = workspaceID
			summary.Loaded++
			res, err := docs.Upsert(ctx, in, now)
			if err != nil {
				summary.Errors = append(summary.Errors, ErrorEntry{
					Path:   projectID + "/" + string(kind) + "/" + in.Name,
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

// prefixedErrors prepends the project_id to each ErrorEntry's Path so
// boot logs identify which project's seed produced the failure.
func prefixedErrors(errs []ErrorEntry, projectID string) []ErrorEntry {
	if len(errs) == 0 {
		return nil
	}
	out := make([]ErrorEntry, len(errs))
	for i, e := range errs {
		out[i] = ErrorEntry{
			Path:   projectID + "/" + e.Path,
			Reason: e.Reason,
		}
	}
	return out
}
