#!/usr/bin/env bash
# Builds satellites-client and installs it to $SATELLITES_INSTALL_DIR
# (default $HOME/.satellites/bin). Dev + operator workflows converge
# on the same install location; the ./bin carve-out from
# sty_bfd1dd92 is gone. See sty_64e69db8 for the alignment story.
set -euo pipefail

cd "$(dirname "$0")/.."

bash scripts/build.sh client

INSTALL_DIR="${SATELLITES_INSTALL_DIR:-$HOME/.satellites/bin}"
mkdir -p "$INSTALL_DIR"
mv -f satellites-client "$INSTALL_DIR/satellites-client"
echo "built: $INSTALL_DIR/satellites-client"
