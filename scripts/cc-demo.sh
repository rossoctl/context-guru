#!/usr/bin/env bash
# Real Claude Code integration demo: route `claude` through context-guru and read /stats.
# Requires: proxy built (go build -o bin/context-guru-proxy ./cmd/context-guru-proxy),
# and ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN in the env (an Anthropic-compatible endpoint).
#
# Usage: ANTHROPIC_BASE_URL=... ANTHROPIC_AUTH_TOKEN=... scripts/cc-demo.sh
set -euo pipefail

PORT="${PORT:-8090}"
MODEL="${CG_MODEL:-claude-haiku-4-5}"
BIN="${BIN:-./bin/context-guru-proxy}"
: "${ANTHROPIC_BASE_URL:?set ANTHROPIC_BASE_URL to the upstream model endpoint}"
: "${ANTHROPIC_AUTH_TOKEN:?set ANTHROPIC_AUTH_TOKEN}"

demo=$(mktemp -d)
printf 'package main\nimport "fmt"\nfunc main(){ fmt.Println(greet("world")) }\nfunc greet(n string) string { return "hello "+n }\n' > "$demo/main.go"
printf '# cc-demo\nTiny repo for the context-guru Claude Code integration demo.\n' > "$demo/README.md"

# agent preset: dedup/extract/extract_llm — the levers for long Claude Code sessions.
# Upstream + cheap model (for extract) both point at the same Anthropic-compatible endpoint.
LISTEN_ADDR="127.0.0.1:$PORT" \
ANTHROPIC_UPSTREAM="$ANTHROPIC_BASE_URL" \
CHEAP_MODEL="$MODEL" CHEAP_MODEL_PROVIDER=anthropic \
CHEAP_MODEL_BASE="$ANTHROPIC_BASE_URL" CHEAP_MODEL_KEY="$ANTHROPIC_AUTH_TOKEN" CHEAP_MODEL_AUTH=bearer \
  "$BIN" --preset agent >/tmp/cc_proxy.log 2>&1 &
proxy=$!
trap 'kill $proxy 2>/dev/null' EXIT
for _ in $(seq 1 30); do curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.3; done

settings=$(mktemp)
cat > "$settings" <<JSON
{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:$PORT/anthropic","ANTHROPIC_AUTH_TOKEN":"$ANTHROPIC_AUTH_TOKEN","ANTHROPIC_MODEL":"$MODEL","ANTHROPIC_SMALL_FAST_MODEL":"$MODEL"}}
JSON

echo "== stats before ==" ; curl -s "http://127.0.0.1:$PORT/stats"; echo
# Read-only demo prompt. For a fully non-interactive run, pre-authorize the read
# tools (e.g. --allowedTools "Read") rather than disabling approvals wholesale.
( cd "$demo" && claude -p "Read main.go and README.md and summarize each in one line." \
    --settings "$settings" )
echo "== stats after =="  ; curl -s "http://127.0.0.1:$PORT/stats"; echo
rm -f "$settings"
