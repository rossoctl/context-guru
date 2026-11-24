#!/usr/bin/env bash
# local-tests_test.sh: fixture tests for local-tests.sh (--dry-run --reuse) and for the API-error
# contract of check-ready.sh. Run it in a git checkout. It needs no network and posts nothing.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(git -C "$here" rev-parse --show-toplevel)
cd "$root"
H=$(git rev-parse HEAD)
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
fails=0
steps=$(grep -oE '^step [a-z0-9-]+' scripts/release/part-a.sh | awk '{print $2}')
mk() { { echo "commit $1"; for s in $steps; do echo "ok      $s"; done; } > "$d/summary.txt"; }
# expect <name> <want-rc> <want-text> : run local-tests.sh on the fixture in $d
expect() {
  local out rc
  out=$(scripts/dev/local-tests.sh --dry-run --reuse "$d" 1 2>&1); rc=$?
  if [ "$rc" = "$2" ] && grep -qF -- "$3" <<<"$out"; then echo "ok   $1"; else echo "FAIL $1 (rc=$rc, want rc=$2 and '$3')"; fails=1; fi
}
mk "$H"; echo "DONE rc=0" >> "$d/summary.txt"; expect pass 0 "result=pass"
mk "$H"; sed -i.bak 's/^ok      race-dash/flaky   race-dash (x)/' "$d/summary.txt"; echo "DONE rc=0" >> "$d/summary.txt"; expect flaky 0 "flaky: passed"
mk "$H"; sed -i.bak 's/^ok      race-rest/FAILED  race-rest (twice)/' "$d/summary.txt"; echo "DONE rc=1" >> "$d/summary.txt"
printf -- '--- FAIL: TestNoHooksKeepsTheBinaryDefaults (0s)\nFAIL\n' > "$d/race-rest.log"; expect known-failure 0 "result=pass"
mk "$H"; sed -i.bak 's/^ok      race-proxy/FAILED  race-proxy (twice)/' "$d/summary.txt"; echo "DONE rc=1" >> "$d/summary.txt"
printf -- '--- FAIL: TestReal (0s)\nFAIL\n' > "$d/race-proxy.log"; expect real-failure 1 "result=fail"
printf 'commit %s\nok      vet\nok      build-purego\nok      build-record\nDONE rc=143\n' "$H" > "$d/summary.txt"; expect stopped-rc143 1 "result=fail"
mk deadbeef; echo "DONE rc=0" >> "$d/summary.txt"; expect sha-mismatch 2 "Nothing posted"

# check-ready.sh: a failing gh must exit 2, not print NOT READY.
mkdir "$d/bin"; printf '#!/bin/sh\nexit 1\n' > "$d/bin/gh"; chmod +x "$d/bin/gh"
out=$(PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
if [ "$rc" = 2 ] && ! grep -q "NOT READY" <<<"$out"; then echo "ok   check-ready-gh-error"; else echo "FAIL check-ready-gh-error (rc=$rc)"; fails=1; fi
# A gh that works for every call except the comments must exit 2, and must not say "no local-tests
# comment". Every other call succeeds, so a later failure cannot hide a swallowed comments error.
cat > "$d/bin/gh" <<'GH'
#!/bin/sh
case "$*" in
  "pr view"*) echo '{"state":"OPEN","isDraft":false,"mergeable":"MERGEABLE","headRefOid":"abc123","title":"t"}' ;;
  *issues/*/comments*) exit 1 ;;
  *reviews*) echo '<!-- offload-review reviewer=1 round=1 sha=abc123 backend=x verdict=approve -->'; echo '<!-- offload-review reviewer=2 round=1 sha=abc123 backend=x verdict=approve -->' ;;
  *check-runs*) printf 'completed\tsuccess\tpurego\tgithub-actions\n' ;;
  *pulls/*/commits*) exit 0 ;;
  *) exit 1 ;;
esac
GH
out=$(PATH="$d/bin:$PATH" scripts/release/check-ready.sh 1 2>&1); rc=$?
if [ "$rc" = 2 ] && grep -q "could not read the comments" <<<"$out" && ! grep -q "no local-tests comment" <<<"$out"; then echo "ok   check-ready-comments-error"; else echo "FAIL check-ready-comments-error (rc=$rc)"; echo "$out" | tail -3; fails=1; fi
exit "$fails"
