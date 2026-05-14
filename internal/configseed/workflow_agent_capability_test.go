package configseed

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestWorkflowAgentCapabilities (sty_8f99ef39) closes the configuration
// gap that lets a workflow doc reference an agent_id whose seed
// frontmatter does not advertise the matching capability. Without this
// lint a workflow like default.md can name `agent_id: story_reviewer`
// for a kind:work contract:story_review slot while `story_reviewer.md`
// carries the action only under `reviews:` (or omits it entirely) — and
// the bug only surfaces at runtime when the orchestrator's task_add call
// hits `agent_cannot_deliver` (internal/client/task.go:390).
//
// Shape mirrors TestSeedNameConvention in name_convention_test.go:
//
//  1. Walk config/seed/ and collect every agent name -> frontmatter map.
//  2. Walk config/seed/**/workflows/*.md, extract (kind, action, agent_id)
//     triples from each `kind: …, action: …, agent_id: …` body line.
//  3. For kind=work, assert action is in the agent's `delivers:` list.
//     For kind=review, assert action is in the agent's `reviews:` list.
//
// Failure mode: collect every offence then fail once with all gaps named,
// so a single audit run shows the full surface area (same pattern as
// the seed-name lint).
func TestWorkflowAgentCapabilities(t *testing.T) {
	t.Parallel()

	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}

	agents := loadAgentFrontmatters(t, seedDir)

	// Body-line regex: matches the workflow's task-list entries, shape
	//   `{kind: work, action: contract:plan, agent_id: developer_agent}`
	// or comma-separated variants. The three keys can appear in any
	// order but each on the same line (the workflow doc's convention).
	kindRE := regexp.MustCompile(`\bkind:\s*([a-z_]+)`)
	actionRE := regexp.MustCompile(`\baction:\s*([a-zA-Z0-9_:]+)`)
	agentRE := regexp.MustCompile(`\bagent_id:\s*([a-z][a-z0-9_]*)`)

	type offence struct {
		workflow string
		line     int
		kind     string
		action   string
		agentID  string
		reason   string
	}
	var offenders []offence

	workflowGlob := filepath.Join(seedDir, "*", "workflows", "*.md")
	matches, err := filepath.Glob(workflowGlob)
	if err != nil {
		t.Fatalf("glob workflows %s: %v", workflowGlob, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no workflow files matched %s — lint cannot validate", workflowGlob)
	}

	for _, wf := range matches {
		content, rerr := os.ReadFile(wf)
		if rerr != nil {
			t.Fatalf("read %s: %v", wf, rerr)
		}
		_, body, perr := Parse(content)
		if perr != nil {
			t.Fatalf("parse %s: %v", wf, perr)
		}

		lines := strings.Split(string(body), "\n")
		for i, ln := range lines {
			km := kindRE.FindStringSubmatch(ln)
			am := actionRE.FindStringSubmatch(ln)
			gm := agentRE.FindStringSubmatch(ln)
			if km == nil || am == nil || gm == nil {
				continue
			}
			kind := km[1]
			action := am[1]
			agentID := gm[1]
			if !strings.HasPrefix(action, "contract:") {
				// Workflow docs name actions in canonical `contract:<name>`
				// form; anything else on a triple line is shape drift the
				// lint should surface for the author.
				offenders = append(offenders, offence{
					workflow: wf,
					line:     i + 1,
					kind:     kind,
					action:   action,
					agentID:  agentID,
					reason:   "action not in canonical `contract:<name>` form",
				})
				continue
			}

			fm, ok := agents[agentID]
			if !ok {
				offenders = append(offenders, offence{
					workflow: wf,
					line:     i + 1,
					kind:     kind,
					action:   action,
					agentID:  agentID,
					reason:   "agent_id has no agent seed file under config/seed/",
				})
				continue
			}

			switch kind {
			case "work":
				if !containsString(fm.StringSlice("delivers"), action) {
					offenders = append(offenders, offence{
						workflow: wf,
						line:     i + 1,
						kind:     kind,
						action:   action,
						agentID:  agentID,
						reason:   "agent's frontmatter `delivers:` does not list this action",
					})
				}
			case "review":
				if !containsString(fm.StringSlice("reviews"), action) {
					offenders = append(offenders, offence{
						workflow: wf,
						line:     i + 1,
						kind:     kind,
						action:   action,
						agentID:  agentID,
						reason:   "agent's frontmatter `reviews:` does not list this action",
					})
				}
			default:
				offenders = append(offenders, offence{
					workflow: wf,
					line:     i + 1,
					kind:     kind,
					action:   action,
					agentID:  agentID,
					reason:   "unknown kind (expected `work` or `review`)",
				})
			}
		}
	}

	for _, o := range offenders {
		t.Errorf("workflow %s:%d kind=%s action=%s agent_id=%s — %s",
			o.workflow, o.line, o.kind, o.action, o.agentID, o.reason)
	}
}

// loadAgentFrontmatters returns a map of agent name -> parsed
// frontmatter, indexed by the frontmatter's `name:` slug for every
// type=agent seed file under seedDir. Failing to parse any agent file
// fails the test outright — the lint cannot validate workflow
// references against an incomplete agent index.
func loadAgentFrontmatters(t *testing.T, seedDir string) map[string]Frontmatter {
	t.Helper()
	out := map[string]Frontmatter{}
	walkErr := filepath.Walk(seedDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".md") {
			return nil
		}
		// Only agent docs live under .../agents/<name>.md in the seed
		// tree; the lint uses the directory anchor rather than parsing
		// every md file to keep scope tight.
		if !strings.Contains(filepath.ToSlash(path), "/agents/") {
			return nil
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read agent %s: %v", path, rerr)
		}
		fm, _, perr := Parse(content)
		if perr != nil {
			t.Fatalf("parse agent %s: %v", path, perr)
		}
		name := fm.String("name")
		if name == "" {
			t.Fatalf("agent %s: frontmatter missing required `name:` key", path)
		}
		out[name] = fm
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk seed dir %s: %v", seedDir, walkErr)
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
