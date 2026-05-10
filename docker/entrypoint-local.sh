#!/bin/sh
# Local development entrypoint for satellites.
#
# Current state: satellites boots, logs its version line, and (from story 10.4
# onwards) serves /healthz. Later stories add a wait-for-surreal loop and a
# seed hook; keep this stub until story 10.9 introduces the doc seeding path.
set -e

exec /app/satellites-server
