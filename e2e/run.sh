#!/usr/bin/env bash
# Host entry for the PR #578 review. Builds the sandbox image from this checkout and runs a tier.
set -uo pipefail
# known <full-results-dir> re-runs only the main baseline and the KNOWN relabelling on an existing full run.
# Exit: non-zero on any FAIL or crashed phase. INCOMPLETE (a check BLOCKED outside report.BY_DESIGN_BLOCKED) exits
# 0; it is a judged run that proved less than a PASS, and the report's headline says INCOMPLETE.
TIER="${1:?usage: e2e/run.sh smoke|full|stress [--keep] | known <full-results-dir>}"; KEEP="${2:-}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
# A sleeping host freezes the VM mid-render: every UI render timeout in the 20261020T132417Z full run fell in a
# window where the Mac slept (lid closed, then maintenance-sleep cycles). Hold off idle sleep for this script's
# life; closing the lid on battery still sleeps, so keep it open or on power for a full run.
command -v caffeinate >/dev/null && caffeinate -ims -w $$ &
KNOWN_FILE="$ROOT/e2e/known-findings.txt"
IMG_OUT="$ROOT/e2e/results/$STAMP-build"
# Podman takes --ignorefile; real Docker reads Dockerfile.e2e.dockerignore automatically.
IGN=""; docker --version 2>/dev/null | grep -qi podman && IGN="--ignorefile $ROOT/e2e/build.ignore"
# Podman maps the bind mount to root inside the container; keep-id makes it writable by `sandbox`.
EXTRA=""; docker --version 2>/dev/null | grep -qi podman && EXTRA="--userns=keep-id"
command -v rsync >/dev/null || { echo "run.sh needs rsync on the host (main baseline copy)"; exit 2; }
if [ "$TIER" != known ]; then
  mkdir -p "$IMG_OUT"
  docker build $IGN -f "$ROOT/e2e/Dockerfile.e2e" -t cg-e2e:pr578 "$ROOT" >"$IMG_OUT/build.log" 2>&1 ||
    { echo "image build failed; see $IMG_OUT/build.log"; tail -20 "$IMG_OUT/build.log"; exit 2; }
fi
# The host port is published only for --keep (to open the kept dashboard), so tiers can run side by side.
RM="--rm"; PUB=""; [ "$KEEP" = --keep ] && { RM=""; PUB="-p 127.0.0.1:18788:8788"; }
rebuild() { (cd "$ROOT" && python3 -c "import sys;from e2e import report;report.build(sys.argv[1])" "$1"); }

run_tier() {  # run_tier <tier> [image] [outdir] -> sets OUTDIR; returns the container's exit code
  local t="$1" img="${2:-cg-e2e:pr578}"
  OUTDIR="${3:-$ROOT/e2e/results/$(date -u +%Y%m%dT%H%M%SZ)-$t}"; mkdir -p "$OUTDIR"
  docker run $RM $EXTRA $PUB -e HOST_EPOCH="$(date +%s)" --name "cg-e2e-$t-$$-$RANDOM" -v "$OUTDIR:/results" "$img" "$t"
}
fails() { python3 -c "import json,sys;print(sum(r['verdict']=='FAIL' for r in json.load(open(sys.argv[1]+'/verdicts.json'))))" "$1"; }
# FAIL ids that are not in the known-findings allow-list (empty output = none).
# Exits non-zero when it cannot judge (no verdicts, bad JSON), and the gate then treats smoke as failed.
unknown_fails() { (cd "$ROOT" && python3 -m e2e.report --unknown-fails "$1" "$KNOWN_FILE"); }

# KNOWN: a failure that also fails on main is not the PR's doing. The worktree is always removed.
MAINWT=/tmp/cg-e2e-main
cleanup_wt() { git -C "$ROOT" worktree remove --force "$MAINWT" 2>/dev/null; rm -rf "$MAINWT"; git -C "$ROOT" worktree prune; }
label_known() {  # label_known <full results dir>
  local FULL="$1"
  [ "$(fails "$FULL")" != 0 ] || return 0
  trap cleanup_wt EXIT
  cleanup_wt
  # A stale baseline from an earlier `known` run must not relabel anything if this build fails.
  MAINOUT="$FULL/on-main"; rm -rf "$MAINOUT"; mkdir -p "$MAINOUT"
  if git -C "$ROOT" worktree add -q --detach "$MAINWT" main &&
     mkdir -p "$MAINWT/e2e" && rsync -a --exclude results --exclude __pycache__ "$ROOT/e2e/" "$MAINWT/e2e/" &&
     docker build $IGN -q -f "$MAINWT/e2e/Dockerfile.e2e" -t cg-e2e:main "$MAINWT" >"$MAINOUT/build.log" 2>&1; then
    # Only the Go suite: main has no History tab, so every dashboard check would crash rather than FAIL there.
    run_tier suite cg-e2e:main "$MAINOUT" >"$MAINOUT/run.log" 2>&1 || true
  else
    echo "main baseline could not be built; no check relabelled KNOWN (see $MAINOUT)"
  fi
  python3 - "$FULL" "$MAINOUT" <<'PY'
import json, sys
full, main = sys.argv[1], sys.argv[2]
v = json.load(open(f"{full}/verdicts.json"))
try:
    onmain = {r["id"]: r["verdict"] for r in json.load(open(f"{main}/verdicts.json"))}
except (FileNotFoundError, ValueError):
    onmain = {}
for r in v:
    if r["verdict"] == "FAIL" and onmain.get(r["id"]) == "FAIL":
        r["verdict"] = "KNOWN"; r["detail"] = (r["detail"] + " also fails on main").strip()
json.dump(v, open(f"{full}/verdicts.json", "w"), indent=1)
PY
  cleanup_wt; trap - EXIT
}

if [ "$TIER" = known ]; then
  FULL="$(cd "${2:?known needs a full results dir}" && pwd)"; label_known "$FULL"; rebuild "$FULL"
  echo "results: $FULL"; exit 0
fi
if [ "$TIER" = stress ]; then run_tier stress; rc=$?; echo "results: $OUTDIR"; exit $rc; fi

run_tier smoke; rc=$?; SMOKE="$OUTDIR"
if [ "$TIER" = smoke ]; then
  [ -f "$SMOKE/verdicts.json" ] && rebuild "$SMOKE"
  echo "results: $SMOKE"; exit $rc
fi

# Gate: only the known PR findings may FAIL, and no phase may have crashed.
UNKNOWN="$(unknown_fails "$SMOKE")"; GATE_RC=$?
if [ ! -f "$SMOKE/verdicts.json" ] || grep -q crashed "$SMOKE/phases.jsonl" || [ $GATE_RC -ne 0 ] || [ -n "$UNKNOWN" ]; then
  echo "smoke gate failed; not running the full tier. results: $SMOKE"
  [ -n "$UNKNOWN" ] && echo "$UNKNOWN"; [ -f "$SMOKE/verdicts.json" ] && rebuild "$SMOKE"
  exit 1
fi

run_tier full; FULL="$OUTDIR"
run_tier stress; STRESS="$OUTDIR"
cp "$STRESS/phases.jsonl" "$FULL/stress-phases.jsonl" 2>/dev/null
cp "$STRESS/stress-endpoints.jsonl" "$FULL/" 2>/dev/null

python3 - "$FULL" "$STRESS" <<'PY'
import json, sys
full, stress = sys.argv[1:3]
def jl(p):
    try:
        return [json.loads(l) for l in open(p) if l.strip()]
    except FileNotFoundError:
        return []
sp = {p["phase"]: p["seconds"] for p in jl(f"{stress}/phases.jsonl")}
ep = {e["endpoint"]: round(e["s"] * 1000, 1) for e in jl(f"{stress}/stress-endpoints.jsonl")}  # s -> ms
out = {"phases": jl(f"{full}/phases.jsonl"),
       "stress": {"import_s": sp.get("import"), "replay_s": sp.get("replay"),
                  "history_api_ms": ep.get("/api/history"), "facets_ms": ep.get("/api/facets?measured=1"),
                  "insights_pooled_ms": ep.get("/api/insights?timeline=1")}}
json.dump(out, open(f"{full}/timings.json", "w"), indent=1)
PY

label_known "$FULL"
rebuild "$FULL"
echo "results: $FULL"; echo "report:  $FULL/report.html"
[ "$(fails "$FULL")" = 0 ]
