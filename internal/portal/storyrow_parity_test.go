// sty_de9f10f9 + sty_bac14f30 — assert that the templated story-row
// in _panel_stories.html and the JS-built row in
// pages/static/common.js:_appendStoryRow render the SAME ordered
// list of <td class="…"> cells.
//
// Both sides are derived from source on every run, NOT from
// snapshots — so future drift fails loud without a snapshot
// refresh ritual.
//
// History: sty_de9f10f9 landed this test xfail-style with a known
// col-select divergence on the JS-append path. sty_bac14f30
// closed the divergence by adding col-select to _appendStoryRow;
// this test now asserts strict ordered equality.
package portal

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bobmcallan/satellites/pages"
)

func TestStoryRow_SSR_JS_ColumnParity(t *testing.T) {
	t.Parallel()

	ssrCells := renderStoryRowSSRCells(t)
	jsCells := extractAppendStoryRowJSCells(t)

	if len(ssrCells) != len(jsCells) {
		t.Fatalf(
			"story-row SSR/JS column count drift.\n"+
				"  ssr cells: %v (%d)\n"+
				"  js  cells: %v (%d)",
			ssrCells, len(ssrCells), jsCells, len(jsCells),
		)
	}
	for i := range ssrCells {
		if ssrCells[i] != jsCells[i] {
			t.Fatalf(
				"story-row SSR/JS column drift at index %d.\n"+
					"  ssr cells: %v\n"+
					"  js  cells: %v",
				i, ssrCells, jsCells,
			)
		}
	}
}

// renderStoryRowSSRCells executes the real _panel_stories.html
// template via the same pages.Templates() entrypoint the portal
// uses, with one synthesised storyCard so `range .Composite.Stories`
// produces exactly one tr.story-row. Returns the ordered class
// attribute of every <td> inside that row (NOT the detail row).
func renderStoryRowSSRCells(t *testing.T) []string {
	t.Helper()
	tmpl, err := pages.Templates()
	if err != nil {
		t.Fatalf("pages.Templates: %v", err)
	}

	composite := projectWorkspaceComposite{
		Stories: []storyCard{{
			ID:                 "sty_parity",
			ProjectID:          "proj_parity",
			Title:              "parity-probe",
			Status:             "backlog",
			Priority:           "high",
			Category:           "feature",
			Tags:               []string{"area:parity"},
			CreatedAt:          "2026-01-01T00:00:00Z",
			UpdatedAt:          "2026-01-01T00:00:00Z",
			Description:        "probe",
			AcceptanceCriteria: "probe",
		}},
	}
	data := map[string]any{"Composite": composite}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "_panel_stories.html", data); err != nil {
		t.Fatalf("execute _panel_stories.html: %v", err)
	}

	rendered := buf.String()
	rowOpen := strings.Index(rendered, `<tr class="story-row"`)
	if rowOpen < 0 {
		t.Fatalf("rendered template has no <tr class=\"story-row\">; rendered=%s", rendered)
	}
	rowClose := strings.Index(rendered[rowOpen:], "</tr>")
	if rowClose < 0 {
		t.Fatalf("rendered story-row has no </tr>")
	}
	rowSlice := rendered[rowOpen : rowOpen+rowClose]
	return scanTDClasses(rowSlice)
}

// extractAppendStoryRowJSCells reads pages/static/common.js, locates
// the _appendStoryRow function body, and scans the row.innerHTML
// concatenation for ordered <td class="…"> literals.
func extractAppendStoryRowJSCells(t *testing.T) []string {
	t.Helper()
	source := readCommonJS(t)
	body := extractJSFunctionBody(t, source, "_appendStoryRow(")
	// Restrict to the row.innerHTML assignment. The detail row
	// builder uses detail.innerHTML which we explicitly want to
	// skip — the SSR comparison is row-only.
	rowInner := isolateInnerHTMLAssignment(t, body, "row.innerHTML")
	return scanTDClasses(rowInner)
}

// scanTDClasses returns the ordered class attribute of every
// <td class="…"> literal in the haystack. Both rendered HTML and
// the JS source use plain `class="…"` (the JS strings are single-
// quoted), so a single regex covers both sides.
func scanTDClasses(haystack string) []string {
	re := regexp.MustCompile(`<td\s+class="([^"]+)"`)
	matches := re.FindAllStringSubmatch(haystack, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, normaliseClassAttr(m[1]))
	}
	return out
}

// normaliseClassAttr keeps only the first space-separated token —
// the column-identifying class. Avoids tripping on cells that pile
// modifiers like `col-updated muted`.
func normaliseClassAttr(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, " \t"); idx > 0 {
		return s[:idx]
	}
	return s
}

// extractJSFunctionBody finds the method-shorthand DEFINITION of
// the named function (lines like `<name>(args) {`) — NOT a call
// site like `this.<name>(...)`. Returns everything from the opening
// `{` to its matched closing `}`. Robust to nested braces.
func extractJSFunctionBody(t *testing.T, source, name string) string {
	t.Helper()
	// Match a definition: start-of-line (after optional whitespace),
	// the bare name, the opening paren, the args, the closing paren,
	// optional whitespace, then `{`. The leading anchor stops us
	// from picking up `this.<name>(` call sites.
	pat := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(strings.TrimSuffix(name, "(")) + `\([^)]*\)\s*{`)
	loc := pat.FindStringIndex(source)
	if loc == nil {
		t.Fatalf("function definition for %q not found in common.js", name)
	}
	start := loc[1] - 1 // index of the opening `{`
	depth := 0
	for i := start; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start : i+1]
			}
		}
	}
	t.Fatalf("matched closing brace not found for %q", name)
	return ""
}

// isolateInnerHTMLAssignment returns the substring of body
// containing the assignment to <target> = '…' + '…' + … ; up to
// the terminating semicolon. Subsequent .innerHTML assignments are
// excluded so the story-row scan doesn't pick up detail-row cells.
func isolateInnerHTMLAssignment(t *testing.T, body, target string) string {
	t.Helper()
	idx := strings.Index(body, target)
	if idx < 0 {
		t.Fatalf("%s assignment not found in function body", target)
	}
	end := strings.Index(body[idx:], ";")
	if end < 0 {
		t.Fatalf("%s assignment has no terminating semicolon", target)
	}
	return body[idx : idx+end]
}

// readCommonJS reads pages/static/common.js relative to the repo
// root (walking up from the test file's package path).
func readCommonJS(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/portal → repo root is two parents up.
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	path := filepath.Join(root, "pages", "static", "common.js")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read common.js (%s): %v", path, err)
	}
	return string(raw)
}

// equalStringSlices compares two slices treating nil and empty as
// equal. Order-insensitive (callers pass already-sorted slices).
func equalStringSlices(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
