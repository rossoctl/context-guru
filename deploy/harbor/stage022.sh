#!/bin/bash
# iter022 per-arm driver. Adapted from stage-sab.sh; every difference is deliberate:
#   - ONE BINARY, three CONFIGS (iter021 and the #137 runner varied the binary; here the code is
#     identical across arms and only the yaml differs, which is what makes B-A attributable).
#   - Rig lives under ~/ now. /tmp is on a 10-day cleaner that already destroyed iteration 021's
#     frozen binary and the offline-rescore captures.
#   - 32k band: 32k declared window, LOCA clearing at 32k.
#   - KILLS BY PID ONLY. A pattern naming the binary also matches THIS SCRIPT'S OWN argv, and the
#     box is shared with another engineer's services.
# CHAIN: LOCA -> repair shim -> cg-proxy -> capture hop -> gateway.
set -uo pipefail
H="$HOME/cg-loca"
cd "$H"
NAME="$1"; BIN="$2"; CFG="$3"; PORT="$4"; TASKCFG="$5"; BAND="${6:-32}"
# BAND selects the declared window AND LOCA's clearing threshold together. They must agree:
# a proxy told 64k while LOCA clears at 32k measures a pressure curve nothing else shares.
CLEAR_AT=$((BAND*1000)); CLEAR_LEAST=$((BAND*1000/4))
set -a; . "$HOME/.cg-bench/env"; set +a
export ANTHROPIC_CUSTOM_HEADERS=          # benchmark traffic must NOT go through Context Guru
export UV_CONSTRAINT="$H/uv-constraints.txt"   # mcp<2 : 2.1.1 removed Server.list_tools
export PATH="$H/bin:$H/.venv/bin:/home/vpcuser/.nvm/versions/node/v22.23.2/bin:$PATH"
command -v npx >/dev/null || { echo "NPX_MISSING -- filesystem MCP tools would silently vanish"; exit 1; }
SHIM_PORT=$((PORT+50)); CAP_PORT=$((PORT+70))
echo "ports: proxy=$PORT shim=$SHIM_PORT capture=$CAP_PORT  (6980 = model-info, must not collide)"
if [ "$SHIM_PORT" = 6980 ] || [ "$CAP_PORT" = 6980 ] || [ "$PORT" = 6980 ]; then echo PORT_COLLISION_6980; exit 1; fi

CAPPID=""; PXPID=""; SHPID=""
cleanup() { for p in $SHPID $PXPID $CAPPID; do [ -n "$p" ] && kill "$p" 2>/dev/null; done; }
trap cleanup EXIT

pgrep -f "http.server 6980" >/dev/null || (cd "$H" && nohup python3 -m http.server 6980 --bind 127.0.0.1 >/dev/null 2>&1 &)
sleep 1
rm -f "$H/i022flap-$NAME.jsonl" "$H/i022capfail-$NAME.jsonl" "$H/i022log-$NAME.jsonl"

CAPTURE_UPSTREAM="$ANTHROPIC_BENCHMARK_BASE_URL" CAPTURE_PORT="$CAP_PORT" \
CAPTURE_FLAPLOG="$H/i022flap-$NAME.jsonl" CAPTURE_FAILLOG="$H/i022capfail-$NAME.jsonl" \
  nohup .venv/bin/python capture_hop_sab.py > "$H/i022cap-$NAME.log" 2>&1 &
CAPPID=$!
for i in $(seq 1 30); do curl -sf "http://localhost:$CAP_PORT/capture-stats" >/dev/null && break; sleep 0.5; done
curl -sf "http://localhost:$CAP_PORT/capture-stats" >/dev/null || { echo CAPTURE_FAILED; exit 1; }
echo "  capture hop pid=$CAPPID on :$CAP_PORT"

MODEL_INFO_URL="http://localhost:6980/model-window-${BAND}k.json" \
ANTHROPIC_UPSTREAM="http://localhost:$CAP_PORT" ANTHROPIC_API_KEY="$ANTHROPIC_AUTH_TOKEN" \
CHEAP_MODEL=aws/claude-haiku-4-5 CHEAP_MODEL_PROVIDER=anthropic \
CHEAP_MODEL_BASE="$ANTHROPIC_BENCHMARK_BASE_URL" CHEAP_MODEL_KEY="$ANTHROPIC_AUTH_TOKEN" \
CHEAP_MODEL_AUTH=bearer LISTEN_ADDR=":$PORT" CACHE_MODE=on INJECT_EXPAND=always CONTEXT_GURU_PREFIX_ASK=1 \
CG_LOG_LEVEL=debug CG_LOG_FILE="$H/i022log-$NAME.jsonl" \
"$BIN" --config "$CFG" > "$H/i022proxy-$NAME.log" 2>&1 &
PXPID=$!
for i in $(seq 1 40); do curl -sf "http://localhost:$PORT/healthz" >/dev/null && break; sleep 0.5; done
curl -sf "http://localhost:$PORT/healthz" >/dev/null || { echo PROXY_FAILED; tail -5 "$H/i022proxy-$NAME.log"; exit 1; }
grep -qE "\"?pipeline\"?[=:]" "$H/i022proxy-$NAME.log" || { echo "PROXY_FAILED: did not bind :$PORT"; exit 1; }
echo "  proxy pid=$PXPID bound :$PORT $(grep -oE "\"?pipeline\"?[=:] ?\"[^\"]+\"" "$H/i022proxy-$NAME.log" | head -1)"

SHIM_UPSTREAM="http://localhost:$PORT/anthropic" SHIM_PORT="$SHIM_PORT" SHIM_DIGEST= \
  .venv/bin/python repair_shim_sab.py > "$H/i022shim-$NAME.log" 2>&1 &
SHPID=$!
for i in $(seq 1 30); do curl -sf "http://localhost:$SHIM_PORT/shim-stats" >/dev/null && break; sleep 0.5; done
curl -sf "http://localhost:$SHIM_PORT/shim-stats" >/dev/null || { echo SHIM_FAILED; exit 1; }
echo "  shim pid=$SHPID on :$SHIM_PORT"

echo "########## i022 ARM $NAME (${BAND}k) cfg=$(basename "$CFG") ##########"; date -u
LOCA_ANTHROPIC_BASE_URL="http://localhost:$SHIM_PORT" LOCA_ANTHROPIC_API_KEY="held-by-proxy" \
timeout 21600 .venv/bin/loca run-claude-api -c "$TASKCFG" -m aws/claude-sonnet-5 \
  --max-workers 8 --max-tool-uses 400 \
  --use-clear-tool-uses --clear-trigger-tokens "$CLEAR_AT" --clear-at-least-tokens "$CLEAR_LEAST" \
  > "$H/i022loca-$NAME.log" 2>&1
echo "loca exit=$?"; date -u
grep -E "Overall Success|Avg Accuracy|Avg Cost" "$H/i022loca-$NAME.log" | tail -4
curl -s "http://localhost:$PORT/stats" -o "$H/st-i022-$NAME.json"
curl -s "http://localhost:$CAP_PORT/capture-stats" -o "$H/cap-i022-$NAME.json"
echo "ARM_DONE $NAME"
