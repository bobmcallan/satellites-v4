# satellites

Developer-in-the-loop agentic engineering platform. Three binaries cooperate: `satellites-server` (state + MCP + cron) holds the substrate; `satellites-agent` runs as an autonomous worker against external repos; `satellites-client` is the per-task dispatch CLI that runs team-member subprocesses in a per-task worktree on a `client-{task_id}-…` branch. Two surfaces sit in front: MCP is the authoring surface (write verbs author primitives, read verbs orient), and `satellites-client task run` is the execution surface (file edits, builds, tests, commits all happen behind it). The split is load-bearing — see `pr_substrate_model`.

Module path: `github.com/bobmcallan/satellites`.

## Positioning

satellites is the substrate Claude (and other narrow MCP-driven agents) plug into for trustworthy software work. The platform exists so a small autonomous agent surface earns audit-grade evidence per step — every plan, decision, file change, and review verdict lives on an append-only ledger. Trust comes from the work, not the agent. See [docs/architecture.md](docs/architecture.md) for the design rationale and the seven-primitive data model (workspace, project, document, story, task, ledger, repo).

## Substrate model

Behaviour lives in markdown rows, not Go code. **Agents**, **contracts**, **principles**, and **workflows** are configseed-booted primitives — changing what an agent does means writing prose and re-seeding, not editing source. This configuration-over-code mandate is what makes the substrate auditable: every behaviour the system enforces is reviewable text on a ledger, not a compiled-in policy.

Two surfaces sit on top of that substrate. **MCP** is the authoring surface — write verbs (`story_create`, `task_create`, `ledger_append`, …) author primitives; read verbs (`story_get`, `task_walk`, `ledger_search`) carry orientation. The **`satellites-client task run`** CLI is the execution surface — file edits, builds, tests, and commits live behind it, in a per-task worktree on a `client-{task_id}-…` branch. The orchestrator never edits files directly; it dispatches work through the CLI. Cite `pr_substrate_model`.

Each story flows through a fixed lifecycle pipeline: `plan → develop → review → iterate → commit → merge_to_main → story_close`. Reviewer rejection mints a successor `kind=work` task on the same chain rather than mutating the prior verdict; verdicts accumulate as ledger rows so the chain stays append-only.

Contracts live in a two-tier scope model. Lifecycle contracts (`plan`, `story_close`) sit at `scope=system` — they are the orchestrator's own shell. Work contracts (`develop`, `commit`, `push`, `merge_to_main`, …) sit at `scope=project` — they are the substrate the dispatched agents operate inside. The two tiers never mix. Cite `pr_contract_separation`.

## Quickstart

### Quickstart — boot the server

```
cp scripts/satellites.example.toml scripts/satellites.toml   # canonical config carrier (gitignored)
$EDITOR scripts/satellites.toml                              # customise port / OAuth / Gemini / etc.
$EDITOR .env                                                 # optional runtime overrides (gitignored)
./scripts/deploy.sh up                                       # boot satellites + SurrealDB via docker compose
open http://localhost:8080
```

### Quickstart — consume from a project

A consumer project bootstraps the execution surface via the `satellites_init` MCP verb. Open a project-bound MCP session against a running `satellites-server`, then call `satellites_init` with the consumer project root as the working directory. The verb returns one of three install-payload states:

- `install_required` — no `satellites-client` binary present under `./.satellites/`; fetch the embedded payload, sha256-verify, `chmod +x`, and write it to `./.satellites/satellites-client`.
- `update_available` — a binary is installed but its version lags the server; replace it via the same fetch / verify / write path.
- `up_to_date` — the colocated binary is current; no fetch needed.

Alongside the install payload, `satellites_init` returns an `auth_bootstrap.kind="ready"` block carrying a project-scoped agent API key (minted on first call for a project-bound session). Write the bearer into `./.satellites/satellites-client.toml` so the colocated CLI authenticates against the same server without re-prompting. From that point on, `./.satellites/satellites-client task run …` is the execution surface for every dispatched work task in that project.

## pprod

The shared pre-production deployment runs on Fly: <https://satellites-pprod.fly.dev/>. The `pprod` smoke target (`go test -tags=pprod ./tests/integration/... -run Pprod`) re-validates `/healthz` + `/mcp` after every push to `main`.

## Documentation

- Architecture & primitives — [docs/architecture.md](docs/architecture.md)
- UI design notes — [docs/ui-design.md](docs/ui-design.md)
- Local development workflow — [docs/development.md](docs/development.md)
- Release history — [CHANGELOG.md](CHANGELOG.md)

## Build

Use `scripts/build.sh` for everyday build, lint, and maintenance tasks. It's a plain bash dispatcher — `scripts/build.sh <command>`, with `build` as the default.

```
./scripts/build.sh build     # stamps each binary from its own .version section (default)
./scripts/build.sh server    # builds satellites-server only  (reads [satellites-server])
./scripts/build.sh agent     # builds satellites-agent only   (reads [satellites-agent])
./scripts/build.sh client    # builds satellites-client only  (reads [satellites-client])
./scripts/build.sh docker    # builds the satellites docker image (reads [satellites-server])
./scripts/build.sh fmt       # gofmt -s -w .
./scripts/build.sh vet       # go vet ./...
./scripts/build.sh lint      # golangci-lint run (skipped if not installed)
./scripts/build.sh test      # go test ./...
./scripts/build.sh clean     # remove built binaries
./scripts/build.sh help      # show usage
```

Plain `go build ./...` also works and produces `dev`-stamped binaries with build/commit defaults of `unknown` — suitable for quick iteration without ldflags.

## Deploy locally

The local docker stack (satellites + SurrealDB) is driven by `docker/docker-compose.yml`. Use `scripts/deploy.sh` as the single operator entry point — it wraps `docker compose` with the compose file + a mandatory `scripts/satellites.toml` + an optional `.env` for runtime overrides.

```
cp scripts/satellites.example.toml scripts/satellites.toml   # canonical config carrier
$EDITOR scripts/satellites.toml                              # customise
$EDITOR .env                                                 # optional runtime overrides
./scripts/deploy.sh up                                       # build + start the stack (default subcommand)
./scripts/deploy.sh logs                                     # tail combined logs
./scripts/deploy.sh restart
./scripts/deploy.sh down
```

`scripts/satellites.toml` is mounted into the container at `/app/satellites.toml` and the binary boots from it via `SATELLITES_CONFIG=/app/satellites.toml`. Both `scripts/satellites.toml` and `.env` are gitignored — treat them as machine-local. The full env-var reference lives in [Server configuration](#server-configuration) below; the same keys can be set in TOML (canonical) or via `.env` (override).

Config is layered: TOML is the canonical source, env vars are overrides, and every key has an in-code default. Resolution order (highest first) is **process env var → TOML file → code default**. Production reads via `SATELLITES_CONFIG=/path/to/file` (missing-explicit-file is an error). `scripts/satellites.example.toml` lists every key with its default — copy to `scripts/satellites.toml` and customise. Defaults live in `internal/config/config.go::defaults`; prod-required gaps are named by `validate()`. OAuth providers are gated on `google_client_id` / `google_client_secret` (env override: `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET`) and the GitHub equivalents; absent values hide the corresponding landing button and surface the no-auth diagnostic banner.

## Run

```
./satellites-server  # satellites-server <version> (build: <build>, commit: <commit>)  — state + MCP + cron
./satellites-agent   # satellites-agent <version> (build: <build>, commit: <commit>)   — autonomous worker against external repos
./satellites-client  # satellites-client <version> (build: <build>, commit: <commit>)  — per-task dispatch CLI; `task run` runs a team-member subprocess in a per-task worktree
```

Each binary prints one boot line with its name and the full version metadata.

## .version

The `.version` file at the repo root carries the semantic version for each binary in its own section. Only `version` is stored — the build timestamp and git commit are generated at build time so they always reflect the actual build moment, not a stale file edit.

```
[satellites-server]
version = 0.0.1

[satellites-agent]
version = 0.0.1

[satellites-client]
version = 0.0.1
```

`scripts/build.sh`:
- parses the appropriate section for `version` (section-scoped — never reads across sections),
- generates `build` via `date -u +"%Y-%m-%d-%H-%M-%S"` at build time,
- generates `commit` via `git rev-parse --short HEAD` at build time,
- injects all three into `internal/config.{Version, Build, GitCommit}` via `-ldflags -X`.

Bumping only one section's `version` affects only that binary's boot line version string.

## Version metadata

Runtime exposure lives at `internal/config/version.go`:

```go
var Version   = "dev"     // overridden by ldflags from .version section
var Build     = "unknown" // overridden by ldflags from date -u at build time
var GitCommit = "unknown" // overridden by ldflags from git rev-parse --short HEAD

func GetFullVersion() string  // "<version> (build: <build>, commit: <commit>)"
```

`cmd/satellites-server/main.go`, `cmd/satellites-agent/main.go`, and `cmd/satellites-client/main.go` all call `config.GetFullVersion()` in their boot line. A plain `go build ./...` produces runnable binaries stamped with the three defaults above.

## Server configuration

Every env var the server reads. Populate the relevant subset in a local
`.env` (gitignored) at the repo root — `docker/docker-compose.yml`
includes it via `env_file: ../.env` and the Go binary reads each key via
`os.Getenv` at boot. The same keys can be exported in your shell or set
as Fly secrets in pprod. TOML (`satellites.toml`, also gitignored) is the
canonical config; env vars are overrides; `internal/config/config.go`
holds the in-code defaults. Resolution order (highest first):
**process env var → TOML file → code default**.

### Server

```
PORT=8080            # HTTP listen port
ENV=dev              # dev | prod — dev relaxes auth, enables /debug/pprof
LOG_LEVEL=info       # trace | debug | info | warn | error
DEV_MODE=true        # when ENV=dev, also requires DEV_USERNAME/PASSWORD
```

### Database

```
# docker-compose resolves "surrealdb" to the sibling container; off-compose
# change to ws://root:root@localhost:8000/rpc/satellites/satellites.
DB_DSN=ws://root:root@surrealdb:8000/rpc/satellites/satellites
```

### Documents

```
DOCS_DIR=/app/docs   # container-side mount point for the repo's docs/ tree
```

### Auth — basic + DevMode

When `ENV=dev` and `DEV_MODE=true`, this credential authenticates without
OAuth. Disabled entirely in prod.

```
DEV_USERNAME=dev@local
DEV_PASSWORD=change-me
```

### Auth — OAuth providers

A provider is enabled iff BOTH its `CLIENT_ID` and `CLIENT_SECRET` are
non-empty. Empty values hide the corresponding "Sign in with …" button on
the landing page and make `/auth/<provider>/start` return 404. Register
OAuth apps at:

- Google → <https://console.cloud.google.com/apis/credentials> — callback
  `${OAUTH_REDIRECT_BASE_URL}/auth/google/callback`.
- GitHub → <https://github.com/settings/applications/new> — callback
  `${OAUTH_REDIRECT_BASE_URL}/auth/github/callback`.

For pprod, set the secrets per provider:

```
fly secrets set --app satellites-pprod \
    GOOGLE_CLIENT_ID=<id> \
    GOOGLE_CLIENT_SECRET=<secret> \
    GITHUB_CLIENT_ID=<id> \
    GITHUB_CLIENT_SECRET=<secret> \
    OAUTH_REDIRECT_BASE_URL=https://satellites-pprod.fly.dev
fly secrets list --app satellites-pprod   # confirm presence
```

A v4 deploy without these secrets renders the landing page with no OAuth
buttons; covered by `TestLanding_HidesOAuthWhenEmptyCreds` in
`internal/portal/portal_test.go`.

```
GOOGLE_CLIENT_ID=
GOOGLE_CLIENT_SECRET=
GITHUB_CLIENT_ID=
GITHUB_CLIENT_SECRET=
OAUTH_REDIRECT_BASE_URL=http://localhost:8080
```

### Embeddings

When `EMBEDDINGS_PROVIDER` is set, the server boots an embed worker that
chunks + embeds documents and ledger rows so `document_search` /
`ledger_search` return semantic hits instead of falling back to filter-
only `Search` via `ErrSemanticUnavailable`. Empty / `none` disables the
worker (default) — useful for tests + dev without an API key. Pprod
default: gemini `text-embedding-004`.

```
EMBEDDINGS_PROVIDER=         # gemini | openai | stub | none
EMBEDDINGS_MODEL=            # e.g. text-embedding-004 (gemini)
EMBEDDINGS_API_KEY=          # provider key
EMBEDDINGS_DIMENSION=        # optional override; provider default otherwise
```

For pprod:

```
fly secrets set --app satellites-pprod \
    EMBEDDINGS_PROVIDER=gemini \
    EMBEDDINGS_MODEL=text-embedding-004 \
    EMBEDDINGS_API_KEY=<google-ai-studio-key>
fly logs --app satellites-pprod | grep "embedding worker started"
```

Tests use `EMBEDDINGS_PROVIDER=stub` (deterministic, no network); see
`tests/integration/embed_worker_test.go`.

### Reviewer (Gemini)

When `GEMINI_API_KEY` is set, the contract reviewer is the Gemini-backed
implementation (`cmd/satellites-server/main.go::buildReviewer`). When unset, the
reviewer falls back to `AcceptAll` with a startup warning so dev/test
boots stay green.

```
GEMINI_API_KEY=              # Google AI Studio key
GEMINI_REVIEW_MODEL=         # default: gemini-2.5-flash
```

### MCP

```
SATELLITES_API_KEYS=         # comma-separated Bearer tokens for /mcp
```

### Tests-only

Test integrations pick up Gemini + embeddings credentials from
`tests/.env` (gitignored). Copy `tests/.env.example` → `tests/.env` and
fill in the keys; `tests/common` auto-loads them via package init. Host-
exported values always win.
