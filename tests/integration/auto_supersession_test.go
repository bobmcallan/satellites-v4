// auto_supersession_test.go — sty_9d046bc7 AC2 evidence anchor.
//
// Boots a real satellites-server testcontainer wired to a Surreal
// instance + bind-mounted docs/, replicates the sty_a7850269 chain
// shape (closed=failure develop orphan + auto-superseded successor),
// and asserts:
//
//   - The substrate's TaskAdd inner detection step stamps
//     PriorTaskID on the auto-minted successor.
//   - The merge_to_main chain-shape gate accepts the linked chain
//     (replicates internal/agent/worker/hotpath.go's
//     verifyChainPriorWorkSuccess in-test since the symbol is package-
//     private).
//
// AC1 is exercised by the unit tests in internal/client/task_test.go;
// this file's purpose is the cross-cutting wire-layer + chain-shape
// assertion (`pr_local_iteration`, `pr_evidence_audit`).

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestAutoSupersession_ChainShapeGateAccepts is the AC2 evidence
// anchor for sty_9d046bc7.
func TestAutoSupersession_ChainShapeGateAccepts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("integration harness requires a posix shell + docker")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available — skipping testcontainers-backed test")
	}
	if testing.Short() {
		t.Skip("skipping testcontainers test in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	net, err := network.New(ctx)
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	t.Cleanup(func() { _ = net.Remove(ctx) })

	surreal, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:          "surrealdb/surrealdb:v3.0.0",
			ExposedPorts:   []string{"8000/tcp"},
			Cmd:            []string{"start", "--user", "root", "--pass", "root"},
			Networks:       []string{net.Name},
			NetworkAliases: map[string][]string{net.Name: {"surrealdb"}},
			WaitingFor:     wait.ForListeningPort("8000/tcp").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("surreal: %v", err)
	}
	t.Cleanup(func() { _ = surreal.Terminate(ctx) })

	const bearer = "key_auto_supersession_test"
	docsHost := filepath.Join(repoRoot(t), "docs")
	baseURL, stop := startServerContainerWithOptions(t, ctx, startOptions{
		Network: net.Name,
		Env: map[string]string{
			"SATELLITES_DB_DSN":   "ws://root:root@surrealdb:8000/rpc/satellites/satellites",
			"SATELLITES_API_KEYS": bearer,
			"SATELLITES_DOCS_DIR": "/app/docs",
		},
		Mounts: []mount.Mount{{
			Type:     mount.TypeBind,
			Source:   docsHost,
			Target:   "/app/docs",
			ReadOnly: true,
		}},
	})
	defer stop()

	// 1. Project — the auth middleware resolves the api-key caller
	//    against this project's workspace memberships.
	proj := callAPIv1(t, ctx, baseURL, bearer, "project_add", map[string]any{"name": "auto-supersession"})
	projectID, _ := proj["id"].(string)
	if projectID == "" {
		t.Fatalf("project_add returned no id: %+v", proj)
	}

	// 2. Agent — the seeded system-scope substrate_auditor advertises
	//    `delivers: contract:substrate_audit`, so TaskAdd's capability
	//    check accepts kind=work action=contract:substrate_audit
	//    tasks. The `developer_agent` seed lives under config/seed/
	//    wksp_5b3257d1/ (workspace-scoped to a hardcoded id absent
	//    from a fresh container) so the test routes through a
	//    system-scope agent that the bootstrap path always exposes.
	//    Resolved via document_get(type=agent,name=...) since /api/v1
	//    does not expose a typed agent_get verb.
	agent := callAPIv1(t, ctx, baseURL, bearer, "document_get", map[string]any{"type": "agent", "name": "substrate_auditor"})
	agentID, _ := agent["id"].(string)
	if agentID == "" {
		t.Fatalf("document_get(agent substrate_auditor) returned no id: %+v", agent)
	}
	const action = "contract:substrate_audit"

	// 3. Story.
	story := callAPIv1(t, ctx, baseURL, bearer, "story_add", map[string]any{
		"project_id":          projectID,
		"title":               "auto-supersession dogfood",
		"description":         "replicates sty_a7850269 chain shape",
		"acceptance_criteria": "chain shape gate accepts linked chain",
		"priority":            "medium",
		"category":            "improvement",
	})
	storyID, _ := story["id"].(string)
	if storyID == "" {
		t.Fatalf("story_add returned no id: %+v", story)
	}

	// 4. Orphan: kind=work action=contract:develop task; task_add
	//    publishes it.
	orphanOut := callAPIv1(t, ctx, baseURL, bearer, "task_add", map[string]any{
		"agent_id": agentID,
		"prompt":   "first develop attempt",
		"story_id": storyID,
		"kind":     "work",
		"action":   action,
	})
	orphanID, _ := orphanOut["task_id"].(string)
	if orphanID == "" {
		t.Fatalf("task_add (orphan) returned no task_id: %+v", orphanOut)
	}

	// 5. Claim + close=failure to make the orphan a closed=failure
	//    work task — the predecessor shape the auto-supersession
	//    detection picks up.
	claimed := callAPIv1(t, ctx, baseURL, bearer, "task_claim", map[string]any{
		"worker_id": "test-worker",
	})
	if got, _ := claimed["id"].(string); got != orphanID {
		t.Fatalf("task_claim picked %q, want %q (raw=%+v)", got, orphanID, claimed)
	}
	closed := callAPIv1(t, ctx, baseURL, bearer, "task_update", map[string]any{
		"id":      orphanID,
		"status":  "closed",
		"outcome": "failure",
	})
	if got, _ := closed["status"].(string); got != "closed" {
		t.Fatalf("task_update did not close orphan: %+v", closed)
	}

	// 6. Successor: same (kind, action, story_id). The substrate's
	//    inner detection in TaskAdd should stamp prior_task_id on the
	//    new mint.
	successorOut := callAPIv1(t, ctx, baseURL, bearer, "task_add", map[string]any{
		"agent_id": agentID,
		"prompt":   "retry develop attempt",
		"story_id": storyID,
		"kind":     "work",
		"action":   action,
	})
	successorID, _ := successorOut["task_id"].(string)
	if successorID == "" {
		t.Fatalf("task_add (successor) returned no task_id: %+v", successorOut)
	}

	// 7. Round-trip the successor row to confirm prior_task_id is
	//    stamped on the persisted shape.
	successorRow := callAPIv1(t, ctx, baseURL, bearer, "task_get", map[string]any{"id": successorID})
	gotPrior, _ := successorRow["prior_task_id"].(string)
	if gotPrior != orphanID {
		t.Fatalf("successor.prior_task_id = %q, want %q (raw=%+v)", gotPrior, orphanID, successorRow)
	}

	// 8. Close the successor as outcome=success — mirrors the
	//    sty_a7850269 chain shape where the retry develop ran to
	//    completion before merge_to_main fired. The gate is called
	//    against the merge_to_main task (out-of-band here), so the
	//    walk-time chain must contain only closed work tasks for
	//    verifyChainShape to return nil.
	successorClaim := callAPIv1(t, ctx, baseURL, bearer, "task_claim", map[string]any{
		"worker_id": "test-worker",
	})
	if got, _ := successorClaim["id"].(string); got != successorID {
		t.Fatalf("task_claim picked %q, want %q (raw=%+v)", got, successorID, successorClaim)
	}
	successorClose := callAPIv1(t, ctx, baseURL, bearer, "task_update", map[string]any{
		"id":      successorID,
		"status":  "closed",
		"outcome": "success",
	})
	if got, _ := successorClose["status"].(string); got != "closed" {
		t.Fatalf("task_update did not close successor: %+v", successorClose)
	}

	// 9. Walk the chain — the gate's input shape.
	walk := callAPIv1(t, ctx, baseURL, bearer, "task_walk", map[string]any{"story_id": storyID})

	// 10. Chain-shape gate. verifyChainPriorWorkSuccess is package-
	//     private under internal/agent/worker so the test replicates
	//     the same algorithm directly against the walk payload (per
	//     the develop task body's "or replicate its logic" allowance).
	//     ignoreID="" — every closed work task on the chain must be
	//     either outcome=success or have a successor pointing at it
	//     via prior_task_id.
	if err := verifyChainShape(walk, ""); err != nil {
		t.Fatalf("chain shape gate rejected the auto-linked chain: %v\nwalk=%+v", err, walk)
	}
}

// verifyChainShape replicates internal/agent/worker/hotpath.go's
// verifyChainPriorWorkSuccess algorithm against the task_walk wire
// payload. It is a thin re-implementation, NOT a separate gate — the
// production gate is unmodified per the AC2 review-criteria
// ("verifyChainPriorWorkSuccess is NOT modified").
func verifyChainShape(walk map[string]any, ignoreID string) error {
	rawTasks, ok := walk["tasks"].([]any)
	if !ok {
		return fmt.Errorf("walk payload missing tasks array: %+v", walk)
	}
	supersededBy := map[string]bool{}
	for _, raw := range rawTasks {
		t, _ := raw.(map[string]any)
		if pid, _ := t["prior_task_id"].(string); pid != "" {
			supersededBy[pid] = true
		}
	}
	for _, raw := range rawTasks {
		t, _ := raw.(map[string]any)
		id, _ := t["id"].(string)
		if id == ignoreID {
			continue
		}
		kind, _ := t["kind"].(string)
		if kind != "" && kind != "work" {
			continue
		}
		status, _ := t["status"].(string)
		if status != "closed" {
			return fmt.Errorf("chain has open work task %q (status=%s)", id, status)
		}
		outcome, _ := t["outcome"].(string)
		if outcome != "success" {
			if supersededBy[id] {
				continue
			}
			return fmt.Errorf("chain has unsuccessful prior work task %q (outcome=%s) with no retry successor", id, outcome)
		}
	}
	return nil
}

// emit a JSON-encoded version of the walk for evidence-row capture.
func dumpWalk(walk map[string]any) string {
	b, _ := json.Marshal(walk)
	return string(b)
}

var _ = dumpWalk // referenced by ad-hoc evidence dumps; retained for operator use.
