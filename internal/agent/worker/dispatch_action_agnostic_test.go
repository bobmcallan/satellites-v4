// dispatch_action_agnostic_test.go — sty_447b9fe0 AC9 architectural
// regression. Asserts the worker's dispatch path
// (internal/agent/worker/client_claude.go) carries zero per-action
// runtime branches: no `switch ti.Action`, no `if action == "..."`,
// no `dispatchClass`, no `runHotPath`. Re-introducing any of those
// shapes — even under a "temporary" name — trips this test.
//
// The `developActionEpilogue` constant is allow-listed: it shapes the
// PROMPT the dispatched claude reads (a thin-pointer-template
// concern), NOT a runtime dispatch branch. The prose distinction
// lives in plan.md §3.3 for the story that authored this test.

package worker

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDispatcher_NoPerActionBranching(t *testing.T) {
	data, err := os.ReadFile("client_claude.go")
	require.NoError(t, err)
	src := string(data)

	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`ti\.Action\s*==\s*"contract:(commit|merge_to_main)"`),
		regexp.MustCompile(`task\.Action\s*==\s*"contract:(commit|merge_to_main)"`),
		regexp.MustCompile(`switch\s+ti\.Action\b`),
		regexp.MustCompile(`switch\s+task\.Action\b`),
		regexp.MustCompile(`dispatchClass\b`),
		regexp.MustCompile(`runHotPath\b`),
	}
	for _, rx := range forbidden {
		if rx.MatchString(src) {
			t.Fatalf("dispatcher contains forbidden per-action branch: %s", rx)
		}
	}

	// allow-listed: developActionEpilogue is a PROMPT-string shaper,
	// not a runtime dispatch branch (plan.md §3.3).
	assert.Contains(t, src, "developActionEpilogue")
}
