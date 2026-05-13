● Bootstrap complete. State:

  - Project: one proj_85eb971a (Default), owned by you. No repos registered.
  - satellites_info: v0.0.158 / commit 56167837 confirmed live.

  Two questions before I create anything — the answers shape the rest:

  1. Project shape. V3 was project-per-repo. V4 currently has a single "Default" project. Three options:
  - (a) Rename "Default" → "satellites" and register this repo under it. Lightweight.
  - (b) Create a new "satellites" project alongside Default and register here. Project-per-repo, V3-style.
  - (c) Keep Default as-is and register the repo under it (same project, mixed scope later).

  2. Document vs story for the methodology work. "Hamburger is broken" is a bug story, but the thing you're really asking for is two pieces of durable infrastructure that should
  outlive this fix:
  - A runbook document ("how to diagnose a UI bug from a vague report") — lives in documents, gets referenced by future bug stories.
  - A story to actually do the work: fix the hamburger + add the integration test + add the post-deploy smoke test.

  I'd suggest the document captures the method (root-cause recipe — repro → DOM/console inspection → suspect change set → fix → test that would have caught it → post-deploy
  probe), and each bug story references it as the standard. That way "methodology" isn't trapped in this conversation; it's a thing future-you (or another agent) can document_get
  and apply.

  Does (b) + runbook-doc + bug-story sound right? If yes I'll create them. If you want a different shape (e.g. methodology as a principle instead of a document, or skip the doc
  and put it in CLAUDE.md), say so.

✻ Baked for 1m 26s

※ recap: Goal: align satellites MCP with this repo and stand up a methodology for diagnosing/preventing bugs like the broken hamburger menu. Next action: confirm project shape
  (rename Default, new "satellites" project, or reuse Default) so I can register the repo and create the runbook doc plus bug story. (disable recaps in /config)

❯ - Default project should be removed. This would assume every project starts with a default project, however that is confusing, as that is mutiple repos for the same project,
  and stories therefore would not make sense.

  - The repo is the connector and hence Satellites (remote repo address) is the unique definer. Is a repo has no remote, project nees to be created manually.

  - The MCP config (.mcp.json) can be limited to a specific project (V3 has projectid=xxx in the url). This (url) needs to be listed in the project configuration (like V3)

  - No, a document is not required for a bug. Documents are project assets, which defined specs/architecture/etc. Stories define the bug.

  - THe issue is asking claude (you) to replicate the issue, and this is specific to Satelllites, however can broaden out. HOW. Lets focus on Satellites (v4), rreviously, you
  have could not login to the site, and so we implemented a SSR MCP get, which enabled a rendered page recal (HTML). This is an approach, however the best is an actualy page
  analysis, useing chromedp (or alt) and clicking the buttons/menu items. Discuss approaches, specifially for Satellites, and then how this could extend to other applications.

  - Revise your method/runbook, where documents are not the center, stories are. How can this become part of satellites offer.

● Understood — let me work through each thread.

  Project model corrections

  Got it:
  - Remove Default. No auto-created project.
  - Project is keyed on remote URL (canonical identity). One project per remote.
  - No-remote repos require explicit project_create. Don't assume.
  - Project record carries its MCP URL (with ?project_id=… baked in) so .mcp.json scopes to one project. V3 had this; V4 doesn't yet — project_create only takes name. That's a
  missing field on the project schema, not just a config convention.

  The V4 MCP server today is project-agnostic and every tool takes project_id as a parameter. To match V3, the gap is: (1) add mcp_url (or derive from project_id) to the project
  record, (2) make the MCP handler honour ?project_id=… in the URL and reject cross-project access, (3) project_get returns the connection string the user pastes into .mcp.json.
  That's a story on its own.

  Replicating UI issues — approach for Satellites, then general

  Three layers, ranked by the level of fidelity:

  1. SSR HTML fetch (what we already have via the portal/MCP get). Cheap, deterministic, no JS execution. Catches: missing markup, server-rendered errors. Misses: Alpine handlers,
   JS toggles, anything that runs after DOMContentLoaded. The hamburger lives in this gap — the markup is probably fine; the click handler doesn't fire.
  2. Headless browser via chromedp (already a transitive dep — github.com/chromedp/sysutil is in go.mod, and tests/portalui/ already uses it). A Satellites MCP tool — call it
  portal_replicate — that takes a path + a sequence of actions (navigate, click[selector], wait[selector], screenshot, dom_snapshot, console_log) and returns the result. For the
  hamburger: navigate /, click [data-test=nav-toggle], wait 200ms, dump the dropdown's class list and any console errors. That's enough to identify "click fired but Alpine threw"
  vs "click never bound" vs "selector wrong."
  3. Real-session driver. Chromedp with cookie injection from a real authenticated user session — needed for pages behind auth. We already have the SessionStore; the tool would
  mint a one-shot cookie scoped to the runner.

  For Satellites specifically: layer 2 is the win, because we own the app and can ship portal_replicate as a first-class MCP tool. Layer 3 enables flows that need login.

  For other applications: same shape, but with two new concerns —
  - Auth. Project record carries credentials/secrets (encrypted) for the target app. The replicate tool injects them.
  - Trust. Driving an external site from Satellites' infra is a reach; sandboxing matters. Probably gate it on project setting + per-call confirmation.

  The story-centered angle is that the tool attaches evidence to a story, not returns a floating blob. So portal_replicate(story_id=..., actions=[...]) produces a ledger_append
  against the story — DOM snapshot, console log, screenshot blob — and the contract that closes the story can verify "yes, evidence was captured and the assertion held."

  Story-centered methodology — Satellites offering shape

  If stories are the center, the methodology lives in story shape + tool integration + contracts, not docs. Sketch:

  - Story categories with required-field schemas. category: bug requires: repro (a chromedp action sequence), observed, expected, root_cause (filled at fix time), fix_commit
  (filled when committed), regression_test_path, post_deploy_check (URL + assertion).
  - Lifecycle hooks (Satellites contracts):
    - On status: in_progress → require repro to be runnable; portal_replicate runs it and confirms the bug reproduces.
    - On status: ready_to_close → run repro again post-merge against the live URL; assertion must flip from "bug present" to "bug absent." Ledger entry attached.
    - On status: closed → contract checks regression_test_path exists and is referenced by CI.
  - Tools the satellites server provides:
    - portal_replicate (chromedp driver, story-scoped).
    - story_attach_repro / story_attach_evidence (sugar over ledger_append).
    - A post-deploy probe runner that picks up post_deploy_check from any recently-closed bug story and re-runs it on a schedule (the failing case becomes a Satellites-emitted
  alert / new bug story).

  The "what's the methodology" question becomes "what does a bug story require." It's enforced by the system, not by anyone remembering a runbook.

  What I'd build first

  This is several stories. Order I'd suggest, smallest first:

  1. Project schema gap: project keyed on remote URL + mcp_url field + .mcp.json-ready string from project_get. Removes Default. (foundation)
  2. portal_replicate MCP tool: chromedp driver, story-scoped, captures DOM + console + screenshot. (tooling)
  3. bug story shape + lifecycle contracts: required fields, repro hook, post-deploy hook. (methodology)
  4. The hamburger fix itself as the first story exercising the new shape — proves the loop end-to-end. (validation)

  Want me to delete the Default project and create the satellites project (keyed on git@github.com:bobmcallan/satellites.git) now as step 0, even before the schema work? It gets
  us off the broken assumption immediately, and the schema/mcp_url story can land into it.
