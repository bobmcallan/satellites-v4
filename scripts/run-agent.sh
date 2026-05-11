#!/usr/bin/env bash
# Runs ./bin/satellites-agent with config-path resolution. The
# operator's preferred config path comes from SATELLITES_AGENT_CONFIG;
# absent, ./bin/satellites-agent.toml is the default (mirrors the
# loader's bin-tier search-path established in sty_5da2ed4d for
# satellites-client). `exec` so SIGINT and SIGTERM forward to the
# binary directly rather than the shell.
set -euo pipefail

cd "$(dirname "$0")/.."

CONFIG_PATH="${SATELLITES_AGENT_CONFIG:-./bin/satellites-agent.toml}"

exec ./bin/satellites-agent --config "$CONFIG_PATH" "$@"
