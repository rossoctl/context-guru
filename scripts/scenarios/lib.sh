#!/bin/bash
# Shared rig for the pre-expiry-summary-gate scenario runs. See
# docs/proposals/timely-compact-validation.md for what each arm proves.
#
# WHY THESE ARE CHECKED IN. Every number this PR argues from came out of a run like this, and a
# measurement nobody else can re-run is an assertion. The three arms are the ones that found real
# defects: the credit's RATE (arm C), the credit's ACCUMULATION over repeated cold events (arm B),
# and the FIRING RATE of the shipped default (arm A) — which is the finding that does not depend on
# any of the code in this PR being right.
#
# ISOLATION, which is the reason this is a library rather than something retyped per run:
#   - Its own proxy binary, port, config and dashboard db per arm. The production Context Guru is
#     not in the request path at any point.
#   - Its own CLAUDE_CONFIG_DIR. Claude Code's settings.json env OVERRIDES the process env, so
#     exporting ANTHROPIC_BASE_URL before launching `claude` does nothing at all — the first attempt
#     at this sent every request to the production Guru silently. The endpoint has to be written
#     into a copied settings.json, which scen_home does.
#   - Upstream is whatever gateway CG_SCEN_UPSTREAM names, reached directly by the test proxy.
#
# This is a FUNCTIONAL validation of the proxy, so the traffic must go THROUGH Context Guru. It is
# not a benchmark of an agent and nothing here is compared against a non-Guru baseline, so the
# "benchmarks bypass Guru" rule does not apply. See the doc.
#
# Configuration, all overridable:
#   CG_SCEN_ROOT      where scratch state goes            (default ./.cg-scen)
#   CG_SCEN_SRC       the tree to build the proxy from    (default the repo root)
#   CG_SCEN_UPSTREAM  the plain provider gateway          (REQUIRED — no default is safe)
#   CG_SCEN_MODEL     the model every turn uses           (default claude-haiku-4-5)
#   CG_SCEN_HOME_SRC  a working Claude config to copy     (default ~/.claude)
#
# Build the source from a FROZEN copy rather than a tree you are still editing: the binary under
# test has to be a known commit, and an arm that reads its work files from a live checkout can have
# them change underneath it mid-run.
set -u

SCEN_ROOT="${CG_SCEN_ROOT:-$PWD/.cg-scen}"
SRC="${CG_SCEN_SRC:-$PWD}"
UPSTREAM="${CG_SCEN_UPSTREAM:?set CG_SCEN_UPSTREAM to the plain provider gateway, e.g. https://gateway.example.com}"
MODEL="${CG_SCEN_MODEL:-claude-haiku-4-5}"
HOME_SRC="${CG_SCEN_HOME_SRC:-$HOME/.claude}"
GO="${GO:-go}"

mkdir -p "$SCEN_ROOT"

scen_build() {
  # PREPEND, never replace: `claude` lives in ~/.local/bin, and overwriting PATH here made
  # every turn exit 127 while the run still looked like it had merely produced no rows.
  # PREPEND, never replace: `claude` lives in ~/.local/bin on some hosts, and
  # overwriting PATH here made every turn exit 127 while the run still looked like it
  # had merely produced no rows. A rig that fails silently certifies nothing.
  PATH="$(dirname "$(command -v "$GO" || echo /usr/local/go/bin/go)"):$HOME/.local/bin:$PATH"
  export PATH
  cd "$SRC" || exit 1
  CGO_ENABLED=0 "$GO" build -o "$SCEN_ROOT/cg-scen" ./cmd/context-guru-proxy || exit 1
  echo "built $(ls -la "$SCEN_ROOT/cg-scen" | awk '{print $5}') bytes"
}

# scen_start <name> <port> <frac> [cache_state]
#
# cache_state is OPTIONAL and omitting it is meaningful: the key then does not appear in the config at
# all, so the arm runs whatever summarize's shipped default is. That is the only faithful way to write
# a "shipped defaults" arm, and writing the current default's value by hand is not the same thing — it
# would keep passing unchanged through exactly the kind of default change that already happened here.
#
# `cold` and `pre_expiry_or_cold` are REFUSED by the proxy since the cold-gated states were withdrawn.
# An arm passing either does not produce a wrong measurement, it produces a proxy that will not start
# and a run with no rows — which is the failure shape this directory's own trap list is made of, so
# the value is checked here rather than discovered in an empty summary.
#
# ⚠️ IT IS `exit`, NOT `return`, AND THAT IS THE DIFFERENCE BETWEEN A GUARD AND A COMMENT. No arm sets
# `set -e` — they are all `set -u` only — so a `return 2` here prints two lines to stderr and then
# lets scen_home, scen_work and every scen_turn run against a directory with no config.yaml and no
# proxy. That is precisely the silently-empty run this guard exists to prevent, reached through a
# different door, and the only caller who can ever trip it is the person the message is written for.
scen_start() {
  local name=$1 port=$2 frac=$3 state=${4:-}
  case "$state" in
    cold|pre_expiry_or_cold)
      echo "scen_start: cache_state '$state' was withdrawn; the proxy refuses it and this run would" >&2
      echo "  produce no rows. Use '' (shipped default) or 'pre_expiry'." >&2
      exit 2
      ;;
  esac
  local d="$SCEN_ROOT/$name"
  mkdir -p "$d"
  local stateline=""
  [ -n "$state" ] && stateline="      cache_state: $state"
  cat > "$d/config.yaml" <<YAML
pipeline: [summarize]
components:
  summarize:
    keep_last: 3
    min_tokens: 500
    resummarize_tokens: 200000
    trigger:
      min_request_frac: $frac
$stateline
      pre_expiry_seconds: 60
    summary_wait_seconds: 120
YAML
  [ -f "$d/proxy.pid" ] && kill "$(cat "$d/proxy.pid")" 2>/dev/null
  sleep 1
  rm -f "$d/dash.db" "$d/proxy.log"
  cd "$d" || exit 1
  nohup "$SCEN_ROOT/cg-scen" \
    --listen "127.0.0.1:$port" \
    --config "$d/config.yaml" \
    --dashboard --dashboard-db "$d/dash.db" \
    --anthropic-upstream "$UPSTREAM" \
    > "$d/proxy.log" 2>&1 &
  echo $! > "$d/proxy.pid"
  sleep 3
  curl -sS -o /dev/null -w "  healthz=%{http_code}\n" "http://127.0.0.1:$port/healthz"
}

# scen_home <name> <port> — a private Claude config dir pointed at THIS scenario's proxy.
scen_home() {
  local name=$1 port=$2
  local h="$SCEN_ROOT/$name/claude-home"
  rm -rf "$h"
  cp -r "$HOME_SRC" "$h"
  # Drop history so --continue cannot resume a conversation recorded against another endpoint.
  rm -rf "$h/projects" "$h/sessions" "$h/history.jsonl" "$h/session-env" "$h/todos"
  SCEN_PORT="$port" SCEN_HOME="$h" SCEN_MODEL="$MODEL" python3 - <<'PY'
import json, os
p = os.environ["SCEN_HOME"] + "/settings.json"
d = json.load(open(p))
e = d.setdefault("env", {})
e["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:%s/anthropic" % os.environ["SCEN_PORT"]
e.pop("ANTHROPIC_CUSTOM_HEADERS", None)   # no Guru token: production Guru is not in this path
m = os.environ["SCEN_MODEL"]
for k in ("ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL",
          "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
          "CLAUDE_CODE_SUBAGENT_MODEL"):
    e[k] = m
e["MAX_THINKING_TOKENS"] = "0"
d["model"] = m
d.pop("enabledPlugins", None)
d.pop("extraKnownMarketplaces", None)
json.dump(d, open(p, "w"), indent=2)
print("  endpoint:", e["ANTHROPIC_BASE_URL"], "| guru header present:", "ANTHROPIC_CUSTOM_HEADERS" in e)
PY
}

# scen_work <name> — a work dir with big-but-real files to read, so the transcript grows on content
# rather than on synthetic filler.
scen_work() {
  local w="$SCEN_ROOT/$1/work"
  mkdir -p "$w"
  cp "$SRC/components/offload/summarize.go"  "$w/a.go"
  cp "$SRC/components/offload/extract_llm.go" "$w/b.go"
  cp "$SRC/dash/kvcache.go"                   "$w/c.go"
  cp "$SRC/dash/compactepisode.go"            "$w/d.go"
  cp "$SRC/proxy/proxy.go"                    "$w/e.go"
  cp "$SRC/apply/apply.go"                    "$w/f.go"
  cp "$SRC/dash/event.go"                     "$w/g.go"
  cp "$SRC/components/trigger.go"             "$w/h.go"
}

# scen_turn <name> <label> <continue|fresh> <prompt...>
scen_turn() {
  local name=$1 label=$2 mode=$3; shift 3
  local d="$SCEN_ROOT/$name" h="$SCEN_ROOT/$name/claude-home" w="$SCEN_ROOT/$name/work"
  local cont=""
  [ "$mode" = "continue" ] && cont="--continue"
  echo "--- [$name/$label] $(date -u +%H:%M:%S)"
  ( cd "$w" && CLAUDE_CONFIG_DIR="$h" timeout 600 claude -p $cont "$@" \
      --model "$MODEL" --permission-mode bypassPermissions \
      > "$d/$label.out" 2>"$d/$label.err" ) || echo "    (exit $?)"
  scen_tail "$name" 1
}

# scen_tail <name> [n] — the last n request rows, as the provider billed them.
scen_tail() {
  SCEN_DB="$SCEN_ROOT/$1/dash.db" SCEN_N="${2:-40}" python3 - <<'PY'
import os, sqlite3
try:
    c = sqlite3.connect("file:%s?mode=ro" % os.environ["SCEN_DB"], uri=True)
    rows = list(c.execute("""
      SELECT r.id, r.fresh_input+r.cache_read+r.cache_write, r.cache_read, r.cache_write,
             r.cache_miss_reason, r.tokens_before, COALESCE(c.saved_gross,0),
             COALESCE(c.events,''), r.cg_llm_cost_usd, r.cg_latency_ms
      FROM requests r LEFT JOIN request_components c
        ON c.request_id=r.id AND c.component='summarize'
      WHERE r.keepalive=0 ORDER BY r.ts DESC LIMIT %s""" % os.environ["SCEN_N"]))
    for r in reversed(rows):
        ev = r[7].replace('"','').replace('{','').replace('}','')[:44]
        print("    #%-4d billed=%-7d read=%-7d write=%-7d %-13s before=%-7d gross=%-7d cg=%.5f cg_ms=%-7.0f %s"
              % (r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[8], r[9] or 0, ev))
except Exception as e:
    print("    (db:", e, ")")
PY
}

# scen_sleep <seconds> <why>
scen_sleep() {
  echo "--- sleeping ${1}s: $2  ($(date -u +%H:%M:%S))"
  sleep "$1"
}

# scen_panel <name> <port> [span] [fill]
#
# ⚠️ PASS `fill` WHENEVER THE ARM'S TRIGGER IS NOT 0.9, OR THE PANEL SCOPES ITS ANSWER TO A SPAN THE
# ARM NEVER RAN.
#
# The panel derives the span it attributes over as `ceiling - fill` (dash/compactepisode.go, spanFor),
# and it takes `fill` from the QUERY STRING with `defaultFillFrac = 0.90` — it has no way to learn what
# the arm put in its config. So a call that omits `fill` always gets `1.00 - 0.90 = 0.10` of the window,
# which is 20,000 tokens on a 200k model, no matter what min_request_frac the proxy is running.
#
# For an arm at 0.5 that is wrong in the direction that hides turns: the true span is `1.00 - 0.5`, five
# times wider, and the narrow one closes after one or two file-reading turns. Trap 3 in
# timely-compact-validation.md is the write-up of a run that was misread for exactly this reason, and it
# was a mismatch of this kind rather than anything about the design.
#
# ⚠️ `fill` IS NOT ONLY THE SPAN'S INPUT, so `?span=` does not make it optional. It also decides which
# conversations the COVERAGE half counts (`maxBilled(conv) >= fillFrac*window`, compactepisode.go) and
# it is republished verbatim in the panel's own Assumptions block. So an explicit-span call that omits
# it still answers with coverage counted against a threshold the arm never ran, and states a fill the
# run did not use — on the call whose output a reader is told to trust. Pass both.
#
# (It does NOT gate episode creation: conversationEpisodes takes only the span, so the episode list
# itself survives a wrong fill. Coverage and Assumptions are what go wrong, which is quieter.)
scen_panel() {
  local name=$1 port=$2 span=${3:-} fill=${4:-}
  local q="tenant=all&range=all"
  [ -n "$span" ] && q="$q&span=$span"
  [ -n "$fill" ] && q="$q&fill=$fill"
  echo "--- panel ($name${span:+, span=$span}${fill:+, fill=$fill})"
  curl -sS "http://127.0.0.1:$port/api/components/compaction-episodes?$q" > "$SCEN_ROOT/$name/panel${span:+-$span}.json" \
    || echo "    (panel fetch failed)"
  wc -c < "$SCEN_ROOT/$name/panel${span:+-$span}.json"
}
