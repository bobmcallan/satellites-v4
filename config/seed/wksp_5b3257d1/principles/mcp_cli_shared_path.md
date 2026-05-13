---
id: pr_mcp_cli_shared_path
name: mcp_cli_shared_path
scope: workspace
tags:
  - architecture
  - transport
  - drift-prevention
  - cli-primary
---
# Transport handlers delegate to internal/client; no business logic in the transport layer

## Rule

*Transport handlers (MCP, HTTP) MUST delegate to `internal/client`; no business logic in the transport layer.*

A transport-handler file in `internal/mcpserver/` (and the matching
HTTP route handlers in `internal/httpserver/`) does exactly three
things: it unmarshals the transport-specific request envelope
(JSON-RPC args for MCP, HTTP body + path params for HTTP), it calls
exactly one typed method on `*internal/client.Client`, and it shapes
the typed return value into the transport-specific response. It must
NOT import any substrate domain package directly:
`internal/document`, `internal/story`, `internal/task`,
`internal/principle`, `internal/skill`, `internal/reviewer`,
`internal/role`, `internal/agent`, `internal/contract`,
`internal/repo`, `internal/kv`, `internal/changelog`,
`internal/workspace`, `internal/project`, `internal/ledger`,
`internal/portalreplicate`, `internal/agentprocess`,
`internal/session` — all are reachable only through
`internal/client` typed methods.

The allowed delegation surface from a transport handler is
`internal/client`, `internal/auth` (request identity),
`internal/config` (server config), `internal/arbor` (logger), the
stdlib, and the transport SDK (`github.com/mark3labs/mcp-go/...` for
MCP; `net/http` and the project's HTTP router for HTTP).

## Why drift-risk

Two transports — MCP and HTTP — both expose the same substrate
verbs. When each transport re-implements the business logic, a
behaviour change has to be made twice and stays in sync only via
discipline. The 2026-05-12 `sty_22f1b946` parity test caught
drift after the fact; this principle prevents the drift at build
time by making the shared `internal/client` path the only place a
substrate verb's logic lives. Adding a verb is one typed method on
`*client.Client` and two thin wire adapters — both transports gain
the new behaviour together, in one commit. Worked example:
`satellites_init` (sibling `sty_796b8fe1`) ships the typed method
in `internal/client/satellites_init.go` and thin MCP / HTTP / CLI
adapters that delegate to it.

## Why dispatch efficiency

The orchestrator-role MCP verbs ride an MCP channel that is already
open during dispatch. Forcing MCP handlers to shell out to
`satellites-client` per call (a CLI-only convergence) would spawn a
subprocess per verb call inside the dispatch loop. Keeping MCP
handlers as thin wire adapters over the shared
`*client.Client` keeps the per-call cost at one in-process method
invocation while still preserving the single business-logic path
that CLI users hit through the same client.

## How to apply

- **Adding a new MCP verb** — register the tool in the relevant
  `internal/mcpserver/<noun>_handlers.go` file. The handler body
  unmarshals JSON-RPC args, calls `s.cli().<Method>(ctx, args…)`,
  and wraps the typed return value into the MCP response envelope.
  No store imports, no domain logic, no substrate-package imports.
- **Adding a new HTTP route** — register the route in
  `internal/httpserver/`. The handler body unmarshals HTTP body /
  path params, calls the same `client.Client.<Method>`, and shapes
  the HTTP response. Same delegation surface.
- **Adding a new service capability** — write the typed method on
  `*internal/client.Client` (file under `internal/client/`). Both
  transports gain the capability without re-implementing it.

## What this forbids

- Transport-handler files importing substrate domain packages
  directly. Enforced by `TestTransportLayering` in
  `internal/mcpserver/layering_test.go` (this story).
- Per-transport business-logic re-implementation: every store call,
  every validation, every fan-out lives on `*client.Client`. The
  transport layer is wire-shape translation only.
- Aliases or shims that route around the shared client — those would
  re-introduce the two-path drift this principle exists to prevent.
  Cite `pr_no_unrequested_compat` when reviewing a PR that adds one
  without explicit AC support.
- A `dispatch_context` aggregate verb (or any other bundling verb
  that supplies multiple substrate row types in one call). Per-verb
  retrieval is the substrate's contract — see
  `pr_substrate_provides_context` / `pr_substrate_model`.

## Citations

- `pr_no_unrequested_compat` — convergence happens via delete +
  migrate, not via alias + defer. Per-noun handler migration in
  `sty_4db0e025` (order:07d) removes legacy direct-substrate imports
  from each transport file in lock-step with adding the typed client
  method that replaces them. No transport-side compat wrappers
  persist after cutover.
- `pr_substrate_provides_context` / `pr_substrate_model` — per-verb
  retrieval is the dispatch model. Each `*client.Client` method maps
  to exactly one substrate verb, keeping the dispatch path
  predictable and matching the configuration-over-code mandate.
- Backed by the AST-based layering test at
  `internal/mcpserver/layering_test.go` (`TestTransportLayering`).
  The test enumerates forbidden substrate imports per transport
  file, with a documented legacy allowlist (each entry tagged
  `TODO(sty_4db0e025)`) that shrinks to zero as
  `sty_4db0e025` (order:07d) per-noun convergence ships.
- This story: `sty_3dc39a5c`. Extraction prerequisite:
  `sty_f3f7bf9b`. Per-noun convergence implementation:
  `sty_4db0e025`.
