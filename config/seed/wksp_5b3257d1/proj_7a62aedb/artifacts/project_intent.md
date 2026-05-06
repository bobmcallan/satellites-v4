---
name: project_intent
tags: [kind:project-intent]
---
# Satellites — project intent

Satellites is a **customisable mapped business process for the
implementation of user stories**. Concretely: a configuration-over-code
substrate where multiple agents collaborate on stories, and the
substrate's behaviour is shaped by markdown documents (agents,
contracts, principles, workflows, artifacts) rather than by branches in
Go code. The substrate stores the documents, validates their structural
shape, and serves them on demand. The agents — orchestrators, dispatched
workers, reviewers — read those documents to know what to do.

## What this means in practice

- **Stories are units of work.** Every change ties to a story id.
  There is no work outside a story, no commit that doesn't reference
  one, no review that isn't anchored to one. Story state and the
  ledger rows attached to it are the audit trail.
- **The substrate is the data plane, not the policy plane.** It
  persists rows, enforces structural invariants (scope rules, FK
  integrity, kind-specific frontmatter validation), and emits ledger
  events. It does not decide what's *right* — that lives in the
  documents agents read.
- **Agents act independently from substrate data.** A dispatched
  Claude subprocess invoked as `bash claude -p 'implement task_xxx'`
  reads its task, agent doc, project intent, principles, and contract
  via MCP using only the task id. No operator-side context injection,
  no chat-time hand-holding. If the agent can't act on the prose
  alone, the prose is incomplete — fix the document, not the agent.
- **Prose is authoritative.** When a rule needs to land, it lands as
  markdown that the seed loader picks up and the runtime reads. New
  behaviour comes from new prose and a re-seed, not from a code
  change. If an agent has to infer a rule from training data or
  environment context, the substrate has failed.

## Non-negotiables for work in this project

- **Configuration over code.** Adding a verb, a contract, a principle,
  a workflow is fundamentally a documentation task that the code
  supports — not a code task with documentation as an afterthought.
  When tempted to express a rule as a Go branch, ask whether prose +
  data can carry it instead.
- **Seed prose is self-sufficient.** A reader given only a document's
  body should be able to act on it. Repo-internal references
  (`docs/...`, `config/...`, `internal/...`) leak the local file
  layout into the prose surface and break dispatched-agent contexts
  that don't carry the repo. Cross-reference by document id, not by
  path.
- **Story = unit of delivery, task = unit of dispatch.** A story
  carries a goal; tasks decompose the work into agent-claimable
  units. The plan is the ordered task chain. There is no separate
  "workflow table" — the task list is the workflow.
- **Evidence is verifiable.** Closing a task means producing a ledger
  row a reviewer (human or agent) can check. Hand-waving doesn't
  close. The reviewer's verdict is operator authority — rejection
  spawns a successor task, not a rationalisation.
- **Process is trust.** When the process feels arbitrary, ask why
  before working around it. The reason is usually a past incident or
  a constraint not yet visible. Operate inside the process; refine
  it by writing a new principle or contract, not by skipping it.

## Reading guide for an agent acting in this project

- The bound project + the intent above tell you **why** you're
  working.
- Active principles (returned alongside this intent) tell you
  **what guardrails apply** universally and project-specific.
- Your agent doc (fetched via `agent_get`) tells you **your role,
  capabilities, and rubric**.
- The contract attached to your task (if any) tells you **what
  evidence the close requires**.
- The story + task chain tells you **where you sit in the larger
  arc**.

If any of those layers feels missing or contradictory, that's a
substrate issue — file it. The substrate's job is to deliver enough
context that the work can be done from prose alone.
