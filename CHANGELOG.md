# Changelog

All notable changes to satellites are recorded here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); the project follows semantic versioning starting from the v4 walking-skeleton ship.

The `[satellites]` and `[satellites-agent]` patches in `.version` are bumped per delivered story by the develop contract; `scripts/release-notes.sh` populates the matching section here from the commit log.

## [Unreleased]

(Add entries via `scripts/release-notes.sh`.)

### Fixed

- **`merge_to_main` hot-path regression coverage: pprod converge decoder + empty-commit failure persistence** (sty_7a61ae53) — three new regression tests in `internal/agent/worker/hotpath_test.go` lock in the post-sty_0be97c3e / post-sty_63541aed shape: `TestFetchPprodCommit_DecodesLiveShape` exercises `fetchPprodCommit` end-to-end through `internal/cliremote.Client` against an `httptest.Server` returning the live `SatellitesInfoOutput` envelope (the existing `TestPollPprodConverge_*` family injects `pprodInfoFetcher` and bypasses the decoder, so it could not catch the shape-drift that caused sty_224774f0's three stale-binary merge_to_main failures); `TestFetchPprodCommit_EmptyCommitDecodesEmpty` asserts an empty `server.commit` decodes as `("", nil)` (the contract `pollPprodConverge` relies on); `TestRunMergeToMainHotPath_EmptyCommitConvergeTimeout_PersistsClose` reproduces the iter-2/iter-3 failure (`pprodCommits = ["", "", ""]`) and asserts the persistence helpers still fire with `task_update(closed,failure)` whose reason carries `last=""` so future failures are grep-able. Per `## Version bump policy`: bumps `[satellites-client]` only (`internal/agent/worker/` is consumed by `cmd/satellites-client/`).

- **`story_close` deploy:behind: per-project resolution + `pprod_commit` / `skip_deploy_check` MCP params** (sty_224774f0) — the deploy:behind gate's `resolvePprodCommit` was hardcoded to `c.SatellitesInfo`, so for any non-satellites project (vire, magentus-forge, resumere, …) the gate compared the consumer's release-evidence `pushed_sha` against satellites' own running commit and rejected — three vire stories sat in_progress despite a live deployment. The resolver now branches in priority order: caller-supplied `pprod_commit` → satellites-self (when `SATELLITES_SELF_PROJECT_ID` matches the bound project) → `release-evidence:no-deploy-endpoint` sentinel (so consumer projects see the missing-configuration root cause instead of a misleading `deploy:behind`). Adds `skip_deploy_check` MCP param as the operator-driven escape valve. New tests `TestStoryClose_DeployBehind_PprodCommitMatch`, `_PprodCommitMismatch`, `_SatellitesSelfPath`, `_NoDeployEndpoint`, `_SkipDeployCheck` lock the four resolver branches plus the opt-out. Per `## Version bump policy`: bumps `[satellites-server]` only (`internal/client/`, `internal/mcpserver/`, `internal/config/`, `cmd/satellites-server/`). Deployment wiring follow-up: the satellites-self pprod env must set `SATELLITES_SELF_PROJECT_ID=proj_7a62aedb` for the regression path to fire on satellites' own stories.

- **`merge_to_main` converge poll + `story_close` deploy:behind gate: prefix-match short SHA** (sty_1e2f7ae7) — pprod's `satellites_info` returns the running commit truncated to an 8-char short form, but both gates compared it against the full 40-char pushed SHA using strict `==`/`!=`. Every converge poll hit its deadline and every `story_close` reported a spurious `deploy:behind`. Replaced both predicates with `strings.HasPrefix(pushedSHA, commit) && len(commit) >= 7` (and its inverse at the close-gate) so a non-empty, collision-safe short SHA is accepted as converged. The fix is inlined at both call sites (no shared helper — two adjacent predicates do not justify the abstraction). First dogfood consumer of sty_f8d0157f's `## Version bump policy` rubric: this commit bumps BOTH `[satellites-client]` (worker hotpath touched) AND `[satellites-server]` (story_close touched) in `.version`. Regression tests: `internal/agent/worker/hotpath_test.go::TestPollPprodConverge_AcceptsShortSHA` and `internal/client/story_close_test.go::TestStoryClose_DeployBehind_AcceptsShortSHA`.

### Changed

- **`commit` + `merge_to_main` contracts require `.version` bump per touched binary** (sty_f8d0157f, `epic:cli-primary`) — the satellites-project lifecycle contracts in `config/seed/wksp_5b3257d1/contracts/` gain a `## Version bump policy` section: every commit in the push range must bump `[satellites-server]` / `[satellites-client]` / `[satellites-agent]` in `.version` for whichever binary the commit touches (resolved by changed-files heuristic on `cmd/<binary>/**`, with conventional-commit scope as the tie-breaker for shared trees like `internal/**` and `config/**`). `merge_to_main.md` mirrors the rule with branch-aggregate scoping ("collectively, across the branch") so a docs-only tail commit doesn't trip the rule when an earlier branch commit already bumped the relevant binary. Closes a recurring failure mode where `.version` stayed flat on a push that touched the binary's code paths, leaving the release manifest behind HEAD (the 499bc050 push of sty_796b8fe1 was the motivating incident — code shipped but the tag-gated `client-release` job never fired). Test: `internal/configseed/contract_lint_test.go::TestVersionBumpRubric_PresentInBothBodies` loads both contracts through `RunWorkspace` and asserts the rubric sentinels live in the row body the reviewer agent reads at dispatch, not just the markdown on disk. No CI gate is added — the rubric clause is the single source of truth; if a missed bump still slips past the reviewer in practice, a follow-up story can codify the gate in `release.yml` with concrete evidence.

### BREAKING

- **Server binary renamed: `satellites` → `satellites-server`** (sty_bfd1dd92, `epic:cli-primary`, `order:03`) — the v3-era binary at `cmd/satellites/` (output `bin/satellites`) is renamed to `cmd/satellites-server/` (output `bin/satellites-server`) so the three-binary topology (`satellites-server`, `satellites-client`, `satellites-agent`) is unambiguous. No compat symlink — operators upgrading must update their invocations. Changes propagated through `scripts/build.sh`, `docker/Dockerfile`, `docker/entrypoint-local.sh`. The new `cmd/satellites-client/` Cobra-based CLI scaffolds in this slice; orders 04+05 fill the verb stubs (today every `satellites-client <noun> <verb>` returns "not yet implemented" with exit code 5).

### Added

- **`cmd/satellites-client/` CLI scaffold** (sty_bfd1dd92, `epic:cli-primary`, `order:03`) — Cobra-based binary covering every noun group from `docs/cli-primary-design.md` §2 (story / task / ledger / project / workspace / kv / repo / agent / contract / principle / document / reviewer / role / skill / changelog / session / system / portal / info — 19 groups, ~100 verb stubs total). Persistent flags from cli-printing-press: `--json`, `--compact`, `--quiet`, `--dry-run`, `--stdin`, `--yes`, `--no-input`, `--no-cache`, `--no-color`, `--select`, `--server`, `--token`. Auto-JSON-when-piped (`internal/cliio` + `golang.org/x/term`). Typed exit codes from `internal/cliexit` (0 OK, 2 Usage, 3 NotFound, 4 Auth, 5 Server, 7 RateLimit; 1 reserved for runtime panics). Credential resolution chain in `internal/clicred`: `--token` flag → `SATELLITES_TOKEN` env → `~/.satellites/credentials.json`. Smoke tests in `cmd/satellites-client/main_smoke_test.go` cover root + per-noun help, version, persistent flags, and the not-implemented exit shape. Orders 04+05 (read + mutating verbs) fill in the stubs.

### Added

- **Realtime story-panel updates over WebSocket (focused slice)** (sty_af303c26 — focused slice; full epic deferred to follow-up stories) — the project page's story panel now patches in place when a story's status changes via MCP. The `storyPanel` Alpine factory opens a workspace-scoped WebSocket via the existing `SatellitesWS` client, listens for `story.<status>` events, filters to the current page's project (`data.project_id` match), and updates the matching `tr.story-row`'s `data-status` attribute + `.status-pill` text. No polling, no manual refresh. The substrate (Hub + AuthHub + `/ws` handler + `internal/story/emit.go`) was already in place from slice 10.x; this story just wires the consumer. Document/contract/repo/ledger panel updates, the disconnect "stale data" indicator, and the configuration-driven entity→panel registry are deferred to follow-up stories. Test: `internal/portal/realtime_bridge_test.go` asserts the page exposes the `data-project-id` host attribute the bridge reads.

- **Changelog primitive — V3 parity port (model + store + 5 MCP verbs + project-page panel)** (sty_12af0bdc, `epic:project-page`, `v3-parity`) — new `internal/changelog` package: `Changelog{ID, WorkspaceID, ProjectID, Service, VersionFrom, VersionTo, Content, EffectiveDate, CreatedBy, CreatedAt, UpdatedAt}` with a `Store` interface plus Memory + Surreal implementations. Newest-first List ordering on `(CreatedAt desc, ID desc)`; project-scoped membership filter mirrors the stories/documents/ledger pattern. MCP tools `changelog_add` / `changelog_get` / `changelog_list` / `changelog_update` / `changelog_delete` registered in `internal/mcpserver/mcp.go` and gated on `s.changelog != nil`; all five honour `?project_id=` URL scoping and reject cross-owner/cross-membership access with `not found`. Project page gains a `changelog` panel (registry entry + `_panel_changelog.html`); composite extended with `Changelogs` + `ChangelogsTotal`. Service is free-form so `satellites`, `satellites-agent`, and any future binary share the same surface — no Go-side enum. Tests: `internal/changelog/store_test.go` covers roundtrip, newest-first ordering across versions (0.0.158 → 0.0.159 → 0.0.160), partial Update, Delete, and membership scoping; `internal/mcpserver/changelog_handlers_test.go` covers the full add → list → get → update → delete cycle plus cross-owner rejection.

- **Footer status row — Gemini liveness + agent uptime + extensible registry** (sty_558c0431, `v3-parity`) — the static two-slot footer becomes a three-slot row: left=identity (`SATELLITES` + version + commit), middle=agent uptime (`agent up Hh Mm Ss`, locally ticking between polls), right=status badges driven by a small `footerStatusItems()` registry — adding a new badge is one entry plus a corresponding field on `/api/health`. The footer fetches `/api/health` on load and every 30 s while the tab is visible (pauses while hidden via `visibilitychange`). New `httpserver.LLMPinger` interface (`Configured`, `Ping(ctx)`) plumbs an LLM probe into `/healthz` as the `gemini` field with V3's enum (`ok` / `unreachable` / `not_configured`); `cmd/satellites/main.go` wires a `geminiPinger` adapter that probes `https://generativelanguage.googleapis.com/v1beta/models?key=…` with a 5 s timeout. A failing LLM ping does NOT flip the response status — only `db_ok=false` does (Fly's machine probe stays gated on the DB). Tests: `TestHealthzGeminiField` covers all four pinger states; `TestPortal_Footer_ThreeSlotShape` asserts the rendered DOM testids.

### Changed

- **Project page story panel — V3 parity refinements** (sty_6300fb27, `epic:project-page`) — the standalone `/projects/{id}/stories` route, `handleStoriesList` handler, `stories_list.html` template, and the legacy banner are gone; the project page's story panel is the only surface. The panel now renders each story's tags inline as clickable `tag-chip` buttons (clicking a chip appends its text to the search input — no navigation, `@click.stop` keeps the chip click off the row's expand toggle). The search input parses `order:<field>` (updated|created|priority|status|title) and `status:<all|done|cancelled>` tokens; default render hides `done` and `cancelled` rows (V3 parity) and either status token lifts that. Reordering is client-side over the rendered tbody, keeping the row + detail-row pair together. Each row now carries `data-status` / `data-priority` / `data-title` / `data-created` / `data-updated` / `data-tags` attributes so the parser + reorder logic can read without re-fetching. Tests in `internal/portal/portal_test.go` (the four `TestStoriesList_*` cases) and `internal/portal/stories_list_search_test.go` were deleted; render-time tests in `internal/portal/project_workspace_view_test.go` extended to assert the new data-* attributes + chip shape; chromedp `TestStoryPanel_OrderAndTagChip` covers `order:created` + chip-click in `tests/portalui/project_detail_layout_test.go`.

### Fixed

- **Hamburger / workspace dropdowns open and stay open** (sty_7e53a4e3, `epic:replication`, `portal`, `ui`) — `@click.outside="close"` was bound to the inner dropdown / menu element, but the toggle button is its **sibling**, not a descendant. Clicking the button fired `toggle` (open → true), then the same click bubbled to the document where Alpine's `.outside` listener saw the target outside the dropdown and immediately fired `close` (open → false). Net: dropdown stayed hidden. Fix: move `@click.outside` onto the wrapping `.nav-hamburger-wrap` / `.nav-workspace` div which contains both the button and the dropdown — clicks on the button are now inside the wrapper, so the outside handler correctly skips them. Render-time regression in `internal/portal/nav_hamburger_test.go` (`TestNav_HamburgerOutsideHandlerOnWrapperNotDropdown`, `TestNav_WorkspaceMenuOutsideHandlerOnWrapperNotMenu`) locks the wiring shape.

- **`satellites_story_update` accepts mutable fields** (sty_330cc4ab, `v3-parity`) — the tool's MCP schema previously declared only `{id}`, so callers could not edit any field; the handler also built `story.UpdateFields{}` empty regardless of input. The schema now accepts `title`, `description`, `acceptance_criteria`, `category`, `priority`, `tags`; the handler reads each from the request, validates `category` against the allowed enum (`feature | bug | improvement | infrastructure | documentation`), and passes a populated `UpdateFields` to the store. `tags` replaces wholesale (V3 parity) — pass an empty array to clear. Both `MemoryStore.Update` and `SurrealStore.Update` now apply the fields via a shared `applyUpdateFields` helper. Cross-owner / cross-membership update returns `not found` (matches `story_get`). Unit tests in `internal/story/update_fields_test.go` and handler tests in `internal/mcpserver/story_update_handler_test.go`.

### Added

- **/hooks/enforce permission resolution gate** (story_c08856b2, `epic:role-based-execution`) — new `internal/permhook` package + HTTP handler. `Resolver.Resolve(ctx, sessionID, tool)` walks the chain `(active CI's action_claim) ?? (session-default-install ledger row) ?? deny no_resolved_permissions`. The Active-CI path reads the `agent_id` + `permissions_claim` from the most recent active `kind:action-claim` row scoped to the session's user; the session-default path reads the `permission_patterns` from the most recent active `kind:session-default-install` row tagged `session:<id>`. `Match(patterns, tool)` supports exact equality, prefix-glob (`Bash:git_*`), `<scope>:**` recursion, and bare `*`. `LookupOrchestratorAgent` resolves project > system precedence (workspace tier collapsed because the document substrate restricts workspace-scope to type=role). HTTP handler at `POST /hooks/enforce` returns `{decision, reason, source, patterns, agent_id}`. Wired into `cmd/satellites/main.go` as a `RouteRegistrar` when sessionStore + ledgerStore + contractStore + docStore are all wired. Tests cover the resolution chain end-to-end (`TestResolver_*`), pattern matching cases (`TestMatch`), override precedence (`TestLookupOrchestratorAgent_OverrideChain`), and HTTP-layer smoke (`TestHandler_DenyOrchestratorWrite`, `TestHandler_AllowDevelopEdit`, `TestHandler_BadRequestRejected`). AC7 (SurrealDB-backed integration test) deferred until the unrelated SurrealDB harness failures are unblocked.

### Changed

- **Strict agent_id required on contract_claim** (story_cc55e093, `epic:role-based-execution`) — flips the agent_id arg to REQUIRED at the registration site (`internal/mcpserver/mcp.go:438`); `contract_claim` rejects calls missing `agent_id` with `{"error":"agent_required",...}` and rejects calls passing non-empty `permissions_claim` with `{"error":"permissions_claim_retired",...}`. The legacy caller-submission code path is removed — `permission_patterns` are now sourced exclusively from the allocated agent document. Test fixtures (`claimFixture`, `closeFixture`, `reviewerFixture`) gained a per-phase lifecycle agent map + `agentFor(ciIdx)` helper; every claim test passes `agent_id`. New tests `TestClaim_RejectsMissingAgentID` + `TestClaim_RejectsPermissionsClaim`. Integration test `agents_roles_grant_release_reclaim_test.go` looks up seeded agents via a new `lookupSystemAgentID` helper. Sequenced after story_488b8223 (orchestrator + lifecycle agent seeds shipped); story_c08856b2 will land the enforce-hook handler that uses these patterns.

### Added

- **Default orchestrator role — substrate slice** (story_488b8223, `epic:role-based-execution`) — `seedLifecycleAgents` runs at boot alongside `seedOrchestratorDocs`, creating six system-scope `type=agent` documents (`preplan_agent`, `plan_agent`, `develop_agent`, `push_agent`, `merge_agent`, `story_close_agent`) carrying `permission_patterns` lifted from each lifecycle contract's permitted_actions. Idempotent — existing rows are skipped, not overwritten. `handleSessionWhoami` now writes a `kind:session-default-install` ledger row when a session's `OrchestratorGrantID` is set, capturing `agent_id`, `agent_name`, `permission_patterns`, and `installed_at` in `Structured`. The row is keyed on `session:<id>` and skipped when an active row already exists within the staleness window. Hook-resolution ACs (AC3-AC7) were deferred to a follow-up gated on v4 `/hooks/enforce` handler delivery; AC8 (migration) is N/A pre-deploy. Tests: `TestSeedLifecycleAgents_*` and `TestSessionWhoami_InstallsSessionDefaultLedgerRow`.

- **Agents own permissions — substrate slice** (story_b39b393f, `epic:role-based-execution`) — additive substrate for the role-based-execution pivot. `ContractInstance` gains an `AgentID string` column (memory + surreal); a new `Store.SetAgent` lets the claim handler stamp the field. `contract_claim` accepts an optional `agent_id` argument: when supplied, the action_claim ledger row's `permissions_claim` is sourced from the named `type=agent` document's `permission_patterns` (caller-submitted `permissions_claim` is ignored), the row's `Structured` carries `agent_id` + `source: "agent_document"`, and the CI is stamped with the agent_id. `Document.Validate` now enforces typed `AgentSettings` shape on `type=agent` Structured payloads (`permission_patterns []string`, `skill_refs []string`, etc.) — mistyped values are rejected. Legacy orchestrator-agent fields (`provider_chain`, `tier`, `permitted_roles`, `tool_ceiling`) coexist via permissive (non-DisallowUnknownFields) decoding so existing fixtures stay valid. The strict-required `agent_id` and `permissions_claim` rejection are deferred to a sequenced follow-up after the orchestrator allocation flow lands; AC6 (drop `skill.contract_binding`), AC7 (seed lifecycle agents — reassigned to story_488b8223), AC8 (`pr_contract_separation` revision — gated on v4 deploy), and AC10 (migration test — N/A pre-deploy) are deferred.

### Changed

- **MCP verb namespace flatten** (story_775a7b49, `epic:role-based-execution`) — registered tool names drop the redundant `story_` prefix at the MCP registration site (`internal/mcpserver/mcp.go`): `story_workflow_claim` → `workflow_claim`, and `story_contract_{next,claim,close,resume,respond}` → `contract_{next,claim,close,resume,respond}`. Handler Go function names are renamed in lockstep (`handleStoryWorkflowClaim` → `handleWorkflowClaim`, etc.) for consistency. Pure rename — no functional changes; per `pr_no_unrequested_compat` no aliases or deprecated wrappers are kept. The five verbs the original AC referenced that do not exist in v4 source (`story_workflow_extend`, `story_acceptance_criteria_amend`, `internal_agent_get`/`_invoke`/`_list`) were struck through at preplan; `internal/seeds/` and the seed-loader test are also N/A in v4. The deps-wired registration is asserted by `TestRegisteredToolNames_RenameFlatten` (`internal/mcpserver/registered_tool_names_test.go`); the bare DEV container in `tests/integration/mcp_test.go` checks that no retired name leaks back into `tools/list`. Updates `docs/architecture.md` and `internal/session/session.go` doc-comments.

### Added

- **Configuration typed-document** (story_d371f155, `epic:configuration-bundles`) — new `type=configuration` document bundling refs to one ordered contract list (workflow shape) plus skill and principle ref sets. Project-scoped, CRUD via existing `satellites_document_*` MCP verbs, FK-validated at the store layer (refs must resolve to active documents of the matching type in the same workspace). `internal/document/configuration.go` carries the `Configuration` Go struct + Marshal/Unmarshal helpers; `internal/document/store.go` adds `ErrDanglingConfigurationRef` + `validateConfigurationRefsLocked`; `internal/document/surreal.go` mirrors the validator for the SurrealDB-backed store. Stories and agents will pick a Configuration in follow-up stories of the same epic.

  **Disambiguation**: this Configuration *entity* is distinct from the read-only `/projects/{id}/configuration` viewer page that shipped in story_433d0661 (commit 60bf060) — that page lists existing contract + skill documents per project; the entity is a named bundle of refs that overrides the implicit project default. Two surfaces share the word "configuration" by accident; a future story may rename the viewer page to clarify.

- **Story → Configuration assignment** (story_4ca6cb1b, `epic:configuration-bundles`) — stories now carry an optional `configuration_id` that overrides the project's default workflow at `story_workflow_claim` time. `internal/story/story.go` adds `ConfigurationID *string`; `internal/story/store.go` adds `Store.Update` (narrow — only `configuration_id` for now per `pr_no_unrequested_compat`) plus `Store.ListByConfigurationID` for the reverse-FK gate; `internal/story/surreal.go` mirrors both. New MCP verb `story_update`; `story_create` extended to accept `configuration_id`. `handleStoryWorkflowClaim` forks: when `proposed_contracts` is empty and the story carries a `configuration_id`, the workflow is derived from the Configuration's `ContractRefs` (resolved doc IDs → contract names); when `proposed` is supplied it wins over the Configuration; when no `configuration_id` is set, behaviour is unchanged (project default fallback). `story_get` / `story_list` now include the resolved `configuration_name`. `handleDocumentDelete` gains a reverse-FK gate that rejects deletion of a `type=configuration` document while any open story references it (done/cancelled stories don't block).

- **Agent → default Configuration assignment** (story_fb600b97, `epic:configuration-bundles`) — agent documents (`type=agent`) gain a `default_configuration_id` field in their `Structured` payload via the new `AgentSettings` struct (`internal/document/agent.go`). FK validation in the document store rejects agent writes whose default doesn't resolve to an active `type=configuration` document in the same workspace; mirrored across `MemoryStore` and `SurrealStore`. `handleStoryWorkflowClaim` accepts an optional `agent_id` argument; the resolution chain becomes `proposed_contracts` → `story.configuration_id` → `agent.default_configuration_id` (when `agent_id` supplied) → project default. `document_get` / `document_list` for `type=agent` surface a `default_configuration_name` sibling. The reverse-FK gate in `handleDocumentDelete` is extended to scan agents alongside stories — deleting a Configuration referenced by either is rejected with both `referencing_stories` and `referencing_agents` named in the response.

  *AC vocabulary remap*: the original v3 wording ("`satellites_internal_agent_*`", "internal agent record") was amended at plan-stage to v4's typed-document model. v4 has no separate internal-agent primitive; agents are `type=agent` documents accessed via `satellites_document_*`. Same field, same precedence, same FK semantics.

## [0.0.67] - 2026-04-26

- feat(bootstrap): scaffold satellites-v4 repo, build.sh, per-binary versioning
- docs: add v4 architecture document
- docs: add v4 portal UI design document
- test(integration): add boot smoke test harness + dev doc
- Create .mcp.json
- chore(rename): module path satellites-v4 → satellites
- docs: scrub external v3 IDs from architecture + ui-design
- feat(logging): port arbor + env config from v3
- infra(docker): port Dockerfile, docker-compose, entrypoint from v3
- feat(http): add healthz endpoint + graceful shutdown with arbor logging
- feat(auth): session + devmode + basic username/password
- feat(auth): google + github oauth providers
- feat(mcp): add /mcp streamable http endpoint + satellites_info tool
- feat(portal): ssr login + landing shell
- feat(document): add primitive + ingest-by-path + boot seed
- infra(ci): add release workflow — compile + ghcr + fly deploy
- test(pprod): add opt-in smoke against live url
- infra(scripts): rename script → scripts + add deploy.sh
- fix(deploy): healthcheck-gated surrealdb + env_file passthrough + connect retry
-   feat(v4): add project, ledger, story primitives + MCP verbs + portal   views
- feat(v4): add project, ledger, story primitives + MCP verbs + portal views
- feat(v4): add workspace primitive + membership + default-workspace bootstrap
- feat(v4): add workspace_id to primitives + boot-time backfill across project/story/ledger/document
- feat(v4): workspace-scoped query filtering across primitives + API-key → system workspace
- feat(v4): workspace membership management verbs + admin/last-admin guards
- feat(v4): portal workspace switcher + breadcrumb + session-sticky scope
- fix(ci): hardcode fly-deploy concurrency group — env context not allowed there
- fix(ci): gate fly deploy behind tags/dispatch — push-to-main builds image only
- fix(ci): disable provenance/sbom on docker build — avoids GHCR 403 on first push
- refactor(ci): drop fly deploy + cache, push image to ghcr.io/<repo> only
- fix(ci): push image with plain docker push — buildx push hits GHCR 403 on HEAD
- feat(documents): unified schema with type discriminator + scope + structured + tags (story_509f1111)
- test(documents): use document_get name arg in project_mcp_test (story_509f1111)
- feat(documents): generic CRUD verbs document_create/update/list/delete + id-keyed get (story_c286b5b1)
- feat(documents): document_search verb with structured filters + substring query (story_2e598f53)
- feat(documents): type-specific wrappers principle/contract/skill/reviewer × create/get/list/update/delete/search (story_bb273934)
- feat(ledger): expand schema with type/durability/source_type/tags/structured/status + Actor→CreatedBy (story_368cd70f)
- feat(ledger): verb layer — get/search/recall/dereference + filter args (story_1a037d03)
- feat(ledger): derivations — kv projection, story-timeline, cost rollup (story_f1b3cc88)
- feat(contract): ContractInstance primitive — schema + memory/surreal stores + FK validation (story_3242dfdb)
- feat(contract): entry verbs — workflow_claim + contract_next + project workflow_spec (story_bc2ffbf8)
- feat(contract): keystone claim verb + process-order gate + session registry (story_919908f0)
- feat(contract): close + respond + resume verbs (story_fc7ea589)
- feat(contract): reviewer hook integration — validation_mode=llm/check-based/agent (story_73b3d1c5)
- feat(rolegrant): agent + role document types + role_grant primitive (story_045a613f)
- feat(rolegrant): MCP verbs + grant middleware (story_1efbfc48)
- test(rolegrant): container-backed integration test for MCP verbs (story_1efbfc48)
- feat(rolegrant): orchestrator grant issuance at SessionStart (story_7d9c4b1b)
- feat(contract): required_role gate on story_contract_claim (story_85675c33)
- test(rolegrant): container-backed integration test for required_role (story_85675c33)
- feat(mechanical): deterministic fallback tier for agent_role_claim (story_548ab5a5)
- feat(task): primitive — struct + enums + store with atomic Claim (story_33d90aa0)
- feat(task): MCP verbs task_enqueue/get/list/claim/close + stage hand-off (story_a8fee0cc)
- feat(dispatcher): priority-aware claim + reclaim watchdog (story_b4513c8c)
- fix(task): stage hand-off inherits parent story priority (story_b4513c8c)
- feat(worker): satellites-agent worker loop (story_daa867ae)
- refactor(contract): drop claimed_by_session_id, ClaimedViaGrantID authoritative (story_4608a82c)
- feat(hub): in-process fan-out primitive (story_fe07e6bb)
- feat(ws): /ws endpoint + workspace-scoped auth hub (story_06b09d78)
- feat(hub): store-layer emit hooks for ledger/task/contract/story (story_7ed84379)
- feat(portal): websocket connection indicator + /ws client (story_ac3e4057)
- feat(repo): schema + store for repo primitive (story_85047a2c)
- feat(repo): MCP query surface + jcodemunch proxy interface (story_970ddfa1)
- feat(repo): reindex task handler + ws emit hooks (story_96db34d0)
- feat(repo): stale-check sweep + push-webhook receiver (story_21d22880)
- fix(mcpserver): inject clock into session staleness path (story_3ae6621b)
- feat(repo): native code indexer; delete internal/jcodemunch (story_75a371c7)
- feat(tests): chromedp E2E suite for WS indicator (story_0e5328cd)
- feat(embeddings): Gemini embedder + chunker + stub (story_5abfe61c C1)
- feat(document): chunk store + SearchSemantic (story_5abfe61c C2)
- feat(ledger): chunk store + SearchSemantic + dereference cascade (story_5abfe61c C3)
- feat(embeddings): worker + verb wiring + docs (story_5abfe61c C4)
- feat(embeddings): Surreal chunk stores + production worker.Start (story_5abfe61c C5)
- feat(repo): emit kind:commit ledger rows from webhook (story_51aee7cd)
- feat(documents): version-history retention + portal version-detail route (story_ebaf2157)
- feat(repo): persist commits + Diff API skeleton + portal recent-commits + diff endpoint (story_c2a2f073)
- feat(repo): portal reindex affordance + admin gate + ws progress chip (story_17b53435)
- feat(portal): add ledger/roles/story/tasks views + grants/agents/documents pages
- feat(config): defaults sweep + Describe() + zero-env-var dev startup (story_833541c5)
- feat(portal): theme picker (dark/system/light) + centered .portal-main (story_5dd7167a)
- feat(portal): landing page (v3 wordmark + 01/02/03 grid, dark default) (story_92210e4a)
- feat(portal): dev-mode quick-signin button + DEV nav chip (story_7105204f)
- test(auth): OAuth start + GitHub callback + portalui E2E (story_e96f6022)
- feat(auth,mcp): OAuth bearer + token exchange for /mcp (story_512cc5cd)
- feat(portal): nav replicates v3 shape with hamburger dropdown (story_e7e8b455)
- fix(portal): restore .nav-inner flex wrapper so dashboard nav renders horizontally (story_31d43312)
- fix(httpserver): preserve http.Hijacker through accessLog so /ws upgrade returns 101 (story_fb6ac2d8)
- feat(portal): footer partial replaces nav version-chip; commit suffix omitted when unknown (story_1340913b)
- feat(portal): workspace switcher dedupe; dropdown is position:absolute (story_4d1ef14f)
- fix(portal): workspace menu omits the active workspace + chromedp height test (story_4d1ef14f)
- refactor(portal): typography tokens for control fonts (story_2469358b)
- fix(document): restore substring-on-Query in Search fallback (story_0954a1dc)
- fix(portal): workspace menu placeholder when only active workspace (story_690b8f5c)
- docs(env): document Fly OAuth secrets recipe + add empty-creds test (story_d59705e2)
- fix(projects): seed per-user default project on first login (story_0f415ab3)
- feat(security): CSP/HSTS/XFO headers + per-IP login rate limit (story_d5652302)
- feat(auth): Surreal-backed session store + sweep (story_0ab83f82)
- feat(auth): Surreal-backed user store + UserStore interface (story_7512783a)

## [0.0.66] - 2026-04-26 — pprod-cutover sweep

### Added
- **Auth durability** — `auth.SurrealUserStore` + `UserStore` interface so OAuth-minted users persist across satellites restarts (story_7512783a).
- **Auth durability** — `auth.SurrealSessionStore` + `Sweep` so cookie sessions survive Fly rolling deploys (story_0ab83f82).
- **HTTP security** — `securityHeaders` middleware (CSP, HSTS prod-only, X-Frame-Options, X-Content-Type-Options, Referrer-Policy) + `internal/ratelimit` token-bucket + per-IP login throttle (story_d5652302).
- **Portal** — per-user default project seeded on first login so `/projects` is non-empty out of the box (story_0f415ab3).
- **Portal** — `nav-workspace-empty` placeholder when the user has only their active workspace (story_690b8f5c).
- **Process** — `merge_to_main` project-scope contract + `merge-to-main` skill; workflow_spec now requires preplan → plan → develop → push → merge_to_main → story_close (story_b6925036).

### Fixed
- **Document search** — restore the substring-on-Query branch in `Document.Search` so `document_search` honours its query argument when SearchSemantic is unavailable (story_0954a1dc).

### Documented
- **OAuth setup** — `.env.example` carries a Fly secrets recipe + per-provider callback URLs (story_d59705e2).
- **WS UX audit** — story_ac3e4057's ACs traced to ship locations; no gap (story_47d367b5).

## [0.0.59] - 2026-04-25 — typography sweep

- Typography tokens for control fonts (story_2469358b).

## [0.0.58] - 2026-04-25 — workspace switcher polish

- Workspace switcher omits the active workspace and the dropdown is position:absolute, eliminating nav reflow (story_4d1ef14f).
- Footer partial replaces the nav version chip; commit suffix omitted when unknown (story_1340913b).
- `accessLog` middleware preserves `http.Hijacker` so `/ws` upgrade returns 101 (story_fb6ac2d8).

## [0.0.55] - 2026-04-24 — portal nav v4 shape

- `.nav-inner` flex wrapper restored (story_31d43312).
- Hamburger-dropdown nav shape replicates v3 (story_e7e8b455).

## [0.0.50] - 2026-04-24 — auth + landing

- OAuth bearer + token exchange for `/mcp` (story_512cc5cd).
- OAuth start + GitHub callback + portalui E2E (story_e96f6022).
- Dev-mode quick-signin button + DEV nav chip (story_7105204f).
- Landing page (v3 wordmark + 01/02/03 grid, dark default) (story_92210e4a).
- Theme picker (story_5dd7167a).
- Config defaults sweep + Describe() + zero-env-var dev startup (story_833541c5).

## [0.0.40] - 2026-04-22 — portal CRUD breadth

- Add ledger/roles/story/tasks views + grants/agents/documents pages.

## [0.0.30] - 2026-04-20 — repo + ledger primitives

- Repo reindex affordance + admin gate + ws progress chip (story_17b53435).
- Persist commits + diff API skeleton + portal recent-commits + diff endpoint (story_c2a2f073).
- Document version-history retention + portal version-detail route (story_ebaf2157).
- Webhook receiver writes `kind:commit` ledger rows (story_51aee7cd).

## [0.0.25] - 2026-04-19 — embeddings worker

- Surreal chunk stores + production worker.Start (story_5abfe61c).
- Worker + verb wiring + docs.
- Ledger chunk store + SearchSemantic + dereference cascade.
- Document chunk store + SearchSemantic.
- Gemini embedder + chunker + stub.

## [0.0.20] - 2026-04-18 — repo indexer

- chromedp E2E suite for WS indicator (story_0e5328cd).
- Native code indexer; deleted `internal/jcodemunch` (story_75a371c7).
- Stale-check sweep + push-webhook receiver (story_21d22880).
- Reindex task handler + ws emit hooks (story_96db34d0).
- MCP query surface + jcodemunch proxy interface (story_970ddfa1).
- Schema + store for repo primitive (story_85047a2c).

## [0.0.10] - 2026-04-15 — websocket primitive

- Portal websocket connection indicator + `/ws` client (story_ac3e4057).
- Store-layer emit hooks for ledger/task/contract/story (story_7ed84379).
- `/ws` endpoint + workspace-scoped auth hub (story_06b09d78).
- In-process fan-out hub primitive (story_fe07e6bb).

## [0.0.05] - 2026-04-12 — task + agent primitives

- Satellites-agent worker loop (story_daa867ae).
- Priority-aware claim + reclaim watchdog (story_b4513c8c).
- MCP verbs task_enqueue/get/list/claim/close + stage hand-off (story_a8fee0cc).
- Task primitive — struct + enums + store with atomic Claim (story_33d90aa0).

## [0.0.01] - 2026-04-01 — v4 walking skeleton

- Repo skeleton + .version + cmd/satellites + cmd/satellites-agent stubs.
- HTTP server + healthz + access log + request id middleware.
- Document, project, workspace, story, contract, ledger, rolegrant, session primitives.
- Initial integration test harness with testcontainers + SurrealDB.
