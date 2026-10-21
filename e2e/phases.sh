#!/usr/bin/env bash
# In-container phase runner for the PR #578 review. One function per phase; a phase that crashes
# stops the run (exit 3), a check that fails does not -- checks are judged later, by check.py.
set -uo pipefail
TIER="${1:?tier: smoke|full|stress|suite}"
R=/results; mkdir -p "$R/logs" "$R/snapshots"
PORT=8788; UP=9000; BASE="http://127.0.0.1:${PORT}"
PLUG=/src/context-guru-plugin

source /src/e2e/timeout.sh

# Evidence of a hung phase, taken while everything is still alive. Python stacks and fd/socket counts first;
# the proxy's goroutine dump last, because SIGQUIT makes Go print every goroutine and then exit.
capture_hang() {  # capture_hang <phase>
  local ev="$R/logs/hang-$1.txt" p pid
  {
    echo "== hang evidence for phase $1 (budget exceeded)"; ps -eo pid,ppid,stat,etimes,args
    echo "== /proc/net/tcp lines: $(wc -l </proc/net/tcp)"
    awk 'NR>1{print $4}' /proc/net/tcp | sort | uniq -c   # socket states, hex (01 ESTABLISHED, 08 CLOSE_WAIT, 06 TIME_WAIT)
    for p in proxy upstream; do
      pid="$(cat /tmp/$p.pid 2>/dev/null)" || continue
      echo "== $p pid $pid fds: $(ls /proc/$pid/fd 2>/dev/null | wc -l)"
    done
    for pid in $(pgrep -f 'e2e\.(upstream|replay)'); do echo "== SIGUSR1 -> $pid (stacks go to that process's log)"; kill -USR1 "$pid"; done
    sleep 2
    pid="$(cat /tmp/proxy.pid 2>/dev/null)" && kill -0 "$pid" 2>/dev/null &&
      { echo "== SIGQUIT -> proxy $pid (goroutine dump goes to logs/proxy.log)"; kill -QUIT "$pid"; sleep 3; }
  } >"$ev" 2>&1
}

phase() {  # phase <name> <function>; a phase over its budget is CRASHED (status timeout), with evidence
  local name="$1" fn="$2" t0 rc budget; t0=$(date +%s); budget=$(phase_budget "$TIER" "$name")
  echo "== $name (budget ${budget}s)"
  run_bounded "$budget" "$fn" "$R/logs/$name.log" "capture_hang $name"; rc=$?
  local st=ok; [ $rc -eq 0 ] || st=crashed
  local why=""; [ $rc -eq 124 ] && why=',"reason":"timeout"'
  [ $rc -ne 0 ] && [ -s /tmp/crash-reason ] && why=",\"reason\":$(head -1 /tmp/crash-reason | jq -Rs 'rtrimstr("\n")')"
  printf '{"phase":"%s","status":"%s","seconds":%d%s}\n' "$name" "$st" $(( $(date +%s) - t0 )) "$why" >>"$R/phases.jsonl"
  if [ $rc -ne 0 ]; then echo "!! $name crashed (rc=$rc); tail:"; tail -20 "$R/logs/$name.log"; exit 3; fi
}

p_preflight() {
  # run.sh passes the host's clock: a VM clock far off it is stepped by chronyd mid-run (seen: +64,957s).
  check_clock "${HOST_EPOCH:-}" 120 >/tmp/crash-reason || { cat /tmp/crash-reason; return 1; }
  [ "$HOME" = /sandbox ] || { echo "HOME is $HOME, not /sandbox"; return 1; }
  case "$(cd ~ && pwd -P)" in /sandbox) ;; *) echo "HOME resolves outside /sandbox"; return 1;; esac
  for p in $PORT $UP; do
    if curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$p/"; then echo "port $p already in use"; return 1; fi
  done
  for c in python3 node chromium curl jq; do command -v $c >/dev/null || { echo "missing $c"; return 1; }; done
  go vet ./dash/ ./proxy/ ./cmd/... && go build ./...
}

# A red package is a result (check.py reads suite.exit), not a crashed phase.
p_suite() { CGO_ENABLED=1 make test; echo $? >"$R/suite.exit"; return 0; }

p_generate() {
  python3 -m e2e.gen.inventory --tier "$TIER" --home "$HOME" &&
  python3 -m e2e.oracle --tier "$TIER" --home "$HOME" --out "$R/expected.json" &&
  python3 -m e2e.oracle --tier "$TIER" --home "$HOME" --out "$R/expected.2.json" &&
  cmp "$R/expected.json" "$R/expected.2.json"   # determinism
}

p_install() {
  mkdir -p "$HOME/.local/bin" "$HOME/.claude"
  cp /opt/cg/context-guru-proxy "$HOME/.local/bin/"
  # install.sh compares CONTEXT_GURU_VERSION to the binary's own version to take the "present" path.
  CONTEXT_GURU_VERSION="$("$HOME/.local/bin/context-guru-proxy" -version 2>&1 | awk '{print $2; exit}')" \
    "$PLUG/scripts/install.sh" | tee "$R/logs/install.facts"
  grep -q '^result=present' "$R/logs/install.facts" || { echo "install.sh did not take the present-binary path"; return 1; }
  [ -f "$HOME/.claude/settings.json" ] || echo '{}' >"$HOME/.claude/settings.json"
  "$PLUG/scripts/settings.py" add --file "$HOME/.claude/settings.json" --url "$BASE/anthropic" --user-scope \
    | tee "$R/logs/settings-add.facts"
  grep -qE '^result=(ok|added)' "$R/logs/settings-add.facts" || { echo "settings.py add did not report result=added|ok"; return 1; }
  "$PLUG/scripts/settings.py" port install | tee "$R/logs/port.facts"
  grep -q "^port=$PORT" "$R/logs/port.facts"
}

DB="$HOME/.local/state/context-guru/dashboard-$PORT.db"

start_upstream() {
  nohup python3 -m e2e.upstream --port $UP >>"$R/logs/upstream.log" 2>&1 &
  echo $! >/tmp/upstream.pid
  for _ in $(seq 1 40); do
    (exec 3<>"/dev/tcp/127.0.0.1/$UP") 2>/dev/null && return 0
    kill -0 "$(cat /tmp/upstream.pid)" 2>/dev/null || break
    sleep 0.25
  done
  echo "fake upstream did not come up"; return 1
}

start_proxy() {
  mkdir -p "$(dirname "$DB")"
  nohup "$HOME/.local/bin/context-guru-proxy" --listen "127.0.0.1:$PORT" --idle-exit=24h --dashboard \
    --dashboard-db "$DB" \
    --anthropic-upstream "http://127.0.0.1:$UP" >>"$R/logs/proxy.log" 2>&1 &
  local pid=$!; echo $pid >/tmp/proxy.pid
  for _ in $(seq 1 60); do
    kill -0 $pid 2>/dev/null || { echo "proxy pid $pid exited"; return 1; }
    curl -fsS -o /dev/null "$BASE/api/history" && return 0; sleep 0.5
  done
  echo "proxy did not come up"; return 1
}
stop_proxy() {
  local pid; pid="$(cat /tmp/proxy.pid 2>/dev/null)" || return 0
  kill "$pid" 2>/dev/null
  for _ in $(seq 1 40); do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.25; done
  echo "proxy pid $pid did not exit"; return 1
}

# Wait for the proxy's OWN evidence that inventory is written (tool_declarations rows for every replayed
# session), so a later stop does not lose a tail the way finding #1 (invWriter not drained on Close) would.
# A timeout is recorded, not fatal: the later check judges it.
settle_inventory() {  # settle_inventory <label>
  local want t0 n=0 res; t0=$(date +%s%3N)
  want=$(python3 -c "import json;print(len({k.split('@')[0] for k in json.load(open('$R/replay-stamps.json'))}))")
  while :; do
    n=$(sqlite3 "$DB" "SELECT COUNT(DISTINCT session_id) FROM tool_declarations" 2>/dev/null || echo 0)
    [ "$n" -ge "$want" ] && { res="$(( $(date +%s%3N) - t0 ))"; break; }
    [ $(( $(date +%s%3N) - t0 )) -ge 20000 ] && { res='"timeout"'; break; }
    sleep 0.25
  done
  printf '{"phase":"replay","label":"%s","sessions_wanted":%d,"sessions_seen":%d,"inventory_settle_ms":%s}\n' \
    "$1" "$want" "$n" "$res" >>"$R/settle.jsonl"
}

settle_session() {  # settle_session <session id>: wait (bounded) for its inventory rows, as settle_inventory does
  local t0 n=0; t0=$(date +%s)
  while [ $(( $(date +%s) - t0 )) -lt 20 ]; do
    n=$(sqlite3 "$DB" "SELECT COUNT(*) FROM tool_declarations WHERE session_id='$1'" 2>/dev/null || echo 0)
    [ "$n" -gt 0 ] && return 0; sleep 0.25
  done
  return 0
}

REPLAY="python3 -m e2e.replay --tier $TIER --home $HOME --base $BASE/anthropic --stamps $R/replay-stamps.json"

p_replay() {
  start_upstream || return 1
  start_proxy || return 1
  if [ "$TIER" = smoke ]; then
    $REPLAY --which post || return 1
  else  # Review Focus 3: restart between halves; rows before the restart must survive
    $REPLAY --which first_half || return 1
    settle_inventory first_half; stop_proxy && start_proxy || return 1
    $REPLAY --which second_half || return 1
  fi
  settle_inventory end
  # Shutdown probe for finding #1: one fresh-session request, then an IMMEDIATE stop. Its
  # tool_declarations rows exist only if shutdown drains the inventory writer.
  $REPLAY --which post --probe=-shutdown-probe --stamps "" --probe-out "$R/shutdown-probe.json" || return 1
  stop_proxy && start_proxy
}

p_transcripts() {
  python3 -m e2e.gen.transcripts --tier "$TIER" --home "$HOME" --stamps "$R/replay-stamps.json" &&
  python3 -m e2e.gen.shape --home "$HOME"
}

# The user installs, works through the proxy, and imports history afterwards. The importer writes
# the DB directly, so the proxy is stopped around it.
p_import() {
  stop_proxy || return 1
  /opt/cg/cg-import-history -db "$DB" -apply &&
  /opt/cg/cg-import-history -db "$DB" -backfill-projects &&
  start_proxy
}

# LiteLLM.Price answers "unknown" until its first (background) fetch lands, so right after a proxy start every
# item reads priced=false. Poll until some item is priced; a timeout is not fatal, the money checks then
# report BLOCKED("no priced items") rather than a false FAIL. Polls the same URL collect reads: the dashboard
# caches a response for 30s and serves it stale while refreshing (dash/api.go), so a cold answer outlives 30s.
warm_prices() {
  for _ in $(seq 1 180); do
    curl -fsS "$BASE/api/insights?timeline=1" | jq -e '[.items[]? | select(.priced)] | length > 0' >/dev/null && return 0
    sleep 0.5
  done
  echo "price cache did not warm"; return 0
}

p_api() {
  warm_prices
  python3 -m e2e.collect --base "$BASE" --expected "$R/expected.json" --out "$R/actual-api.json" \
    --skill "$PLUG/scripts/dashboard.sh" || return 1
  # Unpriced probe, AFTER the main collection: an item is priced only if every session in scope that
  # declares it is priced (dash/toolapi.go ToolReportFor), so an unpriced session in alpha would flip every
  # alpha and pooled item to unpriced and leave the money checks nothing to judge. Stats were read above, so
  # this request is not in the expected request counts (counted=false).
  $REPLAY --which post --model e2e-unpriced-model --probe=-unpriced-probe --stamps "" \
    --probe-out "$R/unpriced-probe.json" || return 1
  python3 - "$R/unpriced-probe.json" <<'PY' || return 1
import json, sys
p = sys.argv[1]; d = json.load(open(p)); d["counted"] = False; json.dump(d, open(p, "w"))
PY
  settle_session "$(python3 -c "import json;print(json.load(open('$R/unpriced-probe.json'))['session_id'])")"
  python3 -m e2e.collect --base "$BASE" --out "$R/actual-api.json" --add-unpriced-probe "$R/unpriced-probe.json" || return 1
  mkdir -p "$R/db"   # mock data only; kept for diagnosing a mismatch
  sqlite3 "$DB" ".backup '$R/db/dashboard-$PORT.db'"
}
p_ui() {
  chromium --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage --remote-debugging-port=9222 \
    --user-data-dir=/tmp/chrome about:blank >"$R/logs/chromium.log" 2>&1 &
  for _ in $(seq 1 40); do curl -fsS -o /dev/null http://127.0.0.1:9222/json/version && break; sleep 0.25; done
  node /src/e2e/snap.mjs "$BASE" "$R/expected.json" "$R" "$TIER"
}
# dashboard.sh exits non-zero when nothing answers on its port; that is the product's behaviour and a
# result for S7.skill_port (collect.py records the exit code), not a crashed phase.
p_skill() { "$PLUG/scripts/dashboard.sh" --no-open 2>&1 | tee "$R/logs/skill.out"; return 0; }
# Endpoint timings at stress scale. A non-200 or a timeout is a crashed phase, never a 0 ms timing.
p_endpoints() {
  local ep out
  for ep in /api/history "/api/facets?measured=1" "/api/insights?timeline=1"; do
    out="$(curl -s -o /dev/null --max-time 60 -w '%{http_code} %{time_total}' "$BASE$ep")"
    [ "${out%% *}" = 200 ] || { echo "$ep answered '${out%% *}'" >/tmp/crash-reason; cat /tmp/crash-reason; return 1; }
    printf '{"endpoint":"%s","ms":%s}\n' "$ep" "${out#* }" >>"$R/stress-endpoints.jsonl"
  done
}
p_assert() { python3 -m e2e.check --results "$R" --tier "$TIER"; return 0; }  # a FAIL is a result, not a crash

cd /src
case "$TIER" in
  # Stress is about time, not correctness: transcripts precede import because the importer reads them.
  stress) for ph in preflight generate install replay transcripts import; do phase "$ph" "p_$ph"; done
          phase endpoints p_endpoints ;;
  # The KNOWN baseline on main: the Go suite only (main has no History tab, so the other phases cannot run).
  suite)  phase suite p_suite; phase assert p_assert ;;
  full)   phase suite p_suite
          for ph in preflight generate install replay transcripts import api skill ui assert; do phase "$ph" "p_$ph"; done ;;
  smoke)  for ph in preflight generate install replay transcripts import api skill ui assert; do phase "$ph" "p_$ph"; done ;;
esac
echo "== phases complete"
