---
id: pr_substrate_provides_context
name: substrate_provides_context
scope: project
tags:
  - process
  - dispatch
  - context
  - v4
---
The substrate stores documents per tier (agent, contract, principle, story, ledger evidence) and exposes per-verb retrieval. Dispatched agents collate the context they need by calling those verbs themselves; the substrate does not assemble or supply the agent's working context at dispatch time. Operator-side Claude Code memory does not flow into a dispatched agent.

## What this means

A dispatched agent runs in its own subprocess with its own `HOME` and its own `~/.claude` directory. It does not see the operator's memory directory at `~/.claude/projects/.../memory/`. It does not inherit the operator's open Claude Code conversation context.

Per-verb retrieval is the model. `story_get` is the dispatch entry point — it returns the story body and the reference ids the orchestrator has pointed the agent at. From there the agent fetches what it needs:

- `agent_get(name=…)` for its own profile, voice, capability list, permission envelope.
- `contract_get(name=…)` for the rubric the action it is performing must satisfy.
- `principle_list(active_only=true, project_id=…)` for the system + project principles in force.
- `task_walk(story_id=…)` for the chain — including any `prior_task_id` and the verdict that triggered a retry.
- `ledger_*` for prior evidence rows the agent needs to read or extend.

The orchestrator's contribution at dispatch is `task_add(prompt=…)` — the rich instruction naming the story id, task id, agent role, and the explicit work the agent should execute. The dispatched bash subprocess carries only that thin pointer. There is no `dispatch_context` aggregate verb and the substrate is not growing one: per-verb retrieval keeps the substrate small and matches the configuration-over-code mandate.

## What it forbids

- Relying on operator-side Claude Code memory to shape dispatched-agent behaviour. Memory at `~/.claude/projects/.../memory/` is orchestrator-only.
- Treating the substrate as a context-assembly service at dispatch. The substrate stores rows and exposes per-verb retrieval; assembling the agent's working context is split between the orchestrator's `task_add(prompt=…)` and the agent's own MCP calls.
- Proposing a `dispatch_context` bundle verb in this principle's downstream stories. Per-verb retrieval is the model. A bundle verb would couple agents to a fixed shape and contradict configuration-over-code.
- Hard-coding agent behaviour in Go. Agent profiles live in `config/seed/agents/`. Changing how an agent works = editing that markdown, not changing the dispatch code.

## Citation

This principle backs the dispatch-loop sections in the `default_agent_process` artifact and the `claude_orchestrator` agent doc. It is paired with `pr_reviewer_voice_authoritative` (the orchestrator's response to rejection): together they describe how the orchestrator-driven dispatch loop operates.
