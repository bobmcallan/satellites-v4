---
id: pr_substrate_provides_context
name: Substrate provides context to dispatched agents
scope: system
tags:
  - process
  - dispatch
  - context
  - v4
---
The substrate is the authoritative source of context for dispatched agents. Memories, principles, agent process, contract bodies, story context, and task evidence chains must be assembled by the substrate at dispatch time and supplied to the agent — not inherited from the operator's local Claude Code installation.

## What this means

A dispatched agent runs in its own subprocess with its own `HOME` and its own `~/.claude` directory. It does not see the operator's memory directory at `~/.claude/projects/.../memory/`. It does not inherit the operator's open Claude Code conversation context. Whatever shape the substrate provides at dispatch time IS what the agent has to work with.

For dispatched agents to behave correctly, the substrate dispatch primitive must compose:

- The current `default_agent_process` artifact body (so the agent reads the same fundamentals + dispatch loop the orchestrator does).
- The agent doc body for the dispatched role (so the agent knows its own profile, voice, capability list, permission envelope).
- All active principles (system + project scope), or at minimum the principles cited by the agent's rubric.
- Story context — story body, AC, fields, recent ledger evidence — for the story the task belongs to.
- The contract document body for the action being dispatched (so the agent's evidence package satisfies the rubric).
- The relevant slice of `task_walk(story_id)` so retry tasks see the prior_task_id chain and the verdict that triggered the retry.

## What it forbids

- Relying on operator-side Claude Code memory to shape dispatched-agent behaviour. Memory at `~/.claude/projects/.../memory/` is orchestrator-only.
- Curating the agent's prompt to include only "what we think it needs". The substrate provides everything load-bearing; the agent fetches anything else via MCP itself.
- Hard-coding agent behaviour in Go. Agent profiles live in `config/seed/agents/`. Changing how an agent works = editing that markdown, not changing the dispatch code.

## Citation

This principle backs the `## dispatch loop` section in `config/seed/artifacts/default_agent_process.md` and the matching `### Dispatch loop` in `config/seed/agents/claude_orchestrator.md`. It is paired with `pr_reviewer_voice_authoritative` (orchestrator's response to rejection) and `pr_mandate_reviewer_enforced` (the floor of every plan): together they describe how the orchestrator-driven dispatch loop operates.
