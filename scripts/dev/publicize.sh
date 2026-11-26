#!/usr/bin/env bash
# publicize.sh moves the internal/ packages that a host outside this module (Cortex) needs
# to the module root, and rewrites every path that names them (#1308).
#
# Run it from the repo root. It is safe to run again on fresh main: it moves only the
# packages that are still under internal/ and still exist, and every step checks first.
# After a PR merges that touches one of these packages, drop the conflicting changes and run
# this script again. Do not resolve those conflicts by hand.
#
# It changes import paths and path words in comments and docs. It changes no logic, apart from
# one rename: Miss.Alertable in respfacts becomes alertable when nothing outside the package
# calls it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

# Each package here is named in the exported signature of a public package, or a host must call it.
# The reason for each is in the PR that added this script.
PUBLIC=(adjudicate waitcause providerusage compactionpoint respfacts reqfacts)

for p in "${PUBLIC[@]}"; do
  if [ -d "internal/$p" ] && [ ! -e "$p" ]; then
    git mv "internal/$p" "$p"
  fi
done

# inband/inbandtest holds test helpers only. No host needs it, so it goes the other way.
if [ -d inband/inbandtest ] && [ ! -e internal/inbandtest ]; then
  git mv inband/inbandtest internal/inbandtest
fi

# Rewrite every tracked text file. The names do not overlap, so the rewrite is idempotent.
alt=$(IFS='|'; echo "${PUBLIC[*]}")
# A word boundary is not enough: Perl's \b does not break before "_" (internal/waitcause_test.go).
# git grep -I skips binary files, with git's own check.
{ git grep -I -l -z -E "internal/($alt)|inband/inbandtest" -- . ':!scripts/dev/publicize.sh' ':!go.sum' || true; } \
  | xargs -0 -r perl -pi -e "s{internal/($alt)(?=[^A-Za-z0-9]|\$)}{\$1}g; s{inband/inbandtest(?=[^A-Za-z0-9]|\$)}{internal/inbandtest}g"

# Miss.Alertable has no caller outside its own package.
if [ -d respfacts ] && grep -rqw 'Alertable' respfacts; then
  if ! git grep -w -l 'Alertable' -- '*.go' ':!respfacts' | grep -q .; then
    perl -pi -e 's/\bAlertable\b/alertable/g' respfacts/*.go
  fi
fi

# Sorted import blocks change when a path changes.
git ls-files -z '*.go' | xargs -0 gofmt -l -w >/dev/null
git add -A

# Add the package rows to the table in docs/design.md, once.
add_row() { # name, row text
  grep -q "^| \`$1/\` |" docs/design.md && return 0
  [ -d "$1" ] || return 0
  perl -0pi -e 's{(\n\| `thread/` \|[^\n]*)}{$1\n| `'"$1"'/` | '"$2"' |}' docs/design.md
}
add_row adjudicate 'the in-band tool the cheap model answers through: `ToolName`, the tool definition, and the stray-call answer (`StrayAnswer`, `ResponseCallIDs`). Used by `accwire` and `cheapmodel`'
add_row waitcause 'what a thread waits for after a reply (a person, a tool, a subagent): `Tracker`, `Scan`, `ClassifyWaitingFor`. `keepalive` and `campaign` read it, and a host feeds it'
add_row providerusage 'the billed token tiers of one response (fresh, cache read, cache write by lifetime): `Parse`, `Tiers`. Read by `keepalive` and `cheapmodel`'
add_row compactionpoint 'where a client compacts its own context, by client and model: `Point`, `For`, `Resolve`. Read by `components/offload`'
add_row respfacts 'the facts a response states, from a body or a stream: usage (`ParseUsage`, `ParseSSEUsage`) and the reason a usage reading is missing (`Miss`)'
add_row reqfacts 'the facts a request states: `Meta` (`MetaFromBody`) and `Lifecycle` (`ScanLifecycle`). `dash.Meta` and `dash.LifecycleReq` are aliases of them'

# The rows above are true at any head. Add the names of the packages that now exist.
fix_row() { # package that must exist, old text, new text
  [ -d "$1" ] || return 0
  OLD=$2 NEW=$3 perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/ if /^\| `/' docs/design.md
}
fix_row inband 'Used by `accwire` and `cheapmodel`' 'Used by `accwire`, `cheapmodel` and `inband`'
fix_row respfacts 'Read by `keepalive` and `cheapmodel`' 'Returned by `respfacts`; read by `keepalive` and `cheapmodel`'
fix_row lifecycle 'Read by `components/offload`' 'Read by `components/offload`; `lifecycle.ResolveCompactionPoint` returns it'
git add -A
