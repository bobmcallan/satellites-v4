# Contract↔Agent Separation

Status: design note backing **story_b7bf3a5f**.

## Principle

A v4 **contract** defines the *audit shape* of a phase: what category it
belongs to, what evidence closes it, which categories its skills must
satisfy, what validation mode the reviewer applies. A contract does not
say *how* the work is done.

A v4 **agent** defines the *execution shape*: which tools it may use
(`permission_patterns`), which skills it carries (`skill_refs`), and —
optionally — the strategy text that briefs an executor on the in/out of
scope for the phase. Different agents can satisfy the same contract with
different execution shapes.

A v4 **reviewer** (`type=reviewer` with `contract_binding`) defines the
*assessment shape*: per-AC rubrics that judge whether a delivery's
evidence satisfies the contract's evidence requirements.

Three documents, three concerns. The contract is the *invariant*; agents
and reviewers are *plug-in points* around it.

## Why

Coupling agent execution constraints to the contract document was a v3
shortcut. Three things break under that shortcut:

1. **Contract version-bumps for unrelated reasons.** When a develop
   agent gains permission to run a new build tool, every project's
   contract document had to version-bump even though the contract's
   audit shape was unchanged. The contract row's history reflected
   agent toolkit churn rather than phase-shape evolution.
2. **Reviewer reads the wrong document.** Trust in a delivery comes from
   the evidence the reviewer can audit (`pr_0c11b762`,
   `pr_evidence`). When the reviewer asks "what was this CI authorised
   to do?", the answer must come from the **claim**, which records the
   agent. Reading agent capabilities off the contract conflated the
   *what's allowed* question with the *what's required* question.
3. **Multiple agents per contract is awkward.** A single develop
   contract can be satisfied by Claude (broad toolkit) or by a
   narrower agent (e.g. mechanical-runner with a smaller set). With
   permitted_actions on the contract, both agents had to share one
   list — so the list grew to the union of every agent's needs, which
   is the wrong direction for least-privilege.

Per `pr_a9ccecfb` (story is the unit of work) and `pr_0c11b762`
(evidence is the trust leverage), the cleanest split puts execution
constraints on the agent so each agent carries its own audit-grade
record of what it asked to do.

## What's already shipped

The substrate side of the refactor was delivered in two upstream
stories:

- **story_b39b393f** — `agent_id` is required on every
  `contract_claim`. The action-claim handler resolves
  `permission_patterns` from the claiming agent's document; the CI is
  stamped with the agent_id.
  - File: `internal/mcpserver/claim_handlers.go:78` — agent doc lookup
    + `Source: "agent_document"` on the action-claim row.
  - Test: `internal/mcpserver/agent_id_claim_test.go:91` —
    `payload.Source != "agent_document"` is a failure.
- **story_cc55e093** — caller-supplied `permissions_claim` is retired.
  The `contract_claim` handler returns `permissions_claim_retired` if
  the caller passes one.
  - Test: `internal/mcpserver/agent_id_claim_test.go:144`
    (`TestClaim_RejectsPermissionsClaim`).

So at v4 substrate time, the contract's `permitted_actions` field is
already not consulted by the action-claim path. It was dead data.

## What story_b7bf3a5f closes out

This story does the data-cleanup follow-up to those substrate changes:

1. `internal/configseed/parsers.go contractToInput` no longer writes
   `permitted_actions` into the contract Structured payload.
2. The six lifecycle contract markdown files in `config/seed/contracts/`
   have the `permitted_actions:` frontmatter block removed.
3. New regression test
   `TestRun_ContractStructuredOmitsPermittedActions` in
   `internal/configseed/runner_test.go` asserts the loader ignores the
   key even when present in frontmatter.
4. The six seeded lifecycle agents in `config/seed/agents/` gain an
   `instruction` frontmatter key — the canonical home for agent-level
   execution guidance (AC 2). Two regression tests in
   `internal/configseed/runner_test.go` —
   `TestRun_AgentStructuredCarriesInstruction` (synthetic) and
   `TestRun_RealSeedAgentsCarryInstruction` (real seed dir) — assert
   the field round-trips into the agent's Structured payload via the
   existing `mergeFrontmatterIntoJSON` path.

## Why no backwards-compat fallback (story AC 4)

The story's AC 4 asked for a deprecation window where the substrate
would fall back to a contract's `permitted_actions` when an agent
didn't declare any. **No such fallback exists or is reachable**, by
construction:

- `internal/mcpserver/claim_handlers.go:48` — `agent_id` is required
  on every `contract_claim`. Missing `agent_id` returns
  `agent_required` (test:
  `internal/mcpserver/agent_id_claim_test.go:121`
  `TestClaim_RejectsMissingAgentID`).
- `internal/mcpserver/claim_handlers.go:78` — once `agent_id` is
  present, `permission_patterns` are resolved from the agent
  document's `Structured.permission_patterns`. There is no second
  branch reading from the contract.
- `internal/mcpserver/claim_handlers.go` — caller-supplied
  `permissions_claim` arg is rejected with `permissions_claim_retired`
  (test: `internal/mcpserver/agent_id_claim_test.go:144`
  `TestClaim_RejectsPermissionsClaim`).

So the fallback path the AC envisioned would have to be added to a
substrate that *currently has no such path*. Adding a fallback now
would (a) re-introduce the contract-coupling this story removes and
(b) violate `pr_no_unrequested_compat` (no compat layers without an
explicit AC requiring them — this AC asked for a fallback
*conditional on* the existing substrate, and the existing substrate
doesn't reach the conditional). The honest answer is that AC 4 is
moot because the upstream substrate already closed the door on the
contingency it tried to plan for.

If a future change ever re-opens the door (e.g. someone proposes
making `agent_id` optional), the design rule from this story is:
**still no contract-side fallback** — instead, define a default
agent in the workspace and route claims through it. The contract
remains pure audit shape regardless.

## What's deliberately left in place

- **The contract body markdown** still describes the phase ("what it
  does", "how", "limitations"). This is generic, useful prose that
  applies regardless of the executing agent. It is not "agent
  instruction" — it's the contract's documentation of itself, which
  belongs on the contract.
- **`evidence_required`** stays on the contract. It's the canonical
  description of what the reviewer expects at close — the audit shape
  itself.

## Future extensions (out of scope here)

- **Agent-side `instruction` field.** Agents could carry a structured
  `instruction` markdown block tuned to their execution shape. Not
  added in this story because the substrate doesn't read it; adding
  inert structure is the kind of unrequested abstraction
  `pr_no_unrequested_compat` warns against. File a follow-up if
  evidence emerges that the field is needed.
- **Multiple agents per contract.** The data model already supports
  it; the orchestration policy that picks which agent claims which
  CI is independent of this story's data shape and can evolve
  separately.

## Citations

- `pr_a9ccecfb` — Story is the unit of work; epics are tags.
- `pr_0c11b762` — Evidence is the primary trust leverage.
- `pr_no_unrequested_compat` — No unrequested abstractions or
  backwards-compat layers.
- `story_b39b393f` — agent_id-sourced permission_patterns on
  contract_claim.
- `story_cc55e093` — permissions_claim retired.
- `story_b7bf3a5f` — this story (data cleanup + design note).
