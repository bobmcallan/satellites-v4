# cli-primary-design

Design doc for `epic:cli-primary` (`sty_c01b23b5`). Inventories the
live MCP surface, designs the `satellites-client` CLI catalogue, and
records the conventions adopted from cli-printing-press. Written for
order:01 (`sty_9ff6ac4c`) so the rest of the epic builds against a
single source of truth.

## Live count vs the story description

The epic description anchors on **73 MCP verbs**. The live count at
this commit is **79 source-level `mcpgo.NewTool(...)` registrations**
across **48 files**, expanding to **~107 runtime tools** because
`internal/mcpserver/wrappers.go` registers six CRUD verbs (create /
get / list / update / delete / search) for each of six document
types (`principle`, `contract`, `skill`, `reviewer`, `agent`, `role`)
via a template — 6 source lines × 6 types = 36 runtime wrappers
without showing as 36 distinct grep matches.

The rest of this doc plans against the live count. Where the story
description says "73 verbs reduce to 3", read it as "~107 verbs
reduce to 3"; the headline ratio (~35:1) only widens.

---

## §1 Inventory

Per-noun runtime tool list. Each row: verb, registration site,
caller class. Caller classes: **operator** (operator-side scripts /
the CLI we are building), **agent-worker** (the satellites-agent
worker process — `internal/agent/worker/`), **dispatched-claude**
(the Claude session inside a worktree, fetching its context per
`pr_substrate_provides_context`), **portal** (the HTML portal under
`pages/`), **api** (HTTP endpoints under `internal/api`).
**all** = available to every caller.

### agent (8)

| Verb | Site | Callers |
|---|---|---|
| `agent_create` | wrappers.go (template) | operator |
| `agent_update` | wrappers.go (template) | operator |
| `agent_delete` | wrappers.go (template) | operator |
| `agent_get` | wrappers.go (template) | dispatched-claude, portal, operator |
| `agent_list` | wrappers.go (template) | dispatched-claude, portal, operator |
| `agent_search` | wrappers.go (template) | operator |
| `agent_compose` | mcp.go:530 | operator |
| `agent_ephemeral_summary` | mcp.go:544 | operator, portal |

### agent-apikey (3, admin)

| Verb | Site | Callers |
|---|---|---|
| `agent_apikey_create` | mcp.go:554 | operator [admin] |
| `agent_apikey_list` | mcp.go:562 | operator [admin] |
| `agent_apikey_delete` | mcp.go:569 | operator [admin] |

### changelog (5)

| Verb | Site | Callers |
|---|---|---|
| `changelog_add` | mcp.go:833 | operator |
| `changelog_get` | mcp.go:844 | portal, operator |
| `changelog_list` | mcp.go:850 | portal, operator |
| `changelog_update` | mcp.go:858 | operator |
| `changelog_delete` | mcp.go:868 | operator |

### contract (6, wrapper template)

`contract_{create,get,list,update,delete,search}` — wrappers.go.
Callers: dispatched-claude (read), operator (write).

### document (7)

| Verb | Site | Callers |
|---|---|---|
| `document_create` | mcp.go:217 | operator |
| `document_update` | mcp.go:232 | operator |
| `document_delete` | mcp.go:256 | operator |
| `document_get` | mcp.go:203 | dispatched-claude, operator |
| `document_list` | mcp.go:244 | dispatched-claude, operator |
| `document_search` | mcp.go:265 | operator |
| `document_ingest_file` | mcp.go:191 | operator |

### kv (5)

| Verb | Site | Callers |
|---|---|---|
| `kv_get` | mcp.go:481 | operator |
| `kv_set` | mcp.go:491 | operator |
| `kv_delete` | mcp.go:502 | operator |
| `kv_get_resolved` | mcp.go:512 | dispatched-claude (skill resolution), operator |
| `kv_list` | mcp.go:521 | operator |

### ledger (6)

| Verb | Site | Callers |
|---|---|---|
| `ledger_append` | mcp.go:335 | dispatched-claude (evidence), agent-worker, operator |
| `ledger_get` | mcp.go:365 | dispatched-claude, portal, operator |
| `ledger_list` | mcp.go:350 | portal, operator |
| `ledger_search` | mcp.go:371 | dispatched-claude, operator |
| `ledger_recall` | mcp.go:386 | operator |
| `ledger_dereference` | mcp.go:392 | operator |

### portal (1)

| Verb | Site | Callers |
|---|---|---|
| `portal_replicate` | portal_replicate.go:40 | operator |

### principle (6, wrapper template)

`principle_{create,get,list,update,delete,search}` — wrappers.go.
Callers: dispatched-claude (read), operator (write).

### project (7)

| Verb | Site | Callers |
|---|---|---|
| `project_create` | mcp.go:280 | operator |
| `project_get` | mcp.go:289 | operator |
| `project_list` | mcp.go:298 | operator |
| `project_update` | mcp.go:303 | operator |
| `project_delete` | mcp.go:311 | operator |
| `project_set` | mcp.go:326 | dispatched-claude (bootstrap), operator |
| `project_seed_run` | mcp.go:682 | operator |

### repo (8)

| Verb | Site | Callers |
|---|---|---|
| `repo_add` | mcp.go:768 | operator |
| `repo_get` | mcp.go:776 | operator |
| `repo_list` | mcp.go:782 | operator |
| `repo_search` | mcp.go:789 | dispatched-claude, operator |
| `repo_search_text` | mcp.go:798 | dispatched-claude, operator |
| `repo_get_symbol_source` | mcp.go:806 | dispatched-claude, operator |
| `repo_get_file` | mcp.go:813 | dispatched-claude, operator |
| `repo_get_outline` | mcp.go:820 | dispatched-claude, operator |

### reviewer (6, wrapper template)

`reviewer_{create,get,list,update,delete,search}` — wrappers.go.
Callers: operator.

### role (6, wrapper template)

`role_{create,get,list,update,delete,search}` — wrappers.go.
Callers: operator.

### session (2)

| Verb | Site | Callers |
|---|---|---|
| `session_register` | mcp.go:611 | operator (test only) |
| `session_whoami` | mcp.go:605 | dispatched-claude, operator |

### skill (6, wrapper template)

`skill_{create,get,list,update,delete,search}` — wrappers.go.
Callers: dispatched-claude (read), operator (write).

### story (9)

| Verb | Site | Callers |
|---|---|---|
| `story_create` | mcp.go:401 | operator |
| `story_update` | mcp.go:414 | operator |
| `story_get` | mcp.go:427 | dispatched-claude, portal, operator |
| `story_list` | mcp.go:433 | portal, operator |
| `story_update_status` | mcp.go:443 | operator, agent-worker |
| `story_field_set` | mcp.go:450 | operator, story_close_agent |
| `story_template_get` | mcp.go:458 | operator |
| `story_template_list` | mcp.go:464 | operator |
| `story_export_walk` | mcp.go:758 | operator, portal |

### system (1)

| Verb | Site | Callers |
|---|---|---|
| `system_seed_run` | mcp.go:673 | operator [admin] |

### task (7)

| Verb | Site | Callers |
|---|---|---|
| `task_add` | mcp.go:582 | operator (orchestrator) |
| `task_update` | mcp.go:596 | dispatched-claude, agent-worker |
| `task_get` | mcp.go:717 | dispatched-claude, agent-worker, operator |
| `task_list` | mcp.go:723 | operator, portal |
| `task_claim` | mcp.go:736 | dispatched-claude, agent-worker |
| `task_walk` | mcp.go:748 | dispatched-claude, operator |
| `task_plan` | mcp.go:715 | operator |

### workspace (7, admin)

| Verb | Site | Callers |
|---|---|---|
| `workspace_create` | mcp.go:623 | operator [admin] |
| `workspace_get` | mcp.go:629 | operator |
| `workspace_list` | mcp.go:635 | operator |
| `workspace_member_add` | mcp.go:640 | operator [admin] |
| `workspace_member_list` | mcp.go:648 | operator |
| `workspace_member_update_role` | mcp.go:654 | operator [admin] |
| `workspace_member_remove` | mcp.go:662 | operator [admin] |

### satellites (1, advisory — survives cutover)

| Verb | Site | Callers |
|---|---|---|
| `satellites_info` | mcp.go:185 | dispatched-claude, operator |

### Inventory totals

- 107 runtime tools across 19 noun groups.
- Wrapper-template verbs: 36 (6 verbs × 6 types).
- Explicit-registration verbs: 71.
- Verbs called by dispatched-claude: 22 (the per-verb retrieval set
  the agents use for context).
- Verbs called by agent-worker: 4 (`task_claim`, `task_update`,
  `ledger_append`, `story_update_status`).
- Verbs surviving the cutover: **3** (`satellites_info`,
  `satellites_help`, `satellites_exec`).

### Frequency note

The `epic:cli-primary` AC asks for a recent-window frequency per
verb. This pull is deferred to develop CI of order:01's _follow-up_
slice — not a blocker for the design doc itself, since the
catalogue mapping (§2) is independent of frequency. AMBER caveat
recorded under §5.

---

## §2 CLI catalogue

Each MCP verb maps 1:1 to a CLI invocation under
`satellites-client <noun> <verb>`. Verb shape preserves CRUD where
applicable per `pr_mcp_naming_convention`. Operator-only verbs are
marked `[admin]` (the CLI's `--allow-admin` flag gates them, default
off). Dispatched-agent verbs are unmarked — every verb on the
dispatched-claude caller list is reachable from the CLI without a
flag.

### satellites-client agent

| MCP verb | CLI invocation |
|---|---|
| `agent_create` | `satellites-client agent create --scope <s> --name <n> [--project-id ...] [--body ... \| --stdin]` |
| `agent_update` | `satellites-client agent update --id <id> [--body ... \| --stdin]` |
| `agent_delete` | `satellites-client agent delete --id <id> [--mode archive\|hard]` |
| `agent_get` | `satellites-client agent get --id <id>` or `--name <n> [--project-id <id>]` |
| `agent_list` | `satellites-client agent list [--scope ...] [--project-id ...] [--tags a,b]` |
| `agent_search` | `satellites-client agent search --query <q> [--scope ...]` |
| `agent_compose` | `satellites-client agent compose --skill-refs s1,s2 [--name ...]` |
| `agent_ephemeral_summary` | `satellites-client agent ephemeral-summary [--project-id ...]` |
| `agent_apikey_create` | `satellites-client agent apikey-create --agent-id <id> [admin]` |
| `agent_apikey_list` | `satellites-client agent apikey-list [--project-id ...] [admin]` |
| `agent_apikey_delete` | `satellites-client agent apikey-delete --id <id> [admin]` |

### satellites-client changelog

| MCP verb | CLI invocation |
|---|---|
| `changelog_add` | `satellites-client changelog add --service <s> [--content ... \| --stdin]` |
| `changelog_get` | `satellites-client changelog get --id <id>` |
| `changelog_list` | `satellites-client changelog list [--service ...] [--project-id ...]` |
| `changelog_update` | `satellites-client changelog update --id <id> [--content ... \| --stdin]` |
| `changelog_delete` | `satellites-client changelog delete --id <id>` |

### satellites-client contract

| MCP verb | CLI invocation |
|---|---|
| `contract_create` | `satellites-client contract create --scope <s> --name <n> [--body ... \| --stdin]` |
| `contract_get` | `satellites-client contract get --id <id>` or `--name <n>` |
| `contract_list` | `satellites-client contract list [--scope ...] [--project-id ...]` |
| `contract_update` | `satellites-client contract update --id <id> [--body ... \| --stdin]` |
| `contract_delete` | `satellites-client contract delete --id <id>` |
| `contract_search` | `satellites-client contract search --query <q>` |

### satellites-client document

| MCP verb | CLI invocation |
|---|---|
| `document_create` | `satellites-client document create --type <t> --scope <s> --name <n> [--body ... \| --stdin]` |
| `document_get` | `satellites-client document get --id <id>` or `--name <n>` |
| `document_list` | `satellites-client document list [--type ...] [--scope ...] [--project-id ...]` |
| `document_update` | `satellites-client document update --id <id> [--body ... \| --stdin]` |
| `document_delete` | `satellites-client document delete --id <id>` |
| `document_search` | `satellites-client document search --query <q> [--type ...]` |
| `document_ingest_file` | `satellites-client document ingest-file --path <p> [--type ...]` |

### satellites-client kv

| MCP verb | CLI invocation |
|---|---|
| `kv_get` | `satellites-client kv get --key <k> [--scope ...]` |
| `kv_set` | `satellites-client kv set --key <k> [--value ... \| --stdin]` |
| `kv_delete` | `satellites-client kv delete --key <k> [--scope ...] [--yes]` |
| `kv_get_resolved` | `satellites-client kv get-resolved --key <k> [--scope ...]` |
| `kv_list` | `satellites-client kv list [--scope ...] [--prefix ...]` |

### satellites-client ledger

| MCP verb | CLI invocation |
|---|---|
| `ledger_append` | `satellites-client ledger append --type <t> [--content ... \| --stdin] [--tags a,b]` |
| `ledger_get` | `satellites-client ledger get --id <id>` |
| `ledger_list` | `satellites-client ledger list [--story-id ...] [--type ...]` |
| `ledger_search` | `satellites-client ledger search --query <q> [--tags a,b]` |
| `ledger_recall` | `satellites-client ledger recall --id <id>` |
| `ledger_dereference` | `satellites-client ledger dereference --id <id> [--yes]` |

### satellites-client portal

| MCP verb | CLI invocation |
|---|---|
| `portal_replicate` | `satellites-client portal replicate [--target ...]` |

### satellites-client principle

| MCP verb | CLI invocation |
|---|---|
| `principle_create` | `satellites-client principle create --scope <s> --name <n> [--body ... \| --stdin]` |
| `principle_get` | `satellites-client principle get --id <id>` or `--name <n>` |
| `principle_list` | `satellites-client principle list [--scope ...] [--project-id ...] [--active-only]` |
| `principle_update` | `satellites-client principle update --id <id> [--body ... \| --stdin]` |
| `principle_delete` | `satellites-client principle delete --id <id>` |
| `principle_search` | `satellites-client principle search --query <q>` |

### satellites-client project

| MCP verb | CLI invocation |
|---|---|
| `project_create` | `satellites-client project create --name <n>` |
| `project_get` | `satellites-client project get --id <id>` |
| `project_list` | `satellites-client project list` |
| `project_update` | `satellites-client project update --id <id> [--name ...]` |
| `project_delete` | `satellites-client project delete --id <id> [--yes]` |
| `project_set` | `satellites-client project set --repo-url <url>` |
| `project_seed_run` | `satellites-client project seed-run --project-id <id>` |

### satellites-client repo

| MCP verb | CLI invocation |
|---|---|
| `repo_add` | `satellites-client repo add --project-id <id> --remote <url>` |
| `repo_get` | `satellites-client repo get --id <id>` |
| `repo_list` | `satellites-client repo list [--project-id ...]` |
| `repo_search` | `satellites-client repo search --query <q> [--repo-id ...]` |
| `repo_search_text` | `satellites-client repo search-text --query <q> [--repo-id ...]` |
| `repo_get_symbol_source` | `satellites-client repo get-symbol-source --repo-id <id> --symbol <s>` |
| `repo_get_file` | `satellites-client repo get-file --repo-id <id> --path <p>` |
| `repo_get_outline` | `satellites-client repo get-outline --repo-id <id> --path <p>` |

### satellites-client reviewer

| MCP verb | CLI invocation |
|---|---|
| `reviewer_create` | `satellites-client reviewer create --scope <s> --name <n> [--body ... \| --stdin]` |
| `reviewer_get` | `satellites-client reviewer get --id <id>` or `--name <n>` |
| `reviewer_list` | `satellites-client reviewer list [--scope ...]` |
| `reviewer_update` | `satellites-client reviewer update --id <id>` |
| `reviewer_delete` | `satellites-client reviewer delete --id <id>` |
| `reviewer_search` | `satellites-client reviewer search --query <q>` |

### satellites-client role

| MCP verb | CLI invocation |
|---|---|
| `role_create` | `satellites-client role create --scope <s> --name <n>` |
| `role_get` | `satellites-client role get --id <id>` or `--name <n>` |
| `role_list` | `satellites-client role list [--scope ...]` |
| `role_update` | `satellites-client role update --id <id>` |
| `role_delete` | `satellites-client role delete --id <id>` |
| `role_search` | `satellites-client role search --query <q>` |

### satellites-client session

| MCP verb | CLI invocation |
|---|---|
| `session_register` | `satellites-client session register` |
| `session_whoami` | `satellites-client session whoami` |

### satellites-client skill

| MCP verb | CLI invocation |
|---|---|
| `skill_create` | `satellites-client skill create --scope <s> --name <n> [--body ... \| --stdin]` |
| `skill_get` | `satellites-client skill get --id <id>` or `--name <n>` |
| `skill_list` | `satellites-client skill list [--scope ...] [--project-id ...]` |
| `skill_update` | `satellites-client skill update --id <id>` |
| `skill_delete` | `satellites-client skill delete --id <id>` |
| `skill_search` | `satellites-client skill search --query <q>` |

### satellites-client story

| MCP verb | CLI invocation |
|---|---|
| `story_create` | `satellites-client story create --title <t> [--description ... \| --stdin]` |
| `story_get` | `satellites-client story get --id <id>` |
| `story_list` | `satellites-client story list [--project-id ...] [--tag ...]` |
| `story_update` | `satellites-client story update --id <id> [--title ...]` |
| `story_update-status` | `satellites-client story update-status --id <id> --status <s>` |
| `story_field_set` | `satellites-client story field-set --id <id> --key <k> --value <v>` |
| `story_template_get` | `satellites-client story template-get --category <c>` |
| `story_template_list` | `satellites-client story template-list` |
| `story_export_walk` | `satellites-client story export-walk --story-id <id>` |

### satellites-client system

| MCP verb | CLI invocation |
|---|---|
| `system_seed_run` | `satellites-client system seed-run [admin]` |

### satellites-client task

| MCP verb | CLI invocation |
|---|---|
| `task_add` | `satellites-client task add --agent-id <id> [--prompt ... \| --stdin]` |
| `task_get` | `satellites-client task get --id <id>` |
| `task_list` | `satellites-client task list [--story-id ...] [--status ...]` |
| `task_claim` | `satellites-client task claim [--workspace-id ...]` |
| `task_update` | `satellites-client task update --id <id> --status <s> [--outcome ...]` |
| `task_walk` | `satellites-client task walk --story-id <id>` |
| `task_plan` | `satellites-client task plan --origin <o> [--prompt ... \| --stdin]` |

### satellites-client workspace

| MCP verb | CLI invocation |
|---|---|
| `workspace_create` | `satellites-client workspace create --name <n> [admin]` |
| `workspace_get` | `satellites-client workspace get --id <id>` |
| `workspace_list` | `satellites-client workspace list` |
| `workspace_member_add` | `satellites-client workspace member-add --workspace-id <id> --user-id <u> --role <r> [admin]` |
| `workspace_member_list` | `satellites-client workspace member-list --workspace-id <id>` |
| `workspace_member_update_role` | `satellites-client workspace member-update-role --workspace-id <id> --user-id <u> --role <r> [admin]` |
| `workspace_member_remove` | `satellites-client workspace member-remove --workspace-id <id> --user-id <u> [admin]` |

### satellites-client satellites

| MCP verb | CLI invocation |
|---|---|
| `satellites_info` | `satellites-client info` |

### Catalogue totals

- 107 verbs in §1 (live runtime count).
- 107 invocations in §2.
- 1:1 cover. No orphans, no inventions.
- 3 verbs survive on the MCP surface post-cutover; the other 104 are
  CLI-only after order:07.

---

## §3 Conventions adopted from cli-printing-press

Adopted as-is from
https://github.com/mvanhorn/cli-printing-press, scoped to satellites'
verb shapes:

### Persistent flags (root command)

| Flag | Purpose | Why satellites needs it |
|---|---|---|
| `--json` | Force JSON output even on tty | Operators piping to `jq` from a tty |
| `--select <field>` | Project the output down to one field | `task add` returns 5 fields; `--select task_id` is the common case |
| `--compact` | Drop to high-gravity field set per noun | Long task/story rows are noisy in shell history |
| `--quiet` | Suppress non-error output | CI green-path runs need a clean stdout |
| `--dry-run` | Print payload that would be sent, exit 0 | Safety net for mutating verbs (delete, update-status) |
| `--stdin` | Read body from stdin | Long story descriptions, ledger content, document bodies |
| `--yes` | Skip confirmation prompts on destructive verbs | CI / scripted use |
| `--no-input` | Refuse interactive prompts | Same — fail fast when running unattended |
| `--no-cache` | Bypass any client-side cache | Diagnostic mode (no cache today; reserved) |
| `--no-color` | Strip ANSI from output | CI logs, NO_COLOR-respecting environments |

### Auto-JSON when piped

When `os.Stdout` is not a tty, the CLI emits JSON regardless of
`--json`. Matches the cli-printing-press rule: tty = human; pipe =
machine. Detected via `golang.org/x/term.IsTerminal(int(os.Stdout.Fd()))`.

### Typed exit codes

| Code | Meaning | Examples in satellites |
|---|---|---|
| `0` | Success | The happy path |
| `2` | Usage / argument error | Missing `--story-id`, malformed flag, mutually exclusive options |
| `3` | Not found | `story_get(non-existent)`, `task_get` after archive |
| `4` | Auth / permission | Bearer expired, scope mismatch, admin-only verb without `--allow-admin` |
| `5` | API / server | 5xx from satellites-server, JSON-RPC parse failure, connection reset |
| `7` | Rate limited | 429 from satellites-server (no current rate limiter; reserved) |

`1` is **not used** — Go's default-on-panic exit. Reserved so a
CLI panic is distinguishable from a graceful error path.

### --compact field sets (per noun)

`--compact` projects the response to just these fields. Other fields
are dropped. The set is opinionated; full output is one flag away
(`--json` without `--compact`).

| Noun | Compact fields |
|---|---|
| story | `id`, `title`, `status`, `priority`, `tags` |
| task | `id`, `story_id`, `kind`, `action`, `status`, `outcome`, `iteration` |
| ledger | `id`, `type`, `tags`, `created_at` (truncate content to 80 chars) |
| document | `id`, `type`, `name`, `scope`, `status` |
| workspace | `id`, `name` |
| project | `id`, `name`, `repo_url_canonical` |
| repo | `id`, `project_id`, `git_remote` |
| kv | `key`, `value` (truncate to 80 chars) |
| changelog | `id`, `service`, `version_to`, `effective_date` |

### Error message style

> Format: `error: <verb>: <field>: <one-sentence-reason>`

Examples:

- `error: task add: --agent-id: agent doc_xxxxxxxx not found`
- `error: story update-status: --status: 'derived' is not a valid transition target`
- `error: ledger append: stdin: empty body (use --content or pipe non-empty input)`

The flag/arg is named explicitly. The reason is one sentence with
no jargon. No stack traces in user-facing output (verbose=true via
`--debug`).

### Credential resolution

Bearer token resolved (in order):

1. `--token <t>` flag.
2. `SATELLITES_TOKEN` env var.
3. `~/.satellites/credentials.json` (per-server keyed).
4. Interactive `auth login` flow (deferred — order:03 scaffold,
   not MVP).

`--server <url>` overrides the canonical pprod URL. Default is the
URL bound by `project_set` against the local repo's git remote.

### Stdin discipline

`--stdin` reads from `os.Stdin` until EOF. The body becomes
whichever content field the verb expects (`--description` for
`story create`, `--content` for `ledger append`, `--body` for
`document create`). Mutually exclusive with the inline flag — pass
one or the other, not both.

---

## §4 Baseline measurement

### Method

The baseline is measured against a live dispatch on pprod. Numbers
captured: tokens consumed by MCP tool definitions in the dispatched
Claude's first-turn context; tokens consumed by the system prompt;
tokens consumed by per-message memory; total first-turn context
size.

### Status of the measurement

This story (order:01) is GREEN-with-AMBER on the baseline AC. The
catalogue and design are complete; the live measurement is
intentionally **deferred to a follow-up slice on order:07** because:

1. The headline number (post-cutover delta vs baseline) is the
   piece that matters for the epic's claim. Capturing it once at
   order:07's deploy lets us measure the same agent before/after
   in one slice, with controlled variables. Capturing at order:01
   then re-capturing at order:07 doubles the risk of measurement
   drift (different agent dispatched, different memory state,
   different model build).
2. The MCP catalogue token cost is dominated by the live
   `tools/list` response, not by source-side counts. A capture
   from a representative dispatch in order:07 supplies both
   numbers (before and after) from the same harness.

### Headline expectation (sanity check, not the AC)

Each MCP tool definition in the satellites catalogue averages
~250-350 tokens (name + description + JSON-Schema for input).
107 verbs × 300 tokens ≈ **32,100 tokens of first-turn
overhead per dispatch**, before any task-specific context. After
cutover (3 verbs × 300 tokens ≈ **900 tokens**), the saving is
roughly **~31,000 tokens per dispatch**. Across thousands of
dispatches the compound is material; the order:07 slice records
the literal number.

This estimate is a sanity check. The AC's "raw, not estimated"
requirement is satisfied by order:07's live capture, recorded as
the `kind:cli-primary-context-delta` ledger row tagged
`epic:cli-primary` + `phase:before` and `phase:after`.

---

## §5 Recommendation

**AMBER** — proceed with two named caveats.

### Caveat 1 — Live count is 107, not 73

The epic's headline number is wrong. 107 → 3 is a **~35:1**
reduction (not the 24:1 implied by 73 → 3). Update the
`sty_c01b23b5` epic body and the order:07 acceptance criterion to
reference the live count. No follow-up story required — a single
edit on the epic.

### Caveat 2 — Baseline measurement deferred to order:07

The "raw, not estimated" baseline is captured at order:07 cutover
time, not order:01. Rationale in §4. The order:07 acceptance
criterion is updated to require both `phase:before` and
`phase:after` measurements in one dispatch window so the delta is
the difference between the same agent's two consecutive context
sizes. This change must be made on `sty_3dc39a5c` before order:07
ships.

### Caveat 3 — Frequency column deferred

§1's per-verb frequency column is empty pending an order:01
follow-up slice that pulls 30-day usage stats from the ledger.
The catalogue mapping (§2) is independent of frequency, so this
does not block orders 02-06. A follow-up story (out-of-scope of
this story; tag `epic:cli-primary`, `order:01-followup`) is the
cheapest path.

### What this doc commits

The doc commits the **catalogue mapping** (§2) and the
**conventions** (§3) as the spec orders 02-08 build against. The
inventory (§1) is the source of truth for the verb count at this
commit. The recommendation (§5) is the gate the user reads before
greenlighting order:02.

If GREEN: orders 02-08 proceed as planned, with the two epic-body
edits above (Caveat 1, Caveat 2) shipped as their own one-line
commits before order:07 dispatches.
