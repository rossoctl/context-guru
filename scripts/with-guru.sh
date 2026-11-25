#!/usr/bin/env bash
# Run any base-URL-swappable agent through context-guru with minimal effort.
#
#   scripts/with-guru.sh [preset] [-- command ...]
#
# Presets: off, conservative (default), medium, high, xhigh, ultrahigh.
#
# Examples:
#   scripts/with-guru.sh conservative -- claude   # route Claude Code through the proxy
#   scripts/with-guru.sh medium -- codex         # route Codex (OpenAI dialect)
#   scripts/with-guru.sh                           # default preset: conservative. Start proxy, print the env to paste
#
# Starts context-guru-proxy on $PORT (default 4000), exports ANTHROPIC_BASE_URL and
# OPENAI_BASE_URL pointing at it, then execs the given command (or prints the exports
# and waits). Gateway keys pass through from your env: set ANTHROPIC_API_KEY /
# OPENAI_API_KEY before running to have the proxy inject the real key on forward.
set -euo pipefail

PORT="${PORT:-4000}"
BIN="${BIN:-./bin/context-guru-proxy}"

preset="conservative"
if [[ "${1:-}" != "" && "${1:-}" != "--" ]]; then preset="$1"; shift; fi
[[ "${1:-}" == "--" ]] && shift

[[ -x "$BIN" ]] || { echo "build first: CGO_ENABLED=1 go build -o $BIN ./cmd/context-guru-proxy" >&2; exit 1; }

LISTEN_ADDR=":$PORT" "$BIN" --preset "$preset" >/tmp/context-guru.log 2>&1 &
proxy=$!
trap 'kill $proxy 2>/dev/null' EXIT
for _ in $(seq 1 30); do curl -sf "localhost:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.3; done

export ANTHROPIC_BASE_URL="http://localhost:$PORT/anthropic"
export OPENAI_BASE_URL="http://localhost:$PORT/openai/v1"
echo "context-guru --preset $preset on :$PORT  (log: /tmp/context-guru.log)" >&2
echo "  ANTHROPIC_BASE_URL=$ANTHROPIC_BASE_URL" >&2
echo "  OPENAI_BASE_URL=$OPENAI_BASE_URL" >&2

if [[ $# -gt 0 ]]; then
  "$@"                                    # run the agent through the proxy
  echo "== savings ==" >&2; curl -s "localhost:$PORT/stats" >&2 || true; echo >&2
else
  echo "no command given — proxy running; Ctrl-C to stop." >&2
  wait "$proxy"
fi
