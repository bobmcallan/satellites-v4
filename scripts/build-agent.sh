#!/usr/bin/env bash
# Builds satellites-agent into ./bin/satellites-agent. Thin wrapper
# over `scripts/build.sh agent` (which already stamps ldflags from the
# [satellites-agent] section of .version with -trimpath enabled). The
# wrapper exists because build.sh writes the binary to repo root; the
# operator-facing convention from sty_ccb35588 is that the agent
# binary lives at ./bin/satellites-agent.
set -euo pipefail

cd "$(dirname "$0")/.."

bash scripts/build.sh agent

mkdir -p bin
mv -f satellites-agent bin/satellites-agent
echo "built: bin/satellites-agent"
