---
name: default_async_dispatch
tags: [kind:agent-process, v4]
---
# satellites · async dispatch

Companion to `default_agent_process` — carries the async-dispatch
loop the orchestrator runs when multiple work tasks are in flight.

#### Async dispatch pattern

When the orchestrator has more than one story (or more than one
slice of the same story) in flight at once, dispatch via
`task run` (the default-async branch) and poll the chain
instead of blocking the orchestrator's bash with `task run
--sync`.

**Pattern.** Author the work task via `task_add` over MCP →
run `satellites-client task run <task_id>` (the CLI
POSTs `/v1/enqueue` on the local daemon socket and exits within
~1s returning `{task_id, daemon_pid, queue_position}`) → while
authoring the next task, poll
`satellites-client task walk --story-id <id>` and
`satellites-client ledger list --task-id <id>
--project-id <pid>` for outcome rows → consume the final state
from the ledger (the `kind:agent-execute-evidence` row recording
`outcome=success|failure` + the per-contract evidence rows the
dispatched team member wrote). The orchestrator never blocks on
a single dispatched run — the wait posture is "poll the chain",
not "await the subprocess".

**Polling cadence.** Default 30s between `task walk` /
`ledger list` polls. The sustainable band is 30-60s (matches
the reclaim watchdog's expectations on the substrate side).
Refinable per-project — short-task projects may poll at 30s;
projects with multi-minute substrate calls may relax to 60s.
Tighter polling (sub-30s) wastes substrate roundtrips without
shortening any single task; looser polling (>60s) leaves real
parallelism on the floor between consecutive `task_add` calls.

**Failure recovery.** When the local serve daemon crashes
mid-flight, the operator restarts it with
`satellites-client serve start`. The daemon's boot-time
reconcile pass clears dead-pid `running` entries from
`state.json` and appends one
`kind:daemon-orphaned-subprocess` evidence row per cleared
entry (tagged `task_id:<orphaned>`). The orchestrator polling
the chain sees the orphan evidence row, treats the task as
failed-without-evidence, and dispatches a retry by minting a
fresh `task_add(prior_task_id=<orphaned_task>, …)` and running
`satellites-client task run` against the new task. No
silent loss: the orphan row + the retry's `prior_task_id`
preserve the audit chain on `task_walk`.

**Worked example — two stories' develop slices in parallel.**
The orchestrator has stories A and B both at the develop step.
Sequence:

1. `task_add(action=contract:develop, story_id=A, agent_id=…)`
   → returns `task_A`.
2. `satellites-client task run task_A` — CLI exits
   within ~1s with `{task_id, daemon_pid, queue_position}`.
3. `task_add(action=contract:develop, story_id=B, agent_id=…)`
   → returns `task_B`.
4. `satellites-client task run task_B` — CLI exits
   within ~1s.
5. Poll loop at 30s:
   - `satellites-client task walk --story-id A`
   - `satellites-client task walk --story-id B`
   - `satellites-client ledger list --task-id task_A
     --project-id <pid>`
   - `satellites-client ledger list --task-id task_B
     --project-id <pid>`
6. Each task surfaces a `kind:agent-execute-evidence` row
   carrying `outcome=success|failure`. When both have surfaced,
   mint A's `develop_review` and B's `develop_review` tasks.

Wall-clock saving vs strict-serial dispatch: the two develops
run concurrently on the daemon's worker pool; total elapsed
≈ max(A_runtime, B_runtime) instead of A_runtime + B_runtime.
The saving scales with parallelism (`--parallelism` flag on
`serve start`, default 2).
