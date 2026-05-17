package client

import (
	"testing"
	"time"

	"github.com/bobmcallan/satellites/internal/story"
	"github.com/bobmcallan/satellites/internal/task"
)

// TestComputeLifecycleStatus exercises each drift reason + on_shape
// case (sty_e0c3d615 AC2). Recorded as the regression-test path in
// the story's template-field carrier.
func TestComputeLifecycleStatus(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mk := func(action, kind, status string) task.Task {
		return task.Task{
			Action:    action,
			Kind:      kind,
			Status:    status,
			CreatedAt: now,
		}
	}

	cases := []struct {
		name        string
		storyStatus string
		tasks       []task.Task
		want        string
	}{
		{
			name:        "empty chain returns plan_absent",
			storyStatus: story.StatusBacklog,
			tasks:       nil,
			want:        LifecyclePlanAbsent,
		},
		{
			name:        "develop without plan returns plan_absent",
			storyStatus: story.StatusInProgress,
			tasks: []task.Task{
				mk("contract:develop", task.KindWork, task.StatusClosed),
			},
			want: LifecyclePlanAbsent,
		},
		{
			name:        "plan-only chain mid-flight is on_shape",
			storyStatus: story.StatusInProgress,
			tasks: []task.Task{
				mk("contract:plan", task.KindWork, task.StatusClosed),
			},
			want: LifecycleOnShape,
		},
		{
			name:        "plan + develop work + develop review is on_shape mid-flight",
			storyStatus: story.StatusInProgress,
			tasks: []task.Task{
				mk("contract:plan", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindReview, task.StatusClosed),
			},
			want: LifecycleOnShape,
		},
		{
			name:        "terminal story with develop work but no review returns review_skipped",
			storyStatus: story.StatusDone,
			tasks: []task.Task{
				mk("contract:plan", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindWork, task.StatusClosed),
				mk("contract:merge_to_main", task.KindWork, task.StatusClosed),
			},
			want: LifecycleReviewSkipped,
		},
		{
			name:        "terminal story without merge_to_main returns close_before_push",
			storyStatus: story.StatusDone,
			tasks: []task.Task{
				mk("contract:plan", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindReview, task.StatusClosed),
			},
			want: LifecycleCloseBeforePush,
		},
		{
			name:        "terminal story with full happy chain is on_shape",
			storyStatus: story.StatusDone,
			tasks: []task.Task{
				mk("contract:plan", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindReview, task.StatusClosed),
				mk("contract:merge_to_main", task.KindWork, task.StatusClosed),
			},
			want: LifecycleOnShape,
		},
		{
			name:        "unknown action returns phase_unknown",
			storyStatus: story.StatusInProgress,
			tasks: []task.Task{
				mk("contract:plan", task.KindWork, task.StatusClosed),
				mk("contract:bogus_phase", task.KindWork, task.StatusClosed),
			},
			want: LifecyclePhaseUnknownPfx + "contract:bogus_phase",
		},
		{
			name:        "open tasks on chain do not influence drift",
			storyStatus: story.StatusInProgress,
			tasks: []task.Task{
				mk("contract:plan", task.KindWork, task.StatusClosed),
				mk("contract:develop", task.KindWork, task.StatusPublished),
				mk("contract:made_up", task.KindWork, task.StatusPublished),
			},
			want: LifecycleOnShape,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeLifecycleStatus(tc.storyStatus, tc.tasks)
			if got != tc.want {
				t.Errorf("computeLifecycleStatus = %q, want %q", got, tc.want)
			}
		})
	}
}
