#!/usr/bin/env bash
# Mutation round: break the PR in five known ways, one at a time, on the smoke tier, and confirm
# each break turns its check red versus an unmutated baseline. A break that stays green is a
# harness gap, reported -- never ignored. The PR's files are always restored from git.
set -uo pipefail
# (.from/.to are read with trailing newlines stripped: the repo's end-of-file hook appends one.)
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
PATHS="dash context-guru-plugin"
[ -z "$(git status --porcelain -- $PATHS)" ] || { echo "working tree has changes under dash/ or the plugin; refusing"; exit 2; }
MFILE=""
restore() { [ -n "$MFILE" ] && git checkout -- "$MFILE"; MFILE=""; }
trap 'restore' EXIT
trap 'restore; exit 130' INT TERM
OUT="$ROOT/e2e/results/$(date -u +%Y%m%dT%H%M%SZ)-mutation"; mkdir -p "$OUT"

runsmoke() { # $1=label ; sets RUN
  e2e/run.sh smoke >"$OUT/$1.log" 2>&1
  RUN="$(sed -n 's/^results: //p' "$OUT/$1.log" | tail -1)"
}
reds() { python3 -c "import json,sys;v=json.load(open(sys.argv[1]+'/verdicts.json'));print(','.join(r['id'] for r in v if r['verdict']=='FAIL'))" "$1" 2>/dev/null; }
crashed() { grep -c '"crashed"' "$1/phases.jsonl" 2>/dev/null || true; }

runsmoke baseline
[ -n "$RUN" ] && [ -f "$RUN/verdicts.json" ] || { echo "baseline produced no verdicts.json; see $OUT/baseline.log"; exit 3; }
BASE_RUN="$RUN"; BASE_RED="$(reds "$RUN")"; BASE_CRASH="$(crashed "$RUN")"; BASE_CRASH="${BASE_CRASH:-0}"
echo "baseline: $BASE_RUN red=[$BASE_RED] crashed=$BASE_CRASH"

: >"$OUT/mutation.json.items"; missed=0
for f in e2e/mutations/*.file; do
  id="$(basename "$f" .file)"; file="$(cat "$f")"; expect="$(cat "e2e/mutations/$id.expect")"
  n="$(python3 -c "import sys;print(open(sys.argv[1]).read().count(open(sys.argv[2]).read().rstrip(chr(10))))" "$file" "e2e/mutations/$id.from")"
  [ "$n" = 1 ] || { echo "$id: anchor occurs $n times in $file; fix the mutation file"; exit 2; }
  MFILE="$file"
  python3 -c "import sys;p,a,b=sys.argv[1:];s=open(p).read();open(p,'w').write(s.replace(open(a).read().rstrip(chr(10)),open(b).read().rstrip(chr(10))))" \
    "$file" "e2e/mutations/$id.from" "e2e/mutations/$id.to"
  git diff --quiet -- "$file" && { echo "$id: mutation changed nothing"; exit 2; }
  runsmoke "$id"
  restore                                          # from git, BEFORE judging
  red="$(reds "$RUN")"; cr="$(crashed "$RUN")"; cr="${cr:-0}"
  python3 - "$id" "$file" "$expect" "$RUN" "$BASE_RUN" "$red" "$BASE_RED" "$cr" "$BASE_CRASH" >>"$OUT/mutation.json.items" <<'PY'
import json,sys
i,f,e,run,base,red,bred,cr,bcr=sys.argv[1:]
red=[x for x in red.split(',') if x]; bred=[x for x in bred.split(',') if x]
hit=lambda ids:[x for x in ids if (x.startswith(e) if e.endswith('.') else x==e)]
base_ok=not hit(bred)
caught=base_ok and (bool(hit(red)) or (int(cr)>0 and int(bcr)==0))
print(json.dumps({"id":i,"file":f,"expect":e,"caught":caught,"crashed_phase":int(cr)>0,
  "expect_failed_in_baseline":not base_ok,"expect_matched":hit(red),
  "turned_red":[x for x in red if x not in bred],"run":run}))
PY
  tail -1 "$OUT/mutation.json.items"
done
python3 - "$OUT" "$BASE_RUN" <<'PY'
import json,sys
o,b=sys.argv[1:]
items=[json.loads(l) for l in open(o+'/mutation.json.items') if l.strip().startswith('{')]
json.dump({"baseline":b,"mutations":items},open(o+'/mutation.json','w'),indent=1)
print("missed:",sum(not m["caught"] for m in items))
PY
rm -f "$OUT/mutation.json.items"
if [ -n "$(git status --porcelain -- $PATHS)" ]; then echo "RESTORE FAILED"; git diff -- $PATHS; exit 4; fi
echo "results: $OUT"
python3 -c "import json,sys;sys.exit(sum(not m['caught'] for m in json.load(open(sys.argv[1]))['mutations']))" "$OUT/mutation.json"
