// Asserts the SurrealLiveSource subscribes to the seven panel tables
// — every per-table translator case in translate.go must have a
// matching subscription in surrealsource.go.

package wshandler

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSurrealLiveSource_SubscribesToAllPanelTables(t *testing.T) {
	want := []string{"tasks", "stories", "ledger", "documents", "repos", "commits", "projects"}
	got := PanelTables()
	require := assert.New(t)
	require.Len(got, len(want), "expected seven-table subscription set")

	wantSorted := append([]string(nil), want...)
	gotSorted := append([]string(nil), got...)
	sort.Strings(wantSorted)
	sort.Strings(gotSorted)
	require.Equal(wantSorted, gotSorted)
}
