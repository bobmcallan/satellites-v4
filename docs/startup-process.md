# Session startup timeline

Chronological trace of what happens between `claude` being invoked
in a satellites working directory and an orchestrator producing its
first substantive reply. The authoritative rules live in
`config/seed/system/artifacts/default_agent_process.md` — this
doc is the timeline view of those rules in action.

## T+0 — Claude Code starts

Operator runs `claude` in `/home/bobmc/development/satellites`.
The CLI reads `.mcp.json` at the working-directory root:

```json
{
  "mcpServers": {
    "satellites": {
      "type": "http",
      "url": "https://satellites-pprod.fly.dev/mcp"
    }
  }
}
```

## T+1 — MCP transport handshake

Claude Code opens HTTP transport to the satellites MCP server,
performs the MCP `initialize` handshake. The server assigns a
session id via the `Mcp-Session-Id` response header.

**Context returned to the agent at T+1:**

| Field | Source | Content |
|-------|--------|---------|
| `serverInfo.name`, `serverInfo.version` | MCP server | `satellites` + version string |
| `instructions` | `config/seed/system/artifacts/default_agent_process.md` | The agent-process prose — primitives, bootstrap rule, fetching-context rule, operating principle. Rendered to the agent as a system reminder under `## satellites`. |
| `capabilities.tools` | MCP server | Tool catalogue: ~110 `mcp__satellites__*` verb names advertised as **deferred** tools (schemas not loaded until `ToolSearch(query="select:<name>")` fetches them). |
| `Mcp-Session-Id` header | MCP server | Opaque session id; auto-registers the session row server-side. |

No project binding exists yet. Any project-scoped verb at this
stage returns `no_project_for_remote`. The `instructions` block is
authoritative for the bootstrap rule that fires at T+3/T+4.

## T+1.5 — Verify CLI presence

Before relying on the CLI default surface, the agent runs a
one-shot health check using the verb that actually ships today:

```bash
command -v satellites-client && satellites-client info
```

`info` (`cmd/satellites-client/reads.go:71`) delegates to
`satellites_info` and returns the server version + caller
identity. A clean return implicitly proves three things:

1. The binary is on PATH.
2. The auth/server config chain (flag > env > bin > XDG) resolved
   to a working token + URL.
3. The remote server is reachable on this version.

Three branches:

| Outcome | Agent action |
|---------|--------------|
| Binary present, `info` returns server + user identity | CLI default surface is live. Proceed to T+2 using `satellites-client …` for every substrate verb. |
| Binary present, `info` errors (auth / network / version skew) | Surface the error to the operator with the literal CLI output. Do not fall through to MCP silently — the operator needs to know the CLI surface is degraded. |
| Binary absent (`command -v` empty) | Fall back to `mcp__satellites__*` verbs and emit install instructions to the operator. |

### Target state (`sty_64e69db8`, backlog)

A dedicated `satellites_init` MCP verb + matching CLI subcommand
is on the backlog. When it lands it will fold the three branches
above into a single contract: presence check, version probe, and
install/upgrade instructions in one response. Today the agent
composes the same outcome from `command -v` + `info` because
neither `satellites-client init` nor `mcp__satellites__satellites_init`
exists yet.

This step is the bridge between the seed's "CLI is the default
surface" rule and the reality that a fresh operator workstation
may not have the binary installed yet. Without it, the agent
either silently uses the MCP fallback (hiding the install gap)
or hits a confusing `command not found` on the first verb call.

## T+2 — Operator types a prompt

E.g. `review epic:cli-primary`. The prompt references a substrate
primitive (epic tag), which by the seed's bootstrap rule triggers
the orientation roundtrip.

## T+3 — Resolve the project from the git remote

```bash
git remote get-url origin
# → git@github.com:bobmcallan/satellites.git
```

The git remote is the canonical key the substrate uses to map a
working directory to a `project_id`.

## T+4 — `project_set` bootstrap

Default surface (CLI via Bash, per `default_agent_process.md`):

```bash
satellites-client project set --repo-url "$(git remote get-url origin)"
```

MCP fallback (used when no `satellites-client` resolves on PATH):

```
mcp__satellites__project_set(repo_url="git@github.com:bobmcallan/satellites.git")
```

**Context returned to the agent at T+4:**

| Field | Content |
|-------|---------|
| `project_id` | The substrate's id for the bound project (e.g. `proj_7a62aedb`). |
| `status` | `resolved` if the remote mapped, `no_project_for_remote` otherwise. |
| `repo_url_canonical` | Normalised form of the remote (server-side canonicaliser, same as `project_create` uses). |
| `intent_body` | Full markdown body of the project's intent doc. |
| `principles[]` | Array of every active principle (workspace + project scope), each with `name`, `scope`, full `body`. ~20 principles in this project. |
| `mcp_url` | `https://satellites-pprod.fly.dev/mcp?project_id=proj_7a62aedb` — the sticky-bound form. |
| `mcp_config` | Drop-in `.mcp.json` block carrying the sticky-bound URL. |

After this call:
- The session is registered server-side against `project_id`.
- Project-scoped verbs default to the bound project — `project_id`
  is no longer required on follow-up calls.
- Active principles are now in the agent's working context;
  reviewer rubrics and the `pipeline_authority` rule can be
  honoured without re-fetching.

## T+5 — Fetch the work surface

For the epic-review prompt: list stories scoped to the epic tag.

CLI:

```bash
satellites-client story list --tag epic:cli-primary --limit 100
```

MCP fallback:

```
mcp__satellites__story_list(
  project_id="proj_7a62aedb",
  tag="epic:cli-primary",
  limit=100,
)
```

## T+6 — Handle oversized results

For a mature epic, the response exceeds the MCP token budget; the
substrate spills to disk and returns a pointer file:

```
Error: result (225,576 characters) exceeds maximum allowed tokens.
Output has been saved to .../tool-results/mcp-satellites-story_list-*.txt
```

Recover with `jq`:

```bash
jq -r '.[0].text' /path/to/spill-file \
  | jq '[.[] | {id, title, status, priority, tags: (.tags // [])}]
        | sort_by(.status, .priority)'
```

CLI form: `--output json | jq '...'` (auto-JSON when stdout is not
a tty, per the seed).

## T+7 — Synthesise and reply

The orchestrator reads the projected rows + the principles bundle
from T+4 and produces the review. Tag axes (`parent:`, `blocks:`,
`order:*`, `followup:`) drive the dependency narrative; statuses
drive the done/backlog/cancelled grouping.

## T+N — Subsequent turns

### Refresh orientation

```
mcp__satellites__project_get(id="proj_7a62aedb")
```

Returns the same bundle as T+4 — `project_id`, `intent_body`,
`principles[]`, `mcp_url`, `mcp_config`, `repo_url_canonical`,
`status` — without re-resolving the repo URL. Use when the intent
or principles may have changed mid-session.

### Dispatch-time context (story_get)

When the orchestrator dispatches a task, `story_get(id=sty_*)` is
the entry-point call the dispatched agent makes. It returns:

| Field | Content |
|-------|---------|
| `story` | Row body, status, fields, tags. |
| `project` | Owning project orientation (id + intent + principles, same shape as `project_set`). |
| `ledger[]` | Recent ledger evidence rows for this story. |
| `agent_process` | The resolved `default_agent_process.md` body — same content as the T+1 `instructions` field, fetched fresh so prose edits + re-seed are observed. |
| `category_template` | The story-category template (development / followup / etc.). |

This is how `default_agent_process.md` reaches a *dispatched*
agent, which never went through T+0/T+1 against the parent's
session.

### Skip refresh

For prompts that don't reference substrate primitives, no further
MCP roundtrip is needed. The T+1 server instructions + T+4
orientation bundle remain in context.

## Making the binding sticky

Persist the `mcp_config` block returned by `project_set` into
`.mcp.json` (replacing the bare URL with the `?project_id=` form).
Future sessions skip T+3 and T+4 — the very first MCP call
already lands on the bound project.

## Sequence summary

```
T+0    claude starts, reads .mcp.json
T+1    MCP initialize → session id + serverInfo + instructions
                        (= default_agent_process.md) + deferred tools
T+1.5  command -v satellites-client && satellites-client info
       → CLI present? auth ok? else install instructions / MCP fallback
       (future: single `satellites_init` verb per sty_64e69db8)
T+2    operator prompt references a substrate primitive
T+3    git remote get-url origin
T+4    project_set → {project_id, intent_body, principles[],
                      mcp_url, mcp_config, repo_url_canonical, status}
T+5    story_list (tag-scoped) or other work-surface verb
T+6    spill-file recovery if oversized
T+7    synthesise reply
T+N    project_get refresh, or story_get for dispatch-time context
       (story + project + ledger + agent_process + category_template)
```

## Alignment with `default_agent_process.md`

The seed is the authoritative surface for agent behaviour; this
timeline is its operational trace. Coverage check:

| Step | Seed coverage | Notes |
|------|---------------|-------|
| T+0, T+1 | Not covered | Transport-level, before any agent action — appropriate to leave out of the seed. The seed body IS what the MCP server returns at T+1, so it can't describe its own delivery. |
| T+1.5 (CLI health check) | **Not covered** | Gap. The seed says CLI is the default surface but does not tell the agent what to do when the binary is absent or degraded. Candidate seed edit (gated on `sty_64e69db8`). |
| T+2 → T+4 (bootstrap) | Covered | `## Bootstrap` section — `project_set(repo_url=…)` as the first call when a prompt references substrate primitives. |
| T+5 (CLI default surface) | Covered | `## Fetching context` — CLI grouped by noun, MCP fallback, config-chain auth. |
| T+6 (spill recovery) | Not covered | Token-budget recovery is a tool-mechanics detail; lives better in a CLI help note than in the seed. |
| T+N refresh (`project_get`) | Covered | `## Bootstrap` final paragraph. |
| T+N dispatch (`story_get` returns `agent_process`) | Implicit | The seed itself flows back through `story_get.agent_process` to dispatched agents, but the seed does not name that delivery path. Worth a line. |

Concrete seed gaps worth filing as follow-ups:

1. **CLI health-check rule** — a `## CLI presence` section telling
   the agent to run `satellites-client init` first, with the
   three-branch behaviour from T+1.5. Folds the install-instructions
   contract into the substrate. Blocked on `sty_64e69db8` shipping
   the verb.
2. **Dispatched-agent entry** — a line in `## Bootstrap` noting
   that dispatched agents enter via `story_get` (not `project_set`)
   and receive the same seed via `agent_process` in that response.
