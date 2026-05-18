// Translator coverage for the `documents` table — kind shape, payload
// keys, and the contract-type fanout. Mirrors the structural style of
// translateLedger's coverage in wshandler_test.go: construct a
// surreallive.Event, call translate(), assert against the produced
// []WireEvent.

package wshandler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/surreallive"
)

func TestTranslate_Documents_CreateEmitsDocumentStatus(t *testing.T) {
	ev := surreallive.Event{
		Table:       "documents",
		Action:      surreallive.ActionCreate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":           "doc_abc",
			"workspace_id": "wksp_A",
			"project_id":   "proj_1",
			"type":         "story",
			"name":         "auth panel",
			"scope":        "project",
			"status":       "in_progress",
			"updated_at":   "2026-05-18T09:00:00Z",
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "document.in_progress", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "wksp_A", got[0].Data["workspace_id"])
	assert.Equal(t, "proj_1", got[0].Data["project_id"])
	assert.Equal(t, "doc_abc", got[0].Data["document_id"])
	assert.Equal(t, "story", got[0].Data["type"])
	assert.Equal(t, "auth panel", got[0].Data["name"])
	assert.Equal(t, "project", got[0].Data["scope"])
	assert.Equal(t, "in_progress", got[0].Data["status"])
	assert.Equal(t, "2026-05-18T09:00:00Z", got[0].Data["updated_at"])
}

func TestTranslate_Documents_UpdateEmitsDocumentStatus(t *testing.T) {
	ev := surreallive.Event{
		Table:       "documents",
		Action:      surreallive.ActionUpdate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":     "doc_def",
			"type":   "story",
			"name":   "auth panel",
			"scope":  "project",
			"status": "done",
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "document.done", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "doc_def", got[0].Data["document_id"])
	assert.Equal(t, "done", got[0].Data["status"])
}

func TestTranslate_Documents_ContractTypeAlsoEmitsContractEvent(t *testing.T) {
	ev := surreallive.Event{
		Table:       "documents",
		Action:      surreallive.ActionUpdate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":     "doc_contract",
			"type":   "contract",
			"name":   "develop",
			"scope":  "project",
			"status": "active",
		},
	}
	got := translate(ev)
	require.Len(t, got, 2, "contract-type document fans out to document.<status> + contract.<status>")
	assert.Equal(t, "document.active", got[0].Kind)
	assert.Equal(t, "contract.active", got[1].Kind)
	assert.Equal(t, "wksp_A", got[1].WorkspaceID)
	assert.Equal(t, "doc_contract", got[1].Data["contract_id"])
	assert.Equal(t, "develop", got[1].Data["name"])
	assert.Equal(t, "project", got[1].Data["scope"])
	assert.Equal(t, "active", got[1].Data["status"])
	assert.Equal(t, "wksp_A", got[1].Data["workspace_id"])
	assert.Equal(t, "proj_1", got[1].Data["project_id"])
}

func TestTranslate_Documents_DeleteDrops(t *testing.T) {
	ev := surreallive.Event{
		Table:       "documents",
		Action:      surreallive.ActionDelete,
		WorkspaceID: "wksp_A",
		Row:         map[string]any{"id": "doc_zzz"},
	}
	assert.Len(t, translate(ev), 0)
}

func TestTranslate_Documents_EmptyWorkspaceDrops(t *testing.T) {
	ev := surreallive.Event{
		Table:       "documents",
		Action:      surreallive.ActionCreate,
		WorkspaceID: "",
		Row: map[string]any{
			"id":     "doc_x",
			"type":   "story",
			"status": "active",
		},
	}
	assert.Len(t, translate(ev), 0, "translate() drops events with empty WorkspaceID")
}
