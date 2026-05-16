---
name: local_build
tags: [v4, satellites-self-modifying, developer]
---
# Local Build (satellites self-modifying)

Satellites is the rare project where develop slices change the
binaries that run the substrate. When a slice touches a path
consumed by an in-use binary, developer_agent MUST build the
binary locally before claiming the develop close. When the
touched binary is `satellites-client` (the orchestrator's running
daemon), the develop close MUST emit an operator-swap evidence
row rather than silently producing a stale-binary state — that
state is `client-in-use` and the running daemon owns
`./.satellites/satellites-client`.

## When this skill applies

The slice touches any of:

- `cmd/satellites-server/**`
- `cmd/satellites-client/**`
- `cmd/satellites-agent/**`
- `internal/**` consumed by one of the above binaries' build graph.

If none apply (pure docs, seed without configseed wiring, or
portal-only changes), this skill is inert.

## Plan phase — what to add to plan.md

When the slice meets the trigger above, `plan.md` MUST carry
an explicit `## Local build` section listing:

- The touched binary or binaries (per the changed-files heuristic
  in the commit contract's `## Version bump policy`).
- The build command for each touched binary.
- A flag if any touched binary is `satellites-client`: plan-md
  states `client-in-use; develop will emit operator-swap evidence;
  release requires operator binary swap before pprod will accept`.

A slice that meets the trigger but ships a plan-md without this
section is a plan gap the development_reviewer cites.

## Develop phase — build commands

Build per touched binary into a local staging path so the binary
in use (under `./.satellites/`) is not overwritten by the
dispatched develop subprocess:

```bash
mkdir -p .satellites/staging
go build -o .satellites/staging/satellites-server ./cmd/satellites-server
go build -o .satellites/staging/satellites-client ./cmd/satellites-client
go build -o .satellites/staging/satellites-agent  ./cmd/satellites-agent
```

Capture build stdout/stderr in the develop close evidence row
tagged `kind:local-build,binary:<name>`.

## Develop phase — tests

After build, run the local-iteration gate sequence per
`pr_local_iteration` (iterate locally before pushing):

```bash
go test -short ./...
go test ./tests/integration/...
```

The integration target boots the real satellites server via the
`tests/integration/` harness — this is the closest local
equivalent to the pprod surface. Capture pass/fail counts in the
develop close evidence row.

## Client-in-use branch

When the touched binary is `satellites-client`, the running
orchestrator's daemon is using `./.satellites/satellites-client`.
The dispatched developer MUST NOT swap it in-process — that is an
operator-side atomic-replace (stop daemon → swap binary →
restart serve), not develop work.

Instead, after a green local build of
`.satellites/staging/satellites-client`, append one ledger row
specifying the operator-swap contract. Required tag set + content fields summary: `kind:operator-swap-required` tag plus `staged_binary` + `sha256` + `post_swap_verify` content keys (see example block below).

Example evidence row:

```
type=evidence
tags=task_id:<develop_task>,kind:operator-swap-required,binary:satellites-client
content={
  "staged_binary":    ".satellites/staging/satellites-client",
  "sha256":           "<sha256 of the staged file>",
  "operator_action":  "stop daemon, swap binary into ./.satellites/, restart serve",
  "post_swap_verify": "satellites-client --version reports the build the develop task produced"
}
```

This row IS the contract: develop did its job (built + tested
the binary), and the operator-swap is required before release can
observe the change in the running daemon. The releaser_agent's
`merge_to_main` already polls pprod's `satellites_info` until the
running commit matches — that polling times out if the operator
skipped the swap, which surfaces the gap explicitly rather than
silently shipping a stale binary.

## When this skill does NOT apply

- Consumer projects (anything other than the satellites repo
  itself). Their developer_agent doc need not list this skill in
  `skill_refs`.
- Slices that touch only docs, seeds without configseed wiring,
  or portal assets that do not link into a satellites binary.
