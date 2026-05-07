package configseed

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSeedNameConvention (sty_94c54229) walks config/seed/ and asserts
// the plain-slug rule for every .md file's frontmatter `name:`:
//
//   - name == basename(file) minus ".md", and
//   - name matches ^[a-z][a-z0-9_]*$.
//
// The verb namespace already carries each document's type; the name
// must not duplicate it (no `agent_`, `pr_`, `contract_` prefixes; no
// display titles with whitespace or capitals). The matching plain-slug
// principle (`seed_name_slug`) cites this test as its regression
// anchor.
//
// Per-type carve-outs: story_template files key on `category` (the
// upsert path falls back to `category` when `name` is absent —
// internal/configseed/sweep.go:237-248). help docs live under
// config/help/ rather than config/seed/, so they do not appear here.
// Both fall back through the same key resolution as the orphan sweep
// to keep the lint and the sweep in sync.
//
// Failure mode: collects every offending file then fails once, so an
// audit shows every gap rather than aborting on the first.
func TestSeedNameConvention(t *testing.T) {
	t.Parallel()

	seedDir, err := filepath.Abs(filepath.Join("..", "..", "config", "seed"))
	if err != nil {
		t.Fatalf("abs seed dir: %v", err)
	}

	slugRE := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	type offence struct {
		path     string
		stored   string
		expected string
		reason   string
	}
	var offenders []offence

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
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			offenders = append(offenders, offence{path: path, reason: "read: " + rerr.Error()})
			return nil
		}
		fm, _, perr := Parse(content)
		if perr != nil {
			offenders = append(offenders, offence{path: path, reason: "parse: " + perr.Error()})
			return nil
		}
		// Resolve the canonical name the same way the sweep does:
		// prefer `name`, fall back to `category` (story_template), then
		// `slug` (help — present here only for symmetry).
		stored := fm.String("name")
		if stored == "" {
			stored = fm.String("category")
		}
		if stored == "" {
			stored = fm.String("slug")
		}
		expected := strings.TrimSuffix(info.Name(), ".md")
		if stored != expected {
			offenders = append(offenders, offence{
				path:     path,
				stored:   stored,
				expected: expected,
				reason:   "basename mismatch",
			})
		}
		if stored != "" && !slugRE.MatchString(stored) {
			offenders = append(offenders, offence{
				path:     path,
				stored:   stored,
				expected: expected,
				reason:   "not a plain slug (regex ^[a-z][a-z0-9_]*$)",
			})
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", seedDir, walkErr)
	}

	for _, o := range offenders {
		t.Errorf("seed name violation: path=%s stored=%q expected=%q reason=%s",
			o.path, o.stored, o.expected, o.reason)
	}
}
