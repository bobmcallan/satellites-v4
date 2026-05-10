# tests/

Test infrastructure for the satellites server.

## Default suite

```
go test ./...
```

Runs every test that does not carry a build tag. Stays green without
external services — testcontainers tests skip on `-short`, live API
tests are excluded by build-tag.

## Live integration tests

A small set of tests exercise real external APIs (Gemini reviewer
today; e2e story lifecycle later). They are gated by a `//go:build
live` constraint so the default suite never invokes them.

### Setup

1. Copy the template:

   ```
   cp tests/.env.example tests/.env
   ```

2. Populate `tests/.env` with at least `GEMINI_API_KEY`. Optional:
   `GEMINI_REVIEW_MODEL`, `EMBEDDINGS_API_KEY`, `EMBEDDINGS_PROVIDER`,
   `EMBEDDINGS_MODEL`. `tests/.env` is gitignored.

3. The loader at `tests/common/dotenv_test_keys.go` runs on package
   init the first time any test imports `tests/common`. Host-exported
   values always win — exporting a key in your shell overrides the
   `tests/.env` value.

### Run

```
make test-live   # live Gemini reviewer test (HTTP-only, no container)
make test-e2e    # full lifecycle e2e against testcontainers, asserts Gemini wired
```

`test-live` runs `go test -tags=live -timeout 60s ./internal/reviewer/...`.
An empty `GEMINI_API_KEY` is a hard fail under `-tags=live` — PASS-by-skip
is rejected (story_d21436a4). Populate `tests/.env` first.

`test-e2e` exports `SATELLITES_E2E_REQUIRE_GEMINI=1` and runs
`TestE2E_StoryLifecycle_FullFlow`. Without the key, the test fails
loudly during boot. With the key, the in-container reviewer wires
Gemini and at least one CI close response carries a non-empty
`llm_usage_ledger_id` (proving the call actually fired).

### Opt out

The default `go test ./...` excludes every `//go:build live` file and
runs testcontainer tests in `-short` mode (which skips them) —
contributors without keys see no failure, no skip noise, no extra
runtime.

## Rotating credentials

Test credentials live in `tests/.env` (gitignored). The `tests/common`
package init loader (story_7f6e0f4e) walks up from its source location
to the nearest `tests/` ancestor, reads `.env` from there, and
propagates a whitelist of keys (`GEMINI_API_KEY`,
`GEMINI_REVIEW_MODEL`, `EMBEDDINGS_API_KEY`, `EMBEDDINGS_PROVIDER`,
`EMBEDDINGS_MODEL`) into the **test process** env. Host-exported values
always win.

After story_b218cb81 those credentials are first-class Config fields,
so the per-test TOML carries them into the container. `tests/.env`
remains the host-side carrier (the loader populates `os.Getenv` for
the test, the test reads from there and writes the TOML), but the
canonical credential carrier in the in-container boot path is the
TOML.

**Do not put test credentials in repo-root `.env`.** That file is
docker-compose's `env_file` and lives outside the test isolation
boundary. The `project_test_env_isolation` memory exists to keep
this distinction durable across sessions; future test paths must
NOT add a root-`.env` read.

To rotate the Gemini key:

1. Update `tests/.env` with the new value.
2. `make test-live` — confirms the new key authenticates.
3. `make test-e2e` — confirms the in-container reviewer picks it up.
4. Revoke the old key in Google AI Studio.

## TOML by default; ENV is for docker

Production satellites resolves config in the order **process env → TOML
→ in-code defaults**. Integration tests now mirror that path: every
shaped value (port, env, log_level, dev_mode, db_dsn, docs_dir,
oauth_*, api_keys, grants_enforced) lives in a per-test TOML file and
is mounted into the container at `/app/satellites.toml`. Only secrets
(`GEMINI_API_KEY`, `SATELLITES_API_KEYS`) and per-run overrides
(`DB_DSN` pointing at the surreal sibling, `EMBEDDINGS_PROVIDER=stub`,
etc.) flow via the container `Env:` map.

Why: the binary's TOML loader is what PPROD relies on. Tests that
inject everything via ENV never exercise that loader, leaving drift
invisible. The new helper makes TOML the default test-config carrier.

The schema reference is `tests/satellites.example.toml` — it mirrors
the production `satellites.example.toml` so contributors can see at a
glance which keys travel via TOML. As of story_b218cb81, every
production env var (including credentials — `gemini_api_key`,
`gemini_review_model`, `embeddings_*`) lives on
`internal/config.Config`, so the TOML can carry the entire boot
state. The `tests/common` dotenv loader still propagates host-side
secrets from `tests/.env` into the test process env so tests can
read them via `os.Getenv` and write them into the per-test TOML —
no path forwards env vars directly into the container.

### Helpers

- `tests/integration/toml_boot.go::writeTestTOML(t, cfg)` — serialises
  a `map[string]any` of TOML keys to a per-test file in `t.TempDir()`,
  returns the absolute host path.
- `tests/integration/toml_boot.go::startServerWithTOML(t, ctx, opts,
  tomlPath)` — boots the satellites testcontainer with the TOML
  bind-mounted at `/app/satellites.toml` and `SATELLITES_CONFIG`
  pointing at it. Returns `(baseURL, logs func() string, stop)`.
  The `logs` closure drains the container stdout for boot-log
  assertions; `stop` is the deferred terminate.

### Boot-log evidence

`internal/config.Load()` populates `cfg.LoadedTOMLPath()` with the
path it read; `cmd/satellites-server/main.go` emits a
`config: loaded TOML path=…` info line at boot. Tests assert on
this log via `logs()` to prove the TOML loader actually ran (not
that the container booted on env+defaults alone).

`TestConfig_ENVOverridesTOML` (`tests/integration/config_resolution_test.go`)
is the lone integration test of the env-only path going forward — it
boots a container with TOML `port=9090` AND env `PORT=8080`, asserts
the container listens on 8080, and asserts the TOML-loaded log line
appears (proving the resolution order rather than env-by-default).

## Layout

- `tests/.env.example` — committed template enumerating the loader's
  whitelisted keys.
- `tests/satellites.example.toml` — schema reference for the
  TOML helper.
- `tests/common/` — shared helpers (dotenv loader, test fixtures).
- `tests/integration/` — testcontainers-driven integration tests.
- `tests/portalui/` — portal UI integration tests.
