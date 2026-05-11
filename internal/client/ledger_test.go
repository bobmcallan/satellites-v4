package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bobmcallan/satellites/internal/ledger"
)

func newLedgerClient(t *testing.T) (*Client, time.Time) {
	t.Helper()
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ledStore := ledger.NewMemoryStore()
	c := New(Deps{Ledger: ledStore})
	return c, now
}

func TestLedgerAppend_ClassifiesKnownTypeUnchanged(t *testing.T) {
	c, now := newLedgerClient(t)
	row, err := c.LedgerAppend(context.Background(), Caller{UserID: "u_alice"}, LedgerAppendInput{
		ResolvedProjectID: "proj_test",
		WorkspaceID:       "wksp_test",
		EventType:         ledger.TypeEvidence,
		Content:           "x",
		Now:               now,
	})
	require.NoError(t, err)
	assert.Equal(t, ledger.TypeEvidence, row.Type)
	assert.Empty(t, row.Tags, "known type does not auto-tag")
}

func TestLedgerAppend_ClassifiesUnknownTypeAsDecisionWithKindTag(t *testing.T) {
	c, now := newLedgerClient(t)
	row, err := c.LedgerAppend(context.Background(), Caller{UserID: "u_alice"}, LedgerAppendInput{
		ResolvedProjectID: "proj_test",
		WorkspaceID:       "wksp_test",
		EventType:         "lifecycle-tick",
		Content:           "x",
		Tags:              []string{"caller-tag"},
		Now:               now,
	})
	require.NoError(t, err)
	assert.Equal(t, ledger.TypeDecision, row.Type)
	assert.Contains(t, row.Tags, "kind:lifecycle-tick")
	assert.Contains(t, row.Tags, "caller-tag")
}

func TestLedgerAppend_RejectsMissingProject(t *testing.T) {
	c, now := newLedgerClient(t)
	_, err := c.LedgerAppend(context.Background(), Caller{}, LedgerAppendInput{
		EventType: ledger.TypeEvidence,
		Now:       now,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "project_id required")
}

func TestLedgerAppend_RejectsMissingType(t *testing.T) {
	c, now := newLedgerClient(t)
	_, err := c.LedgerAppend(context.Background(), Caller{}, LedgerAppendInput{
		ResolvedProjectID: "proj_test",
		Now:               now,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "type required")
}

func TestLedgerList_FiltersByStoryID(t *testing.T) {
	c, now := newLedgerClient(t)
	for i, sid := range []string{"sty_a", "sty_a", "sty_b"} {
		_, err := c.LedgerAppend(context.Background(), Caller{UserID: "u_alice"}, LedgerAppendInput{
			ResolvedProjectID: "proj_test",
			WorkspaceID:       "wksp_test",
			StoryID:           sid,
			EventType:         ledger.TypeEvidence,
			Content:           "row",
			Now:               now.Add(time.Duration(i) * time.Second),
		})
		require.NoError(t, err)
	}

	rows, err := c.LedgerList(context.Background(), Caller{UserID: "u_alice"}, LedgerListInput{
		ResolvedProjectID: "proj_test",
		Options:           ledger.ListOptions{StoryID: "sty_a", Limit: 10},
	})
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

func TestClassifyLedgerEvent_KnownVsUnknown(t *testing.T) {
	knownType, knownTags := ClassifyLedgerEvent(ledger.TypeVerdict)
	assert.Equal(t, ledger.TypeVerdict, knownType)
	assert.Nil(t, knownTags)

	unkType, unkTags := ClassifyLedgerEvent("custom-event")
	assert.Equal(t, ledger.TypeDecision, unkType)
	assert.Equal(t, []string{"kind:custom-event"}, unkTags)
}
