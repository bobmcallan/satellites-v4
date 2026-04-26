package portal

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPortalCSS_HasFontSizeControlToken covers AC1: --font-size-control
// (and the matching padding tokens) live in :root of portal.css.
// story_2469358b.
func TestPortalCSS_HasFontSizeControlToken(t *testing.T) {
	t.Parallel()
	src := readCSS(t)
	rootIdx := strings.Index(src, ":root {")
	if rootIdx < 0 {
		t.Fatalf(":root rule missing from portal.css")
	}
	rootEnd := strings.Index(src[rootIdx:], "}")
	if rootEnd < 0 {
		t.Fatalf(":root rule not closed in portal.css")
	}
	root := src[rootIdx : rootIdx+rootEnd]
	for _, token := range []string{"--font-size-control", "--control-padding-y", "--control-padding-x"} {
		if !strings.Contains(root, token) {
			t.Errorf(":root missing %s — must be defined for AC1 of story_2469358b", token)
		}
	}
}

// TestPortalCSS_ControlsUseFontSizeToken covers AC2: every control rule
// in portal.css references var(--font-size-control), not a literal.
// story_2469358b.
func TestPortalCSS_ControlsUseFontSizeToken(t *testing.T) {
	t.Parallel()
	src := readCSS(t)
	cases := []struct {
		name     string
		selector string
	}{
		{name: "input/select/textarea", selector: "input, select, textarea"},
		{name: ".btn-link", selector: ".btn-link"},
		{name: ".nav-workspace-menu a", selector: ".nav-workspace-menu a"},
		{name: ".theme-picker-btn", selector: ".theme-picker-btn"},
		{name: ".ws-debug", selector: ".ws-debug"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := ruleBody(t, src, tc.selector)
			if strings.Contains(body, "var(--font-size-control)") {
				return
			}
			if regexp.MustCompile(`font-size:\s*0\.\d+`).MatchString(body) {
				t.Errorf("%s rule still uses a literal font-size; replace with var(--font-size-control). body=%q", tc.selector, body)
			} else {
				t.Errorf("%s rule must reference var(--font-size-control); body=%q", tc.selector, body)
			}
		})
	}
}

// TestPortalTemplates_NoInlineFontSize covers AC3: no template carries
// inline `style="font-size:` (or other typography literals) on a control
// element. story_2469358b.
func TestPortalTemplates_NoInlineFontSize(t *testing.T) {
	t.Parallel()
	root := "../../pages/templates"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read templates dir: %v", err)
	}
	pattern := regexp.MustCompile(`style\s*=\s*"[^"]*font-size`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".html") {
			continue
		}
		path := filepath.Join(root, e.Name())
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if pattern.Match(body) {
			t.Errorf("%s contains inline style=\"…font-size…\" — move to portal.css and reference the typography token", path)
		}
	}
}

func readCSS(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../../pages/static/css/portal.css")
	if err != nil {
		t.Fatalf("read portal.css: %v", err)
	}
	return string(src)
}

// ruleBody returns the body of the first rule whose selector starts at
// the given exact selector text. Looks for "<selector> {" in the source
// and returns the text up to the matching "}". Treats the selector as a
// literal — pass exactly as it appears in the file.
func ruleBody(t *testing.T, src, selector string) string {
	t.Helper()
	header := selector + " {"
	idx := strings.Index(src, header)
	if idx < 0 {
		t.Fatalf("selector %q not found in portal.css", selector)
	}
	open := idx + len(header)
	end := strings.Index(src[open:], "}")
	if end < 0 {
		t.Fatalf("selector %q has no closing brace", selector)
	}
	return src[open : open+end]
}
