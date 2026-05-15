// migrate_prior_task_id — sty_9d046bc7 AC3 operator-run tool.
//
// One-shot Go binary that backfills `prior_task_id` on an active
// kind=work task to point at a closed=failure predecessor on the
// same story. Used to recover chains that pre-date the auto-
// supersession detection in client.TaskAdd (e.g. sty_a7850269's
// task_9fbc54a6 → task_9da8a917 linkage that runMergeToMainHotPath's
// chain-shape gate rejected).
//
// Usage:
//
//	migrate_prior_task_id \
//	  --superseded-task-id task_<failed_predecessor> \
//	  --active-task-id     task_<successor> \
//	  --db-dsn ws://root:root@localhost:8000/rpc/satellites/satellites \
//	  --server http://localhost:8080 \
//	  --bearer <api-key>
//
// The tool talks directly to SurrealDB via internal/task.SurrealStore
// to issue the linkage write — task_update MCP/HTTP only supports
// status=closed, and exposing prior_task_id on the public task_add
// surface is out of scope (sty_27516920 owns that). The
// linkage-write is idempotent: if the active task already carries a
// non-empty prior_task_id, the tool is a no-op (exit 0 with a stdout
// note) per the AC3 PASS contract.
//
// After the linkage write, the tool issues `ledger_append` over the
// live HTTP API tagged with `migration:prior_task_id_backfill,
// superseded_task_id:<id>, active_task_id:<id>, story_id:<id>` and
// prints the new ledger row id to stdout for operator capture.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bobmcallan/satellites/internal/cliconfig"
	"github.com/bobmcallan/satellites/internal/cliremote"
	"github.com/bobmcallan/satellites/internal/db"
	"github.com/bobmcallan/satellites/internal/task"
)

// taskShape is the decode target for /api/v1/task/get. Only the
// fields the migration touches are surfaced — the substrate's full
// task row is wider than this struct.
type taskShape struct {
	ID          string `json:"id"`
	StoryID     string `json:"story_id"`
	WorkspaceID string `json:"workspace_id"`
	ProjectID   string `json:"project_id"`
	Status      string `json:"status"`
	Outcome     string `json:"outcome"`
	PriorTaskID string `json:"prior_task_id"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run is the testable entrypoint. Args mirror os.Args[1:], and stdout/
// stderr are pluggable so unit tests can capture them.
func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("migrate_prior_task_id", flag.ContinueOnError)
	fs.SetOutput(stderr)
	supersededID := fs.String("superseded-task-id", "", "Closed=failure predecessor task id (required).")
	activeID := fs.String("active-task-id", "", "Successor task id whose prior_task_id will be stamped (required).")
	server := fs.String("server", "", "satellites-server URL (defaults to satellites-client config).")
	bearer := fs.String("bearer", "", "API key bearer for ledger_append (defaults to satellites-client config token).")
	dbDSN := fs.String("db-dsn", "", "SurrealDB DSN, e.g. ws://root:root@localhost:8000/rpc/satellites/satellites (required).")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateFlags(*supersededID, *activeID, *dbDSN); err != nil {
		fs.Usage()
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg, _, _ := cliconfig.Load("")
	resolvedServer := strings.TrimSpace(*server)
	resolvedBearer := strings.TrimSpace(*bearer)
	if cfg != nil {
		if resolvedServer == "" {
			resolvedServer = cfg.Server
		}
		if resolvedBearer == "" {
			resolvedBearer = cfg.Token
		}
	}
	if resolvedServer == "" {
		return errors.New("--server (or satellites-client config) is required")
	}

	remote := cliremote.New(resolvedServer, resolvedBearer, nil)

	active, err := fetchTask(ctx, remote, *activeID)
	if err != nil {
		return fmt.Errorf("task_get(active=%s): %w", *activeID, err)
	}

	// Idempotence guard: refuse to overwrite an already-linked row.
	if active.PriorTaskID != "" {
		fmt.Fprintf(stdout, "no-op: active_task_id=%s already linked to prior_task_id=%s\n", active.ID, active.PriorTaskID)
		return nil
	}

	superseded, err := fetchTask(ctx, remote, *supersededID)
	if err != nil {
		return fmt.Errorf("task_get(superseded=%s): %w", *supersededID, err)
	}
	if active.StoryID == "" || active.StoryID != superseded.StoryID {
		return fmt.Errorf("story_id mismatch: active.story_id=%q superseded.story_id=%q", active.StoryID, superseded.StoryID)
	}
	if superseded.Outcome != task.OutcomeFailure {
		return fmt.Errorf("superseded_task_id=%s outcome=%q, want %q", superseded.ID, superseded.Outcome, task.OutcomeFailure)
	}

	// Direct DB write through the substrate's SurrealStore.
	dbCfg, err := db.ParseDSN(*dbDSN)
	if err != nil {
		return fmt.Errorf("parse db dsn: %w", err)
	}
	conn, err := db.Connect(ctx, dbCfg)
	if err != nil {
		return fmt.Errorf("db connect: %w", err)
	}
	store := task.NewSurrealStore(conn)
	updated, err := store.SetPriorTaskID(ctx, active.ID, superseded.ID, time.Now().UTC(), nil)
	if err != nil {
		return fmt.Errorf("set_prior_task_id: %w", err)
	}

	// kind:migration ledger row for audit. Carries before/after
	// shapes so an operator can replay or reconcile the linkage from
	// the row alone (`pr_evidence_audit`).
	beforeJSON, _ := json.Marshal(active)
	afterJSON, _ := json.Marshal(updated)
	ledgerArgs := map[string]any{
		"project_id": active.ProjectID,
		"story_id":   active.StoryID,
		"type":       "evidence",
		"tags": []string{
			"kind:migration",
			"migration:prior_task_id_backfill",
			"superseded_task_id:" + superseded.ID,
			"active_task_id:" + active.ID,
			"story_id:" + active.StoryID,
		},
		"content": fmt.Sprintf("prior_task_id backfill\nbefore: %s\nafter: %s\n", string(beforeJSON), string(afterJSON)),
	}
	var ledgerOut struct {
		ID string `json:"id"`
	}
	if err := remote.Call(ctx, "ledger_append", ledgerArgs, &ledgerOut); err != nil {
		return fmt.Errorf("ledger_append: %w", err)
	}
	fmt.Fprintln(stdout, ledgerOut.ID)
	return nil
}

// validateFlags enforces the required-flag set BEFORE the tool
// reaches out to the network. Pulled out for unit-test reach.
func validateFlags(supersededID, activeID, dbDSN string) error {
	if strings.TrimSpace(supersededID) == "" {
		return errors.New("--superseded-task-id is required")
	}
	if strings.TrimSpace(activeID) == "" {
		return errors.New("--active-task-id is required")
	}
	if strings.TrimSpace(supersededID) == strings.TrimSpace(activeID) {
		return errors.New("--superseded-task-id and --active-task-id must differ")
	}
	if strings.TrimSpace(dbDSN) == "" {
		return errors.New("--db-dsn is required")
	}
	return nil
}

// fetchTask issues task_get against the remote and decodes the result
// into the local taskShape. Surfaces the typed cliexit error verbatim
// so the operator sees the canonical not-found / auth message.
func fetchTask(ctx context.Context, remote *cliremote.Client, id string) (taskShape, error) {
	var out taskShape
	if err := remote.Call(ctx, "task_get", map[string]any{"id": id}, &out); err != nil {
		return taskShape{}, err
	}
	if out.ID == "" {
		return taskShape{}, fmt.Errorf("task_get(%s) returned empty id", id)
	}
	return out, nil
}
