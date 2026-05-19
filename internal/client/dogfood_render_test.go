//go:build dogfood

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
)

// TestDogfoodRender is the AC7 evidence capture: runs the renderer
// against the unit-test fixture and writes the rendered markdown plus
// shape metrics (bytes / sha256 / header excerpt) to /tmp so the
// closer can paste the values into the develop evidence row.
//
// Build-tag-gated: this test only runs under `go test -tags dogfood`
// — the default suite must not depend on filesystem side-effects.
func TestDogfoodRender(t *testing.T) {
	fx := newRenderFixture(t)
	out, err := fx.c.RenderTaskPrompt(context.Background(), fx.caller, RenderTaskPromptInput{
		TaskID:   fx.taskID,
		Action:   "contract:develop",
		StoryID:  fx.storyID,
		WorkBody: "Implement sty_72e36256 per the develop contract.",
		Now:      fx.now,
	})
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	sum := sha256.Sum256([]byte(out))
	excerpt := out
	if len(excerpt) > 220 {
		excerpt = excerpt[:220]
	}
	t.Logf("rendered_bytes=%d sha256=%s", len(out), hex.EncodeToString(sum[:]))
	t.Logf("header_excerpt=%q", excerpt)
	if err := os.WriteFile("/tmp/dogfood_render.md", []byte(out), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = fmt.Sprintf // keep fmt import alive in case logger changes.
}
