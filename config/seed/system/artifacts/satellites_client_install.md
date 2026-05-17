---
name: satellites_client_install
tags: [kind:install-schema, v1]
target_install_path: ./.satellites/satellites-client
target_config_path: ./.satellites/satellites-client.toml
default_config:
  repo_path: .
  worktree_root: ./.satellites/worktree
  log_path: ./.satellites/logs
  branch_template: client-{task_id}-from-{base_sha}
auth_bootstrap:
  kind: auth_login
  command: satellites-client auth login
  env_hint: SATELLITES_TOKEN
---
# satellites_init · install schema

This artifact is the **canonical, operator-editable source of truth**
for the install schema the `satellites_init` MCP/HTTP/CLI verb
returns. The schema describes where the consumer project should drop
the `satellites-client` binary, where its TOML lives, and the bootstrap
auth flow the operator runs after the binary lands.

The runtime values come from the **frontmatter** above — not from this
body. Editing a frontmatter value, re-seeding, and calling
`satellites_init` is the configuration-over-code invariant
(`pr_substrate_model`): new behaviour comes from new prose and a
re-seed, not from a code change.

## Fields

| Frontmatter key                       | Meaning                                                                                                                                                  |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `target_install_path`                 | Filesystem path the consumer writes the `satellites-client` binary to (relative to the consumer project root, by convention).                            |
| `target_config_path`                  | Filesystem path the consumer writes the canonical `satellites-client.toml` to.                                                                           |
| `default_config.repo_path`            | The TOML's `[repo].path` — the consumer project's repo root, relative to the `target_install_path` working dir.                                          |
| `default_config.worktree_root`        | The TOML's `[worktree].root` — where the daemon materialises per-task worktrees.                                                                         |
| `default_config.log_path`             | The TOML's `[logging].path` — daemon + per-task log destination.                                                                                         |
| `default_config.branch_template`      | The TOML's `[worktree].branch_template` — git branch name template the daemon uses when minting worktree branches.                                       |
| `auth_bootstrap.kind`                 | Auth bootstrap flow kind the operator runs after the binary lands. `auth_login` for first-time human bootstrap.                                          |
| `auth_bootstrap.command`              | The shell command the operator runs for `kind=auth_login`.                                                                                               |
| `auth_bootstrap.env_hint`             | Env-var name carrying the OAuth bearer after `auth login` mints it (the operator pastes the cleartext into this env var or the TOML's `[auth].token`).   |

## Release-pipeline-derived fields (NOT in this artifact)

The verb also emits `install.{version, build, commit, os, arch,
filename, download_url, sha256}` and `agent_api_key{*}`. Those source
from the **live GitHub release manifest** + the project-bound MCP
session — never from this artifact. Editing this file does not affect
which binary version `satellites_init` recommends; that comes from
`SystemVersion`'s manifest fetch.

## How to flip a value

1. Edit the relevant frontmatter key in this file.
2. Re-seed via `system_seed_run` (MCP) or `satellites-client seed run`
   (CLI).
3. Call `satellites_init` against the same satellites server — the
   payload reflects the new value without recompilation.
