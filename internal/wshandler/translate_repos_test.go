// Translator coverage for the `repos` + `commits` tables — kind
// shape, payload keys, the archive-status branch, and the commit
// fanout for repo.commit_appended.

package wshandler

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/surreallive"
)

func TestTranslate_Repos_CreateEmitsRepoCreated(t *testing.T) {
	ev := surreallive.Event{
		Table:       "repos",
		Action:      surreallive.ActionCreate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":         "repo_abc",
			"git_remote": "git@github.com:example/x.git",
			"status":     "active",
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "repo.created", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "repo_abc", got[0].Data["repo_id"])
	assert.Equal(t, "git@github.com:example/x.git", got[0].Data["git_remote"])
	assert.Equal(t, "active", got[0].Data["status"])
	assert.Equal(t, "proj_1", got[0].Data["project_id"])
}

func TestTranslate_Repos_UpdateEmitsRepoUpdated(t *testing.T) {
	ev := surreallive.Event{
		Table:       "repos",
		Action:      surreallive.ActionUpdate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":              "repo_abc",
			"status":          "active",
			"head_sha":        "deadbeef",
			"last_indexed_at": "2026-05-18T10:00:00Z",
			"symbol_count":    42,
			"file_count":      7,
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "repo.updated", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "repo_abc", got[0].Data["repo_id"])
	assert.Equal(t, "deadbeef", got[0].Data["head_sha"])
	assert.Equal(t, "2026-05-18T10:00:00Z", got[0].Data["last_indexed_at"])
	assert.Equal(t, 42, got[0].Data["symbol_count"])
	assert.Equal(t, 7, got[0].Data["file_count"])
}

func TestTranslate_Repos_ArchivedStatusEmitsRepoArchived(t *testing.T) {
	ev := surreallive.Event{
		Table:       "repos",
		Action:      surreallive.ActionUpdate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"id":     "repo_abc",
			"status": "archived",
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "repo.archived", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "archived", got[0].Data["status"])
	assert.Equal(t, "repo_abc", got[0].Data["repo_id"])
}

func TestTranslate_Commits_CreateEmitsRepoCommitAppended(t *testing.T) {
	ev := surreallive.Event{
		Table:       "commits",
		Action:      surreallive.ActionCreate,
		WorkspaceID: "wksp_A",
		ProjectID:   "proj_1",
		Row: map[string]any{
			"repo_id":      "repo_abc",
			"sha":          "abc123",
			"author":       "Alice",
			"committed_at": "2026-05-18T11:00:00Z",
		},
	}
	got := translate(ev)
	require.Len(t, got, 1)
	assert.Equal(t, "repo.commit_appended", got[0].Kind)
	assert.Equal(t, "wksp_A", got[0].WorkspaceID)
	assert.Equal(t, "repo_abc", got[0].Data["repo_id"])
	assert.Equal(t, "abc123", got[0].Data["sha"])
	assert.Equal(t, "Alice", got[0].Data["author"])
	assert.Equal(t, "2026-05-18T11:00:00Z", got[0].Data["committed_at"])
	assert.Equal(t, "proj_1", got[0].Data["project_id"])
}

func TestTranslate_Repos_DeleteDrops(t *testing.T) {
	ev := surreallive.Event{
		Table:       "repos",
		Action:      surreallive.ActionDelete,
		WorkspaceID: "wksp_A",
		Row:         map[string]any{"id": "repo_zzz"},
	}
	assert.Len(t, translate(ev), 0)
}
