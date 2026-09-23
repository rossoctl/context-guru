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
# THE AGENT'S MODEL, an env var with the historical default so every existing caller (run022 through
# run027) is unaffected. It was hardcoded to aws/claude-sonnet-5.
#
# NOT A FREE KNOB, and the constraint is worth stating where someone might reach for it: extract_llm_sweep
# sends its ask to the REQUEST's own model by construction, because the ask carries only an inventory and
# the outputs are read from that model's prompt cache. So changing this changes the AGENT and the
# ADJUDICATOR together — a haiku agent gets a haiku adjudicator, and there is no configuration that
# separates them (`model` is a refused key on that component). Any comparison across this variable moves
# two things at once.
#
# The model-window document must carry an entry for whatever is set here, or the proxy resolves no window
# and model_info_unresolved goes non-zero — the failure that voided iterations 008-024.
# model-window-128k.json carries aws/claude-{sonnet-5,haiku-4-5} and their bare forms.
LOCA_MODEL="${LOCA_MODEL:-aws/claude-sonnet-5}"
# THE OUTPUT BUDGET, defaulted to LOCA's own default so every earlier run028/run027 invocation is
# unchanged. It is a parameter because it is not a comfort setting: the harness ENDS an episode when a
# response hits this cap, and a killed episode never scores. Measured across iteration 024's ten passes,
# 63 of 150 episodes (42%) ended on a truncated response and none of the 63 scored, while 94% of the
# survivors did -- so accuracy has been close to `1 - kill_rate` and this number is a first-order term in
# every reward comparison the project has made, not a detail.
#
# CHANGING IT CHANGES WHAT IS COMPARABLE. A pass at 8192 cannot be pooled with one at 4096: the arms would
# differ in how often the harness shoots the episode, which is worth more accuracy than any effect being
# measured. Raise it for a whole iteration, never for one arm or one subset of tasks.
LOCA_MAX_TOKENS="${LOCA_MAX_TOKENS:-4096}"

# THINKING, BOUNDED. Empty (the default) reproduces every earlier run exactly: LOCA adds no `thinking`
# field at all when --no-enable-thinking, and Sonnet 5 then applies its OWN adaptive default and thinks
# anyway. That is why the banner has always printed "Extended thinking: DISABLED" while 68% of sonnet's
# responses carried thinking blocks.
#
# Measured on the gateway, same prompt, max_tokens 4096, three repetitions each:
#   field omitted (today)                    4096, 4096, 4096  -- capped 3/3
#   thinking {enabled, budget_tokens 1024}   1327, 1279, 1729  -- capped 0/3
#   thinking {enabled, budget_tokens 2048}   1746, 1459, 1405  -- capped 0/3
#   thinking {disabled}                      1143, 1422, 1419  -- capped 0/3
# So 2048 and 1024 produce the same amount of output: the bound is not binding, HAVING one is what stops
# the runaway. Hence the counter-intuitive shape of this switch -- thinking is turned ON in order to
# BOUND it, because "off" in this rig means "unbounded adaptive".
#
# WHY IT MATTERS BEYOND COST: a truncated response cuts the tool_use JSON mid-object, and the tool then
# fails schema validation. 93 of 96 tool validation failures in the seed-1 baseline, and 120 of 123 in
# arm A, landed immediately after a truncated response. The agent then retries, burning steps.
#
# DO NOT reach for a bigger --max-tokens instead: measured, 8192 let thinking expand into the new budget
# and produced 16 responses at the new wall, the same two zeros, and $18.02 on one task against $2.49.
#
# NOT `effort`. `thinking.adaptive.effort` is rejected 400 ("Extra inputs are not permitted"), and a
# TOP-LEVEL `effort` returns 200 and is SILENTLY IGNORED -- thinking still capped 3/3. `output_config`
# `{effort: low}` does work (1532, 1110, 1342) but LOCA has no flag for it, whereas the two flags below
# are already shipped.
LOCA_THINKING_BUDGET="${LOCA_THINKING_BUDGET:-}"
THINK_ARGS=()
if [ -n "$LOCA_THINKING_BUDGET" ]; then
  THINK_ARGS=(--enable-thinking --thinking-budget-tokens "$LOCA_THINKING_BUDGET")
fi
# BAND selects the declared window AND LOCA's clearing threshold together. They must agree:
# a proxy told 64k while LOCA clears at 32k measures a pressure curve nothing else shares.
CLEAR_AT=$((BAND*1000)); CLEAR_LEAST=$((BAND*1000/4))
set -a; . "$HOME/.cg-bench/env"; set +a
# THE ADJUDICATION DUMP is passed through when the CALLER sets it, and is otherwise absent. Off by
# default because it writes the full prompt and transcript of every ask to disk in the clear — which is
# what makes it the only way to ask "was that judgement right, or is the prompt wrong", and exactly why
# it must be a deliberate act on a controlled box. Set it per pass:
#
#   CG_SWEEP_ASK_DUMP=$HOME/cg-loca/askdump-$NAME bash run027.sh probe
export ANTHROPIC_CUSTOM_HEADERS=          # benchmark traffic must NOT go through Context Guru
export UV_CONSTRAINT="$H/uv-constraints.txt"   # mcp<2 : 2.1.1 removed Server.list_tools
export PATH="$H/bin:$H/.venv/bin:/home/vpcuser/.nvm/versions/node/v22.23.2/bin:$PATH"
command -v npx >/dev/null || { echo "NPX_MISSING -- filesystem MCP tools would silently vanish"; exit 1; }
# CHEAP MODEL RATES, in dollars per MILLION tokens. Restored after being lost in a rig sync: without
# these, PricingConfigured() is false, and extract_llm's economic gate then falls back to built-in LIST
# rates unless the operator's price table happens to answer — measured 333/389/413 times per arm in
# iteration 023, i.e. once per request in all three arms, which means every allow/suppress decision it
# made was taken against list rates and extraction_cost_usd could not be attributed at all.
#
# BELT AND BRACES with the model-window document. That document carries per-token prices, so RatesFor
# answers from it and `cheap_model_price_unconfigured` stays quiet (verified 0 occurrences on the
# iteration 027 probe) — but the gate needs EITHER the card or these vars, and a document that stops
# carrying prices would silently move every gate decision onto list rates. haiku-4.5 list.
export CHEAP_MODEL_PRICE_IN=1.00
export CHEAP_MODEL_PRICE_OUT=5.00
export CHEAP_MODEL_PRICE_CACHE_READ=0.10
export CHEAP_MODEL_PRICE_CACHE_WRITE=1.25

SHIM_PORT=$((PORT+50)); CAP_PORT=$((PORT+70))
echo "ports: proxy=$PORT shim=$SHIM_PORT capture=$CAP_PORT  (6980 = model-info, must not collide)"
if [ "$SHIM_PORT" = 6980 ] || [ "$CAP_PORT" = 6980 ] || [ "$PORT" = 6980 ]; then echo PORT_COLLISION_6980; exit 1; fi

CAPPID=""; PXPID=""; SHPID=""
cleanup() { for p in $SHPID $PXPID $CAPPID; do [ -n "$p" ] && kill "$p" 2>/dev/null; done; }
trap cleanup EXIT

# REUSES ANY LISTENER ON THIS PORT, whatever directory it serves — which is the specific hazard below.
pgrep -f "http.server 6980" >/dev/null || (cd "$H" && nohup python3 -m http.server 6980 --bind 127.0.0.1 >/dev/null 2>&1 &)
sleep 1
# THE BAND HAS TO BE REACHABLE, not merely configured. The line above adopts an already-running
# http.server on :6980 no matter which working directory it was started from, so a stale server from
# another session serves 404s for a file that exists and is correct. Nothing downstream notices: the
# proxy's resolver discards the fetch error and answers from its built-in table, which for a claude
# model is 1,000,000.
#
# THIS IS THE CHECK THAT WOULD HAVE STOPPED ITERATION 024. It ran ten passes with a correct
# model-window-64k.json on disk and an unreachable URL, resolved 1,000,000 on all 2,207 requests,
# and produced a full set of healthy counters describing a configuration that was never in effect.
MODEL_INFO_URL="http://localhost:6980/model-window-${BAND}k.json"
curl -sf "$MODEL_INFO_URL" | grep -q max_input_tokens || {
  echo "MODEL_INFO_UNREACHABLE: $MODEL_INFO_URL"
  echo "  the file may exist and still be unreachable: :6980 is whichever http.server got there first."
  echo "  \`pgrep -af 'http.server 6980'\` then \`readlink /proc/<pid>/cwd\` — it must be $H."
  exit 1
}
rm -f "$H/i022flap-$NAME.jsonl" "$H/i022capfail-$NAME.jsonl" "$H/i022log-$NAME.jsonl"

CAPTURE_UPSTREAM="$ANTHROPIC_BENCHMARK_BASE_URL" CAPTURE_PORT="$CAP_PORT" \
CAPTURE_FLAPLOG="$H/i022flap-$NAME.jsonl" CAPTURE_FAILLOG="$H/i022capfail-$NAME.jsonl" \
CAPTURE_BODYDUMP="$H/i022body-$NAME.json" \
  nohup .venv/bin/python capture_hop_sab.py > "$H/i022cap-$NAME.log" 2>&1 &
CAPPID=$!
for i in $(seq 1 30); do curl -sf "http://localhost:$CAP_PORT/capture-stats" >/dev/null && break; sleep 0.5; done
curl -sf "http://localhost:$CAP_PORT/capture-stats" >/dev/null || { echo CAPTURE_FAILED; exit 1; }
echo "  capture hop pid=$CAPPID on :$CAP_PORT"

MODEL_INFO_URL="$MODEL_INFO_URL" \
ANTHROPIC_UPSTREAM="http://localhost:$CAP_PORT" ANTHROPIC_API_KEY="$ANTHROPIC_AUTH_TOKEN" \
CHEAP_MODEL=aws/claude-haiku-4-5 CHEAP_MODEL_PROVIDER=anthropic \
CHEAP_MODEL_BASE="$ANTHROPIC_BENCHMARK_BASE_URL" CHEAP_MODEL_KEY="$ANTHROPIC_AUTH_TOKEN" \
CHEAP_MODEL_AUTH=bearer LISTEN_ADDR=":$PORT" CACHE_MODE=on INJECT_EXPAND=always CONTEXT_GURU_PREFIX_ASK=1 \
CG_LOG_LEVEL=debug CG_LOG_FILE="$H/i022log-$NAME.jsonl" \
CG_SWEEP_ASK_DUMP="${CG_SWEEP_ASK_DUMP:-}" \
"$BIN" --config "$CFG" > "$H/i022proxy-$NAME.log" 2>&1 &
PXPID=$!
for i in $(seq 1 40); do curl -sf "http://localhost:$PORT/healthz" >/dev/null && break; sleep 0.5; done
curl -sf "http://localhost:$PORT/healthz" >/dev/null || { echo PROXY_FAILED; tail -5 "$H/i022proxy-$NAME.log"; exit 1; }
grep -qE "\"?pipeline\"?[=:]" "$H/i022proxy-$NAME.log" || { echo "PROXY_FAILED: did not bind :$PORT"; exit 1; }
echo "  proxy pid=$PXPID bound :$PORT $(grep -oE "\"?pipeline\"?[=:] ?\"[^\"]+\"" "$H/i022proxy-$NAME.log" | head -1)"

# DID THE DOCUMENT LOAD, asked of the proxy rather than of the file. `model_info_unresolved` is non-zero
# whenever a fetch failed, and it needs no traffic to read — the resolved WINDOW cannot be checked yet,
# because ctx_window is only logged once requests flow. That assertion is at the end of this script.
UNRES=$(curl -s "http://localhost:$PORT/stats" | grep -o '"model_info_unresolved":[0-9]*' | grep -o '[0-9]*$')
if [ -n "$UNRES" ] && [ "$UNRES" != "0" ]; then
  echo "MODEL_INFO_UNRESOLVED=$UNRES: the proxy could not load $MODEL_INFO_URL"
  curl -s "http://localhost:$PORT/stats" | grep -o '"model_info_last_error":"[^"]*"'
  exit 1
fi

SHIM_UPSTREAM="http://localhost:$PORT/anthropic" SHIM_PORT="$SHIM_PORT" SHIM_DIGEST= \
  .venv/bin/python repair_shim_sab.py > "$H/i022shim-$NAME.log" 2>&1 &
SHPID=$!
for i in $(seq 1 30); do curl -sf "http://localhost:$SHIM_PORT/shim-stats" >/dev/null && break; sleep 0.5; done
curl -sf "http://localhost:$SHIM_PORT/shim-stats" >/dev/null || { echo SHIM_FAILED; exit 1; }
echo "  shim pid=$SHPID on :$SHIM_PORT"

echo "########## i022 ARM $NAME (${BAND}k) cfg=$(basename "$CFG") ##########"; date -u
LOCA_ANTHROPIC_BASE_URL="http://localhost:$SHIM_PORT" LOCA_ANTHROPIC_API_KEY="held-by-proxy" \
timeout 21600 .venv/bin/loca run-claude-api -c "$TASKCFG" -m "$LOCA_MODEL" \
  --max-tokens "$LOCA_MAX_TOKENS" ${THINK_ARGS[@]+"${THINK_ARGS[@]}"} \
  --max-workers 8 --max-tool-uses 400 \
  --use-clear-tool-uses --clear-trigger-tokens "$CLEAR_AT" --clear-at-least-tokens "$CLEAR_LEAST" \
  > "$H/i022loca-$NAME.log" 2>&1
echo "loca exit=$?"; date -u
grep -E "Overall Success|Avg Accuracy|Avg Cost" "$H/i022loca-$NAME.log" | tail -4
curl -s "http://localhost:$PORT/stats" -o "$H/st-i022-$NAME.json"
curl -s "http://localhost:$CAP_PORT/capture-stats" -o "$H/cap-i022-$NAME.json"

# THE WINDOW THE PROXY ACTUALLY USED, read off its own log now that traffic has been through it. This is
# a DIFFERENT assertion from the reachability check at the top and it is the one that matters: a document
# that fetches fine but does not name this run's model id falls through to the built-in table just as
# silently as a 404 does, and the built-in answer for a claude model is 1,000,000.
#
# Reported rather than exit 1, because by here the money is already spent — the point is that the pass's
# own output says whether its thresholds meant anything, instead of that fact living only in a log nobody
# reads. Iteration 024 is why: ten passes, $207 an arm, resolved 1,000,000 against a configured 64,000,
# summarize's trigger consequently at 780,000 and never fired once in either arm, every counter healthy.
RESOLVED=$(grep -o '"ctx_window":[0-9]*' "$H/i022log-$NAME.jsonl" 2>/dev/null |
  sort | uniq -c | sort -rn | head -1 | grep -o '[0-9]*$')
if [ "$RESOLVED" != "$CLEAR_AT" ]; then
  echo "!!!!! WINDOW_MISMATCH $NAME: proxy resolved ctx_window=$RESOLVED, band is $CLEAR_AT"
  echo "!!!!! every fraction-based threshold in this pass used the wrong denominator."
  echo "!!!!! DO NOT COMPARE THIS PASS to one that resolved correctly."
else
  echo "  window verified: ctx_window=$RESOLVED throughout (band ${BAND}k)"
fi
echo "ARM_DONE $NAME"
