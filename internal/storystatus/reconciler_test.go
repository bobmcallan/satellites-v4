// sty_051bd266 — exercise the task → story status derivation rule:
// a story at backlog or ready flips to in_progress on the first
// observed task in a non-terminal status. Already-in-progress and
// terminal stories are no-ops. Tasks without a story_id are
// silently ignored.
package storystatus_test

import (
	"context"
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/ledger"
	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/storystatus"
	"github.com/bobmcallan/satellites/internal/task"
)

func TestReconciler_FlipsBacklogToInProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()

	stories := story.NewMemoryStore(ledger.NewMemoryStore())
	created, err := stories.Create(ctx, story.Story{
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		Title:       "x",
		Status:      story.StatusBacklog,
	}, now)
	if err != nil {
		t.Fatalf("create story: %v", err)
	}

	rec := storystatus.New(stories, nil)
	rec.OnEmit(ctx, task.Task{
		ID:          "task_1",
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		StoryID:     created.ID,
		Status:      task.StatusPublished,
	})

	got, err := stories.GetByID(ctx, created.ID, nil)
	if err != nil {
		t.Fatalf("get story: %v", err)
	}
	if got.Status != story.StatusInProgress {
		t.Fatalf("expected status=%s, got %s", story.StatusInProgress, got.Status)
	}
}

func TestReconciler_FlipsReadyToInProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()

	stories := story.NewMemoryStore(ledger.NewMemoryStore())
	created, err := stories.Create(ctx, story.Story{
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		Title:       "x",
		Status:      story.StatusReady,
	}, now)
	if err != nil {
		t.Fatalf("create story: %v", err)
	}

	storystatus.New(stories, nil).OnEmit(ctx, task.Task{
		ID:          "task_2",
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		StoryID:     created.ID,
		Status:      task.StatusClaimed,
	})

	got, _ := stories.GetByID(ctx, created.ID, nil)
	if got.Status != story.StatusInProgress {
		t.Fatalf("expected status=%s, got %s", story.StatusInProgress, got.Status)
	}
}

func TestReconciler_NoOpOnInProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()

	stories := story.NewMemoryStore(ledger.NewMemoryStore())
	created, _ := stories.Create(ctx, story.Story{
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		Title:       "x",
		Status:      story.StatusInProgress,
	}, now)
	updatedAt := created.UpdatedAt

	storystatus.New(stories, nil).OnEmit(ctx, task.Task{
		ID:          "task_3",
		WorkspaceID: "wksp_test",
		StoryID:     created.ID,
		Status:      task.StatusPublished,
	})

	got, _ := stories.GetByID(ctx, created.ID, nil)
	if got.Status != story.StatusInProgress {
		t.Fatalf("status drifted: %s", got.Status)
	}
	if !got.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("UpdatedAt should not move on a no-op flip; was %v now %v", updatedAt, got.UpdatedAt)
	}
}

func TestReconciler_NoOpOnDoneStory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()

	stories := story.NewMemoryStore(ledger.NewMemoryStore())
	created, _ := stories.Create(ctx, story.Story{
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		Title:       "x",
		Status:      story.StatusDone,
	}, now)

	storystatus.New(stories, nil).OnEmit(ctx, task.Task{
		ID:          "task_4",
		WorkspaceID: "wksp_test",
		StoryID:     created.ID,
		Status:      task.StatusPublished,
	})

	got, _ := stories.GetByID(ctx, created.ID, nil)
	if got.Status != story.StatusDone {
		t.Fatalf("done story drifted to %s", got.Status)
	}
}

func TestReconciler_NoOpOnTerminalTaskStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now().UTC()

	stories := story.NewMemoryStore(ledger.NewMemoryStore())
	created, _ := stories.Create(ctx, story.Story{
		WorkspaceID: "wksp_test",
		ProjectID:   "proj_test",
		Title:       "x",
		Status:      story.StatusBacklog,
	}, now)

	// task closed on its own should NOT flip the story to in_progress.
	storystatus.New(stories, nil).OnEmit(ctx, task.Task{
		ID:          "task_5",
		WorkspaceID: "wksp_test",
		StoryID:     created.ID,
		Status:      task.StatusClosed,
	})

	got, _ := stories.GetByID(ctx, created.ID, nil)
	if got.Status != story.StatusBacklog {
		t.Fatalf("backlog story drifted on terminal task: %s", got.Status)
	}
}

func TestReconciler_NoOpOnEmptyStoryID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	stories := story.NewMemoryStore(ledger.NewMemoryStore())

	// No panic, no substrate write — listener silently drops.
	storystatus.New(stories, nil).OnEmit(ctx, task.Task{
		ID:          "task_6",
		WorkspaceID: "wksp_test",
		Status:      task.StatusPublished,
	})
}
