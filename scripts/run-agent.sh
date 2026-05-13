#!/usr/bin/env bash
# Runs satellites-agent from $SATELLITES_INSTALL_DIR (default
# $HOME/.satellites/bin) with config-path resolution. The operator's
# preferred config path comes from SATELLITES_AGENT_CONFIG; absent,
# $HOME/.satellites/satellites-agent.toml is the default. `exec` so
# SIGINT and SIGTERM forward to the binary directly rather than the
# shell. See sty_64e69db8 for the alignment story.
set -euo pipefail

cd "$(dirname "$0")/.."

INSTALL_DIR="${SATELLITES_INSTALL_DIR:-$HOME/.satellites/bin}"
CONFIG_PATH="${SATELLITES_AGENT_CONFIG:-$HOME/.satellites/satellites-agent.toml}"

exec "$INSTALL_DIR/satellites-agent" --config "$CONFIG_PATH" "$@"
