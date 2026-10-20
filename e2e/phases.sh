#!/usr/bin/env bash
# In-container phase runner for the PR #578 review. One function per phase; a phase that crashes
# stops the run (exit 3), a check that fails does not -- checks are judged later, by check.py.
set -uo pipefail
TIER="${1:?tier: smoke|full|stress}"
R=/results; mkdir -p "$R/logs" "$R/snapshots"
PORT=8788; UP=9000; BASE="http://127.0.0.1:${PORT}"
PLUG=/src/context-guru-plugin

phase() {  # phase <name> <function>
  local name="$1" fn="$2" t0 rc; t0=$(date +%s)
  echo "== $name"; "$fn" >"$R/logs/$name.log" 2>&1; rc=$?
  local st=ok; [ $rc -eq 0 ] || st=crashed
  printf '{"phase":"%s","status":"%s","seconds":%d}\n' "$name" "$st" $(( $(date +%s) - t0 )) >>"$R/phases.jsonl"
  if [ $rc -ne 0 ]; then echo "!! $name crashed (rc=$rc); tail:"; tail -20 "$R/logs/$name.log"; exit 3; fi
}

p_preflight() {
  [ "$HOME" = /sandbox ] || { echo "HOME is $HOME, not /sandbox"; return 1; }
  case "$(cd ~ && pwd -P)" in /sandbox) ;; *) echo "HOME resolves outside /sandbox"; return 1;; esac
  for p in $PORT $UP; do
    if curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$p/"; then echo "port $p already in use"; return 1; fi
  done
  for c in python3 node chromium curl jq; do command -v $c >/dev/null || { echo "missing $c"; return 1; }; done
  go vet ./dash/ ./proxy/ ./cmd/... && go build ./...
}

p_suite() { CGO_ENABLED=1 make test; }

p_generate() {
  python3 -m e2e.gen.inventory --tier "$TIER" --home "$HOME" &&
  python3 -m e2e.gen.transcripts --tier "$TIER" --home "$HOME" &&
  python3 -m e2e.gen.shape --home "$HOME" &&
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
  "$PLUG/scripts/settings.py" add --file "$HOME/.claude/settings.json" --url "$BASE/anthropic" --user-scope
  "$PLUG/scripts/settings.py" port install | tee "$R/logs/port.facts"
  grep -q "^port=$PORT" "$R/logs/port.facts"
}

start_proxy() {
  nohup "$HOME/.local/bin/context-guru-proxy" --listen "127.0.0.1:$PORT" --idle-exit=24h --dashboard \
    --dashboard-db "$HOME/.local/state/context-guru/dashboard-$PORT.db" \
    --anthropic-upstream "http://127.0.0.1:$UP" >>"$R/logs/proxy.log" 2>&1 &
  echo $! >/tmp/proxy.pid
  for _ in $(seq 1 60); do curl -fsS -o /dev/null "$BASE/api/history" && return 0; sleep 0.5; done
  echo "proxy did not come up"; return 1
}
stop_proxy() { kill "$(cat /tmp/proxy.pid)" 2>/dev/null; sleep 1; }

p_import() {
  mkdir -p "$HOME/.local/state/context-guru"
  start_proxy && stop_proxy   # creates the DB at the current schema
  /opt/cg/cg-import-history -db "$HOME/.local/state/context-guru/dashboard-$PORT.db" -apply &&
  /opt/cg/cg-import-history -db "$HOME/.local/state/context-guru/dashboard-$PORT.db" -backfill-projects
}

p_replay() {
  nohup python3 -m e2e.upstream --port $UP >>"$R/logs/upstream.log" 2>&1 &
  start_proxy || return 1
  if [ "$TIER" = smoke ]; then
    python3 -m e2e.replay --tier "$TIER" --home "$HOME" --base "$BASE/anthropic" --which post
  else  # Review Focus 3: restart between halves; rows before the restart must survive
    python3 -m e2e.replay --tier "$TIER" --home "$HOME" --base "$BASE/anthropic" --which first_half &&
    stop_proxy && start_proxy &&
    python3 -m e2e.replay --tier "$TIER" --home "$HOME" --base "$BASE/anthropic" --which second_half
  fi
}

p_api()   { python3 -m e2e.collect --base "$BASE" --expected "$R/expected.json" --out "$R/actual-api.json" \
              --skill "$PLUG/scripts/dashboard.sh"; }
p_skill() { "$PLUG/scripts/dashboard.sh" --no-open | tee "$R/logs/skill.out"; }

cd /src
[ "$TIER" = smoke ] || phase suite p_suite
for ph in preflight generate install import replay api skill; do phase "$ph" "p_$ph"; done
echo "== phases complete"
