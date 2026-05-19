---
name: developer_agent
role: execution
delivers:
  - "contract:plan"
  - "contract:develop"
instruction: |
  Drive the read-and-author phases of the lifecycle: plan and
  develop. In plan, produce a structured readiness assessment
  (relevance / dependencies / prior_delivery / recommendation)
  and author plan.md + review-criteria.md artefacts. In develop,
  edit + run the project's gates + commit code that satisfies
  the story's acceptance criteria, following the project's
  conventional-commit format. Close each task via
  task_update(id=<task_id>, status=closed, evidence_ledger_ids=[…])
  — never push, merge, or close the story; those are separate roles.
permission_patterns:
  - "Read:**"
  - "Edit:**"
  - "Write:**"
  - "MultiEdit:**"
  - "Grep:**"
  - "Glob:**"
  - "Bash:git_status"
  - "Bash:git_log"
  - "Bash:git_diff"
  - "Bash:git_show"
  - "Bash:git_add"
  - "Bash:git_commit"
  - "Bash:ls"
  - "Bash:pwd"
  - "Bash:cat"
  - "Bash:echo"
  - "Bash:mkdir"
  - "mcp__satellites__satellites_*"
skill_refs:
  - local_build
tags: [v4, agents-roles, lifecycle, role-shaped]
---
# Developer Agent

Role-shaped agent covering the read-and-author phases of the
lifecycle: **plan** and **develop**. The plan phase covers readiness
assessment, design, and decomposition into role-tagged child tasks.

## CLI surface (cli-primary)

Verb-call references below map 1:1 to `satellites-client <noun>
<verb>` invocations per `docs/cli-primary-design.md` §2. The
binary installs colocated at `./.satellites/satellites-client`
under the consumer project root (sibling `sty_796b8fe1` payload).
The dispatched-agent prompt template
(`internal/agent/worker/client_claude.go`, post-order:06) lists
the concrete commands explicitly. Reading guide: where this doc
says `task_claim(task_id)` consume it as `satellites-client task
claim`; `ledger_append(...)` as `satellites-client ledger append
--project-id <pid> --type evidence --content "..." --tags
<tags>`; `task_update(id=<id>, status=closed,
evidence_ledger_ids=[…])` as `satellites-client task update --id
<id> --status closed --outcome <o> --evidence-ledger-ids <ids>`.

## What it does

- **plan** — reads code, git history, and ledger context to produce
  a structured readiness assessment, authors `plan.md` +
  `review-criteria.md` artefacts. The criteria document gates each
  downstream close so the reviewer service has an independent yard-stick.
- **develop** — writes the code changes that satisfy the story's
  acceptance criteria, runs the project's build / test / lint gates
  locally, stages and commits the work via the project's conventional-
  commit format. Closes the develop task via
  `task_update(id=<task_id>, status=closed, evidence_ledger_ids=[…])`;
  reviewer dispatch (where the contract requires one) is the
  orchestrator's next plan step, not a side effect of closure.

The project's gates and any language-specific tooling come in via the
agent's `skill_refs:` (resolved at dispatch time) and the operator-side
`--allowedTools` envelope, not from this agent's body. The agent
declares the role; the project supplies the toolbox.

## How

The agent surface bundles the union of permission patterns each phase
needs. Capability is declared via the `delivers:` frontmatter list
(`contract:plan`, `contract:develop`); the substrate matches at
task-creation time so the orchestrator can supply this agent's id
on either kind=work task without a separate alias table.

## Lifecycle (claim → work → evidence → close)

Once the orchestrator dispatches this agent on a task:

1. **Claim** — `task_claim(task_id)` to take ownership.
2. **Work** — for plan: read story + ledger + agent + contract,
   author `plan.md` + `review-criteria.md`. For develop: read the
   accepted plan, edit + run the project's gates + commit.
3. **Evidence** — `ledger_append(...)` for every artefact produced
   (plan markdown, review criteria, commit SHA, gate output). The
   reviewer reads these against the contract's rubric.
4. **Close** — `task_update(id=<task_id>, status=closed,
   outcome=success|failure, evidence_ledger_ids=[…])`. Closure
   mutates only the target task; reviewer dispatch (where the
   contract requires one) is the orchestrator's next plan step.
   Do not push, merge, or close the story.

## Develop close-evidence requirement — `files_changed` block

When closing a `contract:develop` task, the develop close evidence
row (`kind:evidence, phase:develop`) MUST carry a structured
`files_changed` block in addition to the markdown content. The
reviewer rubric uses this to apply mechanical AC checks
(e.g. files-changed count non-zero) without grepping prose.

Resolve the block at close time as follows:

1. **Resolve `<base>` from the branch suffix.** The work branch is
   named `client-<task_id>-from-<base_sha>` per the worktree
   template. Extract `<base_sha>` from the current branch name
   (e.g. `git rev-parse --abbrev-ref HEAD` → split on `-from-`).
2. **Run `git diff --name-status <base>..HEAD` inside the
   worktree.** The output lists one file per line, status letter
   first then a tab and the path.
3. **Parse status letters into three arrays:**
   - `A` → `added`
   - `M` → `updated`
   - `D` → `deleted`
   - `R<num>` (rename) → `updated` (use the new path; the rename
     pair is `R<score>\t<old>\t<new>` — capture only `<new>`)
4. **Pass the JSON payload via `--structured` on the close-evidence
   `ledger append`:**

   ```bash
   satellites-client ledger append \
     --project-id <pid> \
     --type evidence \
     --content "..." \
     --tags "task_id:<task_id>,kind:evidence,phase:develop" \
     --structured '{"files_changed":{"added":["a.go"],"updated":["b.go"],"deleted":[]}}'
   ```

   Both keys (`files_changed`, `tokens_used`) are independent and
   may co-exist on one row. The `tokens_used` block is populated
   automatically by the dispatcher's stream-json aggregator on the
   `kind:agent-execute-evidence` row — develop close evidence rows
   only need to carry `files_changed`.

This block is OPTIONAL on the schema (backwards-compat). Phases
that don't touch files — plan, review, commit, merge_to_main —
MUST omit `files_changed` from their close-evidence rows. The
reviewer treats absence as backwards-compat, not a reject.

## Out of scope

- `git push` — that belongs to the **releaser** role.
- `git merge --ff-only` — that belongs to the **releaser** role.
- Story closure / reviewer transition — that belongs to the
  **story_close** role.

## Why role-shaped, not contract-shaped

A contract-shadow agent (one agent per contract) duplicates the
contract document's `agent_instruction` field and forces an alias
table at the orchestrator's plan composer. The role-shaped agent
satisfies the design's ≥2-contracts test (it cleanly drives two
contracts with one shared permission set + one shared playbook) and
keeps the agent catalog small: one row per role, not one per slot.
