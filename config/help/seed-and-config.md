---
title: Seed & System Config
slug: seed-and-config
order: 60
tags: [help, seed, config]
---
# Seed & System Config

Substrate configuration (agents, contracts, workflows, help, plus
project-tier overrides) is **markdown in the repo**. The boot path
reads two parallel trees and upserts each file into the document
store. Markdown is the single source of truth.

## Layout

```
config/
  seed/
    system/                -> system tier (scope=system rows)
      agents/      *.md
      contracts/   *.md
      workflows/   *.md
      principles/  *.md
      artifacts/   *.md
      story_templates/
      replicate_vocabulary/
    <project_id>/          -> project tier (scope=project rows)
      agents/      *.md
      contracts/   *.md
      principles/  *.md
      artifacts/   *.md
  help/            *.md    -> type=help
```

## Two tiers, strict isolation

- **System tier** lives under `config/seed/system/<kind>/`. The
  system loader produces only `scope=system` rows and never walks
  any `proj_*` subdirectory.
- **Project tier** lives under `config/seed/<project_id>/<kind>/`.
  The project loader produces only `scope=project, project_id=<id>`
  rows and never walks the `system/` subtree.

The two trees sit as siblings under `config/seed/` so the boundary
is visually obvious — there is no chance of a future kind name
shadowing or being shadowed by a project_id.

A project tier directory whose `<project_id>` does not resolve to an
existing project row is skipped at boot with a structured warning —
the loader never auto-creates projects.

## Frontmatter

YAML envelope between `---` markers. Required fields per kind:

- `agent`: `name`, `permission_patterns`.
- `contract`: `name`, `category`, `evidence_required`.
- `workflow`: `name`, `required_slots`.
- `help`: `title`, `slug`.

The body is the human description; for help docs the body is
the rendered page itself.

## Re-seed without restart

Global admins can trigger a re-seed without restarting the
server via two MCP verbs (or, for system, the **System Config**
page in the hamburger menu):

- `system_seed_run` — re-runs the system tier (every kind subdir
  at the seed-dir root, plus `config/help/*.md`). Writes a
  `kind:system-seed-run` ledger row.
- `project_seed_run(project_id)` — re-runs the project tier for
  one project (`config/seed/<project_id>/<kind>/*.md`). Writes a
  `kind:project-seed-run` ledger row attached to that project.

Both share the same `{loaded, created, updated, skipped, errors}`
summary shape. Idempotent — a re-run with unchanged file bodies
short-circuits via body-hash and counts files as `skipped`.

## Boot behaviour

- The system seed runs synchronously at startup.
- The project seed pass runs **as a goroutine after the server is
  ready** — it must not impede startup. Errors land in the boot
  log at warn; missing project rows for a discovered `proj_*`
  directory log a warning and skip that subtree.

## Env overrides

- `SATELLITES_SEED_DIR` — defaults to `./config/seed`. Both tiers
  resolve from this single root; project subtrees live under
  `<seed_dir>/proj_*`.
- `SATELLITES_HELP_DIR` — defaults to `./config/help`.

## Limitations

- The loader is **upsert-only** — files removed from disk do not
  archive their corresponding documents. Removal is a future
  story.
- Idempotence relies on body-hash convergence. Drift in the
  structured payload alone is not detected; if you change only
  frontmatter, also tweak the body (or re-seed twice).
