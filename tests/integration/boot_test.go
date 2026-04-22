package integration

import (
	"strings"
	"testing"
	"time"
)

// Smoke tests prove the two binaries compile and boot. They exist before any
// feature code (story_739ae7ef) so the local dev loop is green from the
// first feature story forward.

const bootTimeout = 10 * time.Second

func TestServerBootsWithVersionLine(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t, "satellites")
	got := runBinary(t, bin, bootTimeout)
	t.Logf("boot line: %q", got)

	if !strings.HasPrefix(got, "satellites-server ") {
		t.Fatalf("expected prefix %q, got %q", "satellites-server ", got)
	}
	for _, frag := range []string{"build:", "commit:"} {
		if !strings.Contains(got, frag) {
			t.Errorf("boot line missing %q fragment: %q", frag, got)
		}
	}
}

func TestAgentBootsWithVersionLine(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t, "satellites-agent")
	got := runBinary(t, bin, bootTimeout)
	t.Logf("boot line: %q", got)

	if !strings.HasPrefix(got, "satellites-agent ") {
		t.Fatalf("expected prefix %q, got %q", "satellites-agent ", got)
	}
	for _, frag := range []string{"build:", "commit:"} {
		if !strings.Contains(got, frag) {
			t.Errorf("boot line missing %q fragment: %q", frag, got)
		}
	}
}
