// Translator coverage for the `projects` table — UPDATE produces
// project.updated; DELETE produces project.deleted when the
// notification carries enough of the row to populate workspace_id.
// In-tree fixtures pass WorkspaceID explicitly via the surreallive
// Event struct (the production delete notification may not — see plan
// ldg_11d3f11b §3.3 caveat — but the translator's behaviour on a
// well-formed event is the unit under test).

package wshandler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/surreallive"
)

func TestTranslate_Projects_UpdateEmitsProjectUpdated(t *testing.T) {
	ev := surreallive.Event{
		Table:       "projects",
		Action:      surreallive.ActionUpdate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":            "proj_1",
			"workspace_id":  "wksp_A",
			"name":          "satellites",
			"owner_user_id": "u_alice",
			"status":        "active",
			"updated_at":    "2026-05-18T12:00:00Z",
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "project.updated", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "proj_1", got[0].Data["project_id"])
	assert.Equal(t, "satellites", got[0].Data["name"])
	assert.Equal(t, "u_alice", got[0].Data["owner_user_id"])
	assert.Equal(t, "active", got[0].Data["status"])
	assert.Equal(t, "2026-05-18T12:00:00Z", got[0].Data["updated_at"])
}

func TestTranslate_Projects_DeleteEmitsProjectDeleted(t *testing.T) {
	ev := surreallive.Event{
		Table:       "projects",
		Action:      surreallive.ActionDelete,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":           "proj_1",
			"workspace_id": "wksp_A",
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "project.deleted", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "proj_1", got[0].Data["project_id"])
	assert.Equal(t, "wksp_A", got[0].Data["workspace_id"])
}
