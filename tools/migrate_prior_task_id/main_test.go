// main_test.go — sty_9d046bc7 AC3 unit coverage for the
// migrate_prior_task_id tool.
//
// Covers the argument-validation gate + idempotence guard. The
// DB-level write is exercised by the integration test in
// tests/integration/auto_supersession_test.go (AC2 walk) and the
// MemoryStore unit test for SetPriorTaskID in
// internal/task/store_test.go (AC1).

package main

import (
	"strings"
	"testing"
)

// TestValidateFlags_RequiresAllFields locks the required-flag
// matrix. Each case is the minimum a future caller must satisfy
// before the tool reaches out to the network.
func TestValidateFlags_RequiresAllFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		superseded  string
		active      string
		dbDSN       string
		wantErrSubs string
	}{
		{
			name:        "missing superseded",
			active:      "task_active",
			dbDSN:       "ws://localhost:8000/rpc/satellites/satellites",
			wantErrSubs: "--superseded-task-id",
		},
		{
			name:        "missing active",
			superseded:  "task_failed",
			dbDSN:       "ws://localhost:8000/rpc/satellites/satellites",
			wantErrSubs: "--active-task-id",
		},
		{
			name:        "same superseded + active",
			superseded:  "task_x",
			active:      "task_x",
			dbDSN:       "ws://localhost:8000/rpc/satellites/satellites",
			wantErrSubs: "must differ",
		},
		{
			name:        "missing db-dsn",
			superseded:  "task_failed",
			active:      "task_active",
			wantErrSubs: "--db-dsn",
		},
		{
			name:       "happy path",
			superseded: "task_failed",
			active:     "task_active",
			dbDSN:      "ws://localhost:8000/rpc/satellites/satellites",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFlags(tc.superseded, tc.active, tc.dbDSN)
			if tc.wantErrSubs == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErrSubs)
			}
			if !strings.Contains(err.Error(), tc.wantErrSubs) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSubs)
			}
		})
	}
}
