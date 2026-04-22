# Satellites v4 — Local development loop

Every feature story in v4 runs against a local integration harness before it ships. This doc tells you how to run the harness, what it covers today, and how to extend it when your feature adds a runtime surface.

---

## One command

```
go test ./tests/integration/...
```

Exits 0 on a clean checkout with only the Go toolchain installed. The smoke tests build both binaries from source into a scratch dir and assert each prints its expected boot line with stamped version metadata. Expected runtime: well under a second on a warm build cache.

For verbose output (captured boot lines, per-test timing):

```
go test ./tests/integration/... -v
```

## What the harness exercises today

- **`cmd/satellites`** — builds from source, runs, asserts the output begins with `satellites-server ` and contains the `build: …, commit: …` fragments from `internal/config.GetFullVersion()`.
- **`cmd/satellites-agent`** — same shape, prefix `satellites-agent `.

Tests run in parallel (`t.Parallel()`). The bounded `runBinary` timeout is 10 s, generous for one-shot stubs and safe for future short-running modes.

## Why this exists (and why it's minimal right now)

`pr_local_iteration` is the operating rule: every feature iterates against a local running instance before it ships. v4 lands the harness *before* any feature story so the v3 pattern — iterate on pprod, fix via diagnostic commits — can't establish. The current smoke surface is small because the binaries are print-and-exit stubs; it still proves the build wiring, the version stamping, and the test entry point.

## Extension pattern

New feature stories **extend this package** rather than creating a parallel one.

- Add a new `<feature>_test.go` in `tests/integration/`. Use the helpers in `harness.go` (`buildBinary`, `runBinary`) before writing new ones.
- When your feature introduces a **long-running mode** (e.g. the MCP server staying up to accept calls), add a `start(t)` / `stop(t)` pair to `harness.go` that wraps `exec.Cmd.Start()` + `context.CancelFunc`. Keep the public surface stable: tests should boot, exercise, tear down, not re-implement process plumbing.
- When your feature introduces a **DB or other backing service**, add the `testcontainers-go` dependency and a new `startContainer(t)` helper in `harness.go`. The first story to do this owns the dep addition and a short follow-up paragraph here. Do not add the dep speculatively.
- When your feature introduces **shared fixtures** (seed data, signed sessions), put them in a new file in the same package with a clear name (`fixtures_<area>.go`). Shared helpers live in `harness.go`; feature-specific helpers live next to their tests.

The discipline: one harness, one dependency surface, one set of helpers. Don't fork the harness into parallel packages — that's how v3 accreted three near-identical test harnesses over time.

## Prerequisites

- Go 1.22+ (`go.mod`).
- Docker Engine — not required today, but assumed available when the first DB-introducing story lands. Install it now so the dev loop stays single-command later.

## When the harness isn't the right tool

- **Unit tests** for pure functions — co-locate in the package being tested, not here.
- **Benchmarks** — live next to the code under test.
- **CI-specific checks** (lint, vet, fmt) — driven by `script/build.sh` commands, not the integration harness.

## Troubleshooting

- *"go: no such target"* — you ran the command outside the repo root. The harness resolves the repo root at runtime, so tests work from any CWD; only the `go test ./tests/integration/...` invocation needs the repo root.
- *"build failed"* — the test log includes the full `go build` stderr; fix the build before re-running.
- *Timeout firing on a long-running feature* — you're past the print-and-exit stub stage. Extend `runBinary` into a `start`/`stop` pair per the extension pattern; don't widen the timeout.
