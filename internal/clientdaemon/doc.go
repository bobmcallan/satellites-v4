// Package clientdaemon hosts the local-only `satellites-client serve`
// daemon. The daemon listens on a Unix-domain socket, accepts task_id
// enqueue requests, and dispatches each task as its own claude
// subprocess in the operator's per-task git worktree — identical in
// shape to a synchronous `satellites-client task run` invocation.
//
// Substrate model refusal (pr_substrate_model — sty_5aa20f1b AC10)
//
// The daemon is dispatch plumbing only. It is NOT a hosting layer
// for LLM behaviour and it MUST NOT be extended into one. Three
// prohibitions bind every change to this package:
//
//  1. No server-side claude hosting. The daemon never asks the
//     satellites-server (pprod) to spawn `claude -p`. The agent
//     subprocess executes on the operator's workstation only.
//  2. No subagent harness inside the daemon. The daemon does NOT
//     spawn Claude Code's `Agent` tool, sub-Claude sessions, or
//     any other subagent harness. The dispatched agent's reasoning
//     lives in the same `claude -p` subprocess that
//     `internal/agent/worker.RunDispatched` already spawns; the
//     daemon is one more invoker of that function, not a parallel
//     implementation.
//  3. No LLM behaviour moved into Go. The daemon does not
//     summarise, judge, classify, or otherwise reason about task
//     content. Decisions that require judgment travel to the
//     dispatched claude subprocess, not into the daemon's Go code.
//
// Worked failure mode: sty_4db0e025 captures an earlier attempt to
// move LLM behaviour into Go and the cleanup that followed. Future
// changes to this package that risk re-introducing any of the three
// prohibitions above must cite this story and demonstrate why the
// boundary holds.
//
// Layering (pr_mcp_cli_shared_path — sty_5aa20f1b AC9)
//
// The daemon is execution-surface code, not a transport handler;
// substrate reads + writes route through `*cliremote.Client`. The
// package does not import any substrate-domain package
// (internal/document, internal/story, internal/task, …); a defensive
// AST test in layering_test.go pins the layering closed.
package clientdaemon
