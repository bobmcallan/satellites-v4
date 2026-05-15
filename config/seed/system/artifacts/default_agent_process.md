---
name: default_agent_process
tags: [kind:agent-process, v4]
---
# satellites · agent process

Satellites is a **customisable mapped business process for the
implementation of user stories**. Configuration-over-code substrate:
agents, contracts, principles, and workflows are markdown documents.
New behaviour comes from new prose and a re-seed, not from editing
code.

## Primitives

- **projects** — top-level work surface; carry intent + active principles.
- **stories** — units of deliverable work (user stories) scoped to a project.
- **tasks** — the dispatch unit; what a single agent acts on.
- **documents** — typed markdown: agents, contracts, principles, skills, workflows, help.
- **ledger** — append-only audit log; evidence and verdicts.

## Bootstrap

When a user prompt references substrate primitives (story id, task
id, contract name, agent name) or asks about the project, make
`project_set(repo_url=<git remote get-url origin>)` your first call.
It binds your session to the project, returns the project's intent
prose and active principles in one roundtrip, and lets every
subsequent project-scoped verb default to the bound project.

For unrelated prompts the bootstrap is unnecessary. The handshake
orients — it does not demand a roundtrip on every turn.

`project_get(id=<project_id>)` returns the same orientation bundle
without re-resolving the repo URL. Use it on later turns when the
intent or principles need a refresh.

## satellites_init install sequence

The `satellites-client` binary lives at
`./.satellites/satellites-client` colocated under the consumer
project root. When it is missing or out of date, call the
`satellites_init` MCP verb and **execute the steps below using the
fields it returned** — do not ask the user what to do, the payload
is authoritative.

Branch on `state` and `auth_bootstrap.kind`:

- `state == "up_to_date"` → nothing to do. Continue with the user's
  task.
- `state ∈ {"install_required", "update_available"}` AND
  `auth_bootstrap.kind == "ready"`:
  1. `curl -fsSL "${install.download_url}" -o "${target_install_path}"`
  2. `printf "%s  ${target_install_path}\n" "${install.sha256}" | sha256sum -c -` — abort on mismatch.
  3. `chmod +x "${target_install_path}"`
  4. Write `${target_config_path}` from `default_config` + the
     `agent_api_key.key` field on the payload (cleartext is non-empty
     only on a fresh mint; on `source=="existing_key"` the operator
     already has the key from a prior install).
  5. `"${target_install_path}" agent verify` — exits 0 when the
     install is healthy.
- `state ∈ {"install_required", "update_available"}` AND
  `auth_bootstrap.kind == "auth_login"`: run
  `satellites-client auth login` first to mint the OAuth bearer, then
  re-run `satellites_init` — the new bound session will return
  `kind=ready` with the apikey block, and you can proceed with the
  install steps above.

Re-running `satellites_init` on a project-bound session is
idempotent: the apikey row is keyed by (caller, project, agent_name)
so a second call returns the existing key's metadata.

## Fetching context

The default substrate surface is the `satellites-client` CLI invoked
via Bash — grouped by noun (`task get <id>`, `ledger append --type
evidence ...`, `story update-status ...`). Auto-JSON when stdout is
not a tty; pipe to `jq`. Auth + server URL resolve from the loader's
config chain (flag > env > .satellites/ > satellites/ (legacy) >
bin/ > XDG).

The `mcp__satellites__*` verbs in your tool list are the equivalent
shape, 1:1 with the CLI verbs. Names and parameters in either form
are authoritative.

## Operating principle

Read the documents that describe your role, your project, and your
task. Act on what they say. Write evidence to the ledger. Prose is
authoritative — fetch rules, do not infer them.

## Async dispatch

When the orchestrator has more than one story (or more than one
slice of the same story) in flight at once, dispatch via
`task run` (default async branch) and poll the chain instead of
blocking the orchestrator's bash with `task run --sync`. The full
pattern — author → run → poll → consume the
`kind:agent-execute-evidence` row → recover from daemon crashes via
`kind:daemon-orphaned-subprocess` rows + `prior_task_id` retries —
lives in the sibling artifact `default_async_dispatch`. Polling
cadence is 30s default, 30-60s sustainable band.
