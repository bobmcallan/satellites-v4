#!/usr/bin/env bash
# Builds satellites-client into ./bin/satellites-client. Thin wrapper
# over `scripts/build.sh client` (which stamps ldflags from the
# [satellites-client] section of .version). The wrapper exists
# because build.sh writes the binary to repo root; the operator-
# facing convention from cli-primary order:03 (sty_bfd1dd92) is that
# operator binaries live at ./bin/<name> alongside ./bin/satellites-agent.
set -euo pipefail

cd "$(dirname "$0")/.."

bash scripts/build.sh client

mkdir -p bin
mv -f satellites-client bin/satellites-client
echo "built: bin/satellites-client"
