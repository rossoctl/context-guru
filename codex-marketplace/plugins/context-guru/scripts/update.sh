#!/bin/sh
# Run outside Codex so restarting its proxy cannot disrupt the invoking session.
set -eu

STATE_BASE=${XDG_STATE_HOME:-"$HOME/.local/state"}
CONTROLLER="$STATE_BASE/context-guru-codex/codex_plugin.py"

if [ ! -f "$CONTROLLER" ]; then
  echo "context-guru-update: setup has not installed the standalone controller" >&2
  exit 2
fi

exec python3 "$CONTROLLER" update --install
