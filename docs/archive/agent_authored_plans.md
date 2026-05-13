# Agent-authored plans, substrate-validated structure

Discussion doc capturing the architectural redirect from
`orchestrator_compose_plan` (substrate hardcodes the plan) to
`task_submit(kind:plan, plan_markdown, tasks[])` (agent authors the
plan; substrate validates structure and auto-inserts review tasks).

This is a significant tightening of `sty_c6d76a5b` ("tasks are the
conversation"). The shipped checkpoint 3 (commit `e72cca7`) emits a
hardcoded 5-slot pair list at compose time — useful as a stepping stone
but not the destination.

## The proposed flow

1. **MCP init.** Server returns the "what is satellites" handshake:
   what a task is, what a contract is, what an agent is, plus the
   instruction to set project context. Today this is the
   `default_agent_process.md` artifact loaded into the MCP
   `instructions` block.

2. **`project_set`.** Returns the project context: the minimum
   contract requirements (plan, develop, …, story_close) and the
   agents available to fulfill them. Both come from project-scope
   markdown, not Go code.

3. **Operator: `implement <story_id>` / `resume <story_id>`.**

4. **Orchestrator agent → `story_get(story_id)`.** Returns the
   story body, status, recent ledger ids, and the instruction to
   submit a plan via `task_submit(kind:plan, …)`. The agent
   may pull more context (ledger rows, code) before it composes the
   plan.

5. **Orchestrator → `task_submit(kind=plan, plan_markdown,
   tasks[])`.** Single submit verb — replaces compose_plan,
   workflow_claim, contract_claim, task_enqueue. Substrate
   mechanically:

   - Validates `tasks[0].kind == "plan"` (rejects otherwise).
   - Validates the task list against the project's
     minimum-required set.
   - Appends the tasks to the story's task list.
   - Closes the implicit kind:plan task with the agent's
     plan_markdown as evidence.
   - Writes a ledger row `kind:plan` carrying plan_markdown.
   - **Auto-inserts a `kind:plan_review` task** immediately after the
     plan task. (Optionally: auto-insert `kind:<step>_review` after
     every implement task in the submitted list — saves the agent
     from doing it manually and guarantees the review-pair
     invariant.)

6. **Reviewer agent (subscriber).** Sees the planned/published
   `kind:plan_review` task on its subscribe channel. Calls
   `story_task_claim(task_id)`. Substrate provides similar context to
   what the orchestrator received (story, ledger refs) plus the
   instruction: read `kind:plan` ledger row, write a pass/fail ledger
   row with detail. The reviewer may pull more context.

7. **Reviewer → `task_submit(kind=plan_accept | plan_reject,
   verdict_markdown, evidence_ledger_ids[])`.** This both closes the
   reviewer's claimed plan_review task and emits a successor task
   that the orchestrator can act on. The accept/reject task IS the
   verdict; there is no verdict-enum on close.

8. **Orchestrator picks up `kind:plan_accept` (or `kind:plan_reject`).**
   On accept: marks the next implement task as ready and begins it.
   On reject: amends the plan per the reviewer's evidence and
   submits a fresh `kind:plan` (next iteration).

9. **Per-step iteration.** Each implement task closes via
   `task_submit(kind=<step>_close, …)`. Substrate flips that
   to closed and (if not already present) auto-spawns the
   `kind:<step>_review` task. Reviewer claims, judges, submits
   `kind:<step>_accept` or `kind:<step>_reject`. On reject, the
   orchestrator's strategy markdown decides whether to retry the
   step (spawn successor implement task with `prior_task_id`),
   split the work into siblings, or escalate.

## Where the line sits

| Concern | Owner |
|---|---|
| What contracts exist in this project | Markdown (project / system) |
| Default minimum task list per story | Markdown (project / system) |
| What tasks make up *this* plan | Orchestrator agent |
| Reviewer's pushback rubric | Markdown (reviewer agent body) |
| Strategy on rejection (iterate / split / multi-cycle) | Orchestrator agent body |
| Auto-insertion of review task per implement | Substrate (mechanical) |
| Validation that plan starts with `plan` and ends with `story_close` | Substrate (mechanical) |
| Pass/fail decision for a review | Reviewer agent → kind:*_accept/reject task |
| Routing review tasks to the right reviewer | ? (see open question below) |

## What this replaces

Verbs that go away:
- `orchestrator_compose_plan` — agent submits via `task_add`.
- `orchestrator_submit_plan` — same.
- `workflow_claim` — substrate creates contract instances (or skips
  the CI concept entirely; see open question) when the plan task list
  is appended.
- `contract_claim` / `contract_close` / `contract_respond` —
  per the existing sty_c6d76a5b spec, deleted.
- `contract_review_close` / `CommitReviewVerdict` — replaced by
  the reviewer submitting `kind:*_accept` / `kind:*_reject` via
  `task_add`.
- `task_enqueue` / `task_publish` / `task_close` — collapsed into
  `task_add`.

Verbs that stay or get added:
- `story_get(story_id)` — context bundle for an agent about to
  work the story.
- `task_submit(kind, markdown, tasks[]?, evidence_ledger_ids[]?)`.
- `story_task_claim(task_id)` — subscriber-side directed claim.
- `story_task_walk(story_id)` — read the conversation log.
- `ledger_*` (unchanged).

## Open questions for the user

These need direction before this can become a story:

1. **Polling vs subscribe for the orchestrator.** The user flagged
   this as "the tricky part." Two shapes:
   - Orchestrator session stays alive, calls `story_task_walk` on a
     loop until `kind:plan_accept` appears. Simple but burns a
     session.
   - Orchestrator session ends after submit; the operator's next
     prompt cycle is the natural poll. Aligns with how Claude Code
     conversations actually work today (operator drives cadence).
     Reviewer service (separate persistent runtime) is the only
     true subscriber.

   Recommend (b): operator-paced. The orchestrator never blocks.

   (User) Yes option B

2. **Do contract instances survive?** Today CIs are first-class
   storage rows. Under the proposed flow the substrate could
   collapse CIs into "the task chain for a contract step" — a CI is
   just `tasks where kind=<step>_*`. Removing CIs simplifies the
   storage model but breaks every CI-keyed query (portal, ledger
   contract_id FK, etc.). Recommend: keep the CI row as a
   bookkeeping anchor for the duration of `sty_c6d76a5b`, revisit
   removal in a follow-up story.

   (User) Contracts become reference ONLY, and task chain (story) is the only store. REMOVE/DELETE CIS as story info.

3. **Where does "default minimum task list" live?** Options:
   - System-scope workflow doc (today: `config/seed/workflows/default.md`).
   - Project-scope workflow doc (overrides system).
   - Both, with project layered over system.

   Recommend: both, layered, like the existing KV scope chain.

   (Ueer) Minimum task list is consturcted form the contract requirement. plan -> develop -> etc.

4. **Reviewer routing.** Today: hardcoded `if contractName ==
   "develop": development_reviewer else story_reviewer` in three
   files. Under the proposed flow this should be a frontmatter
   field on the contract markdown (`reviewer_agent: development_reviewer`)
   so adding a new contract step doesn't require Go changes.
   Small but real config-over-code violation.

   (User) Agents can be marked to delivery contracts, however as the task is the devliery, that is where the agent matche occurs. task = contract:develop agent:develop 

5. **Auto-insertion granularity.** Two options:
   - Substrate auto-inserts a single `kind:plan_review` after the
     plan task; the orchestrator emits `kind:<step>_review` tasks
     manually inside the plan task list.
   - Substrate auto-inserts a `kind:<step>_review` after every
     implement task in the submitted list, regardless of whether
     the agent included them.

   Recommend (b): substrate guarantees the review-after-every-step
   invariant; agents can't forget. The agent still controls the
   list of *implement* tasks.

   (User) Both. The agent/orchestrator instrcutions/context suggest and substrate enforces. All tasks should be reviewed.

6. **What's the kind taxonomy?** Two shapes worth considering:
   - Per-step kinds: `kind:plan`, `kind:plan_review`,
     `kind:plan_accept`, `kind:plan_reject`, `kind:develop`,
     `kind:develop_review`, `kind:develop_accept`, `kind:develop_reject`,
     etc. Verbose but explicit.
   - Generic kinds: `kind:implement`, `kind:review`, `kind:verdict`
     with a `step` field carrying the contract name. Less repetition.

   Recommend per-step kinds (option a) because the substrate's
   validators use them as routing keys; explicit is easier to
   debug.

   (User) Generic is closer. `kind:plan` it is the task, the result is the ledger.

7. **Strategy proposals (split / cycles).** Per the existing story
   body, reviewer pushback can spawn `kind:split-proposal` or
   `kind:cycles-proposal` tasks. Do these go through the same
   `task_add` verb? (Yes, presumably — they're just
   another kind.) And what does the orchestrator do with them?
   Read the markdown body and emit a fresh kind:plan that
   incorporates the split? That keeps it agent-driven.

   (User) As above, and this is the concersation between agent/orchestrator and agent/review the ledger is the result. Task are only tasks. 


NOTE: Possible to have a number of ledger items marked against the task. Hence a task should not have a markdown context, other than descibing the task.

## Relation to in-progress work

- **Just shipped (`e72cca7`):** paired implement+review emission at
  compose time. Useful primitive — auto-pairing is what the
  proposed flow asks for at every step. But the trigger is wrong
  (operator-driven `compose_plan` instead of agent-driven
  `task_submit(kind=plan)`), and the list is hardcoded
  instead of agent-authored.

- **`sty_c6d76a5b` AC re-statement.** The current story body says
  "orchestrator_compose_plan emits the initial implement+review
  task pair per contract (10 tasks for default 5-slot workflow)."
  Under the proposed model that AC is wrong: the agent decides how
  many implement tasks the plan contains; the substrate inserts
  review tasks alongside. The story body needs editing before the
  next implementation slice.

- **Remaining-9 list.** The order shifts:
  1. Add `story_get` + `task_add` + validation rules
     (kind:plan first, default minimum, auto-insert reviews).
  2. Reviewer service: subscribe via hubemit (item #2 from the old
     list — still next, unchanged).
  3. Reviewer's verdict path: emit `kind:*_accept` / `kind:*_reject`
     tasks via `task_add` instead of calling
     `contract_review_close`.
  4. Markdown: reviewer routing on contract frontmatter (open
     question 4).
  5. Delete `orchestrator_compose_plan`, `orchestrator_submit_plan`,
     `workflow_claim`, `contract_claim`, `contract_close`,
     `contract_respond`, `contract_review_close`, `task_close`,
     `task_publish`, `task_enqueue`. Migrate tests.
  6. Boot migration for in-flight CIs.
  7. 13 markdown files migration (orchestrator + reviewer + per-contract
     bodies). The `default_agent_process.md` rewrite is the most
     load-bearing — that's what new MCP sessions read at init.

## Suggested next step

Before implementing: confirm answers to open questions 1, 2, 4, 5, 6
above. The other questions can be punted to during-implementation
discoveries, but those four shape the substrate verb signatures.

After alignment: edit `sty_c6d76a5b`'s body to match this model,
then implement in the order above, on main, wip commits per slice.
