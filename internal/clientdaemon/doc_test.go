package clientdaemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPackageDocRefusal (sty_5aa20f1b AC10) asserts the package doc
// in doc.go cites pr_substrate_model + the three prohibitions + the
// worked failure mode.
func TestPackageDocRefusal(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	docPath := filepath.Join(filepath.Dir(here), "doc.go")
	body, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read doc.go: %v", err)
	}
	src := string(body)
	required := []string{
		"pr_substrate_model",
		"sty_4db0e025",
		"server-side claude hosting",
		"subagent harness",
		"LLM behaviour",
	}
	for _, want := range required {
		if !strings.Contains(src, want) {
			t.Errorf("doc.go missing required citation %q", want)
		}
	}
}
