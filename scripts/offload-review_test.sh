#!/usr/bin/env bash
# offload-review_test.sh: tests for scripts/offload-review/lib.sh (the lock and the per-PR fetch)
# and for the --cleanup path of scripts/offload-review.sh. A temp repo, a bare origin and a stub
# gh on PATH: no network.
set -uo pipefail
. "$(dirname "$0")/offload-review/lib.sh"
require_bash4 || exit 1
here=$(cd "$(dirname "$0")" && pwd)
d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
fails=0
ok()  { echo "ok   $1"; }
bad() { echo "FAIL $1"; fails=$((fails + 1)); }
realgit=$(type -P git)
git() { command git -c core.hooksPath=/dev/null -c user.name=t -c user.email=t@t "$@"; }

# A bare origin with main and 2 PR refs; a clone is the "main checkout".
git init -q --bare -b main "$d/origin.git"
git clone -q "$d/origin.git" "$d/main" 2>/dev/null
( cd "$d/main" && echo 1 > f && git add f && git commit -qm one && git push -q origin main
  for n in 1 2; do git checkout -q -b pr$n && echo $n > f$n && git add f$n && git commit -qm pr$n \
    && git push -q origin "pr$n:refs/pull/$n/head" && git checkout -q main; done )

# shellcheck source=offload-review/lib.sh
lockdir=$d/main/.git/offload-review-worktree.lock

# (a) 2 PRs fetch at the same time, 20 times.
fail_new=0
for i in $(seq 20); do
  fetch_pr_refs "$d/main" 1 main 2>"$d/e1" & p1=$!
  fetch_pr_refs "$d/main" 2 main 2>"$d/e2" & p2=$!
  wait $p1 || fail_new=$((fail_new + 1)); wait $p2 || fail_new=$((fail_new + 1))
done
[ "$fail_new" -eq 0 ] && ok "(a) 40 parallel per-PR fetches all succeed" || bad "(a) $fail_new per-PR fetches failed"
for r in pr1/head pr1/base pr2/head pr2/base; do
  git -C "$d/main" rev-parse -q --verify "refs/offload-review/$r" >/dev/null || bad "(a) ref $r missing"
done
# The old command, in parallel. It writes refs/remotes/origin/main, so it can fail on a ref lock.
# The race is timing: it may not show in 20 loops. The result here is information, not a pass or fail.
fail_old=0
for i in $(seq 20); do
  git -C "$d/main" fetch -q origin "refs/pull/1/head" main 2>/dev/null & p1=$!
  git -C "$d/main" fetch -q origin "refs/pull/2/head" main 2>/dev/null & p2=$!
  wait $p1 || fail_old=$((fail_old + 1)); wait $p2 || fail_old=$((fail_old + 1))
done
echo "info the old command failed $fail_old of 40 times here (it may be 0: the race is timing)"
# The retry: a stub git that fails with "cannot lock ref" 2 times, then works.
mkdir "$d/bin"
cat > "$d/bin/git" <<STUB
#!/usr/bin/env bash
n=\$(cat "$d/count" 2>/dev/null || echo 0)
if [ "\$n" -lt 2 ]; then echo \$((n + 1)) > "$d/count"; echo "error: cannot lock ref 'refs/remotes/origin/main'" >&2; exit 1; fi
exec $realgit "\$@"
STUB
chmod +x "$d/bin/git"; rm -f "$d/count"
( PATH=$d/bin:$PATH OFFLOAD_FETCH_RETRY_SLEEP=0 fetch_pr_refs "$d/main" 1 main 2>/dev/null ) \
  && ok "(a) a 'cannot lock ref' error is tried again" || bad "(a) retry did not work"
cat > "$d/bin/git" <<'STUB'
#!/usr/bin/env bash
echo "fatal: some other error" >&2; exit 128
STUB
SECONDS=0
( PATH=$d/bin:$PATH OFFLOAD_FETCH_RETRY_SLEEP=5 fetch_pr_refs "$d/main" 1 main 2>/dev/null ) \
  && bad "(a) another error passed" || { [ "$SECONDS" -lt 4 ] && ok "(a) another error is not tried again" || bad "(a) another error was retried"; }

# (f) a held lock on the shared ref refs/remotes/origin/main must not stop the fetch, and the shared
# ref must not move. Origin main moves first, so a write to the shared ref would show.
( cd "$d/main" && git checkout -q main && echo 2 > f && git commit -qam two && git push -q origin main \
  && git update-ref refs/remotes/origin/main HEAD~1 )
before=$(git -C "$d/main" rev-parse refs/remotes/origin/main)
: > "$d/main/.git/refs/remotes/origin/main.lock"
SECONDS=0
if OFFLOAD_FETCH_RETRY_SLEEP=100 fetch_pr_refs "$d/main" 1 main 2>"$d/err" && [ "$SECONDS" -lt 3 ] \
  && [ "$(git -C "$d/main" rev-parse refs/remotes/origin/main)" = "$before" ] \
  && [ "$(git -C "$d/main" rev-parse refs/offload-review/pr1/base)" = "$(git -C "$d/origin.git" rev-parse main)" ]
then ok "(f) the fetch ignores a held lock on refs/remotes/origin/main, with no retry"
else bad "(f) fetch touched the shared ref: $(cat "$d/err")"; fi
# The same fetch without --refmap= must fail. That shows (f) can catch the bug.
if git -C "$d/main" fetch -q --no-write-fetch-head origin "+refs/pull/1/head:refs/offload-review/pr1/head" \
  "+refs/heads/main:refs/offload-review/pr1/base" 2>"$d/err"
then bad "(f) the fetch without --refmap= passed: the test cannot catch the bug"
else ok "(f) the fetch without --refmap= fails on that lock ($(head -1 "$d/err"))"; fi
rm -f "$d/main/.git/refs/remotes/origin/main.lock"

# (b) a stale lock with a dead pid is taken over.
mkdir "$lockdir"; ( exit 0 ) & dead=$!; wait $dead; echo "$dead" > "$lockdir/pid"
( lock ) 2>"$d/err" && [ "$(cat "$lockdir/pid")" != "$dead" ] && ok "(b) a dead holder's lock is taken over" || bad "(b) stale lock not taken: $(cat "$d/err")"
rm -rf "$lockdir"
# (b2) a lock with no pid file, older than 60 s, is taken over; a young one is waited for.
mkdir "$lockdir"; touch -t 202001010000 "$lockdir"
( lock ) 2>/dev/null && ok "(b) an old lock without a pid is taken over" || bad "(b) old pidless lock not taken"
rm -rf "$lockdir"
mkdir "$lockdir"
( OFFLOAD_LOCK_TRIES=5 lock ) 2>"$d/err" && bad "(b) a young pidless lock was taken" || ok "(b) a young pidless lock is waited for"
rm -rf "$lockdir"

# (c) a live holder is waited for, not stolen. The message names the path and the pid.
sleep 30 & live=$!
mkdir "$lockdir"; echo "$live" > "$lockdir/pid"
( OFFLOAD_LOCK_TRIES=5 lock ) 2>"$d/err" && bad "(c) a live lock was stolen" || {
  [ "$(cat "$lockdir/pid")" = "$live" ] && grep -q "$lockdir" "$d/err" && grep -q "pid $live" "$d/err" \
    && ok "(c) a live holder is waited for; the error names the path and the pid" || bad "(c) wrong error: $(cat "$d/err")"; }
# The holder lets go while a waiter waits: the waiter gets the lock.
( sleep 1; rm -rf "$lockdir" ) & 
( OFFLOAD_LOCK_TRIES=100 lock ) 2>/dev/null && ok "(c) a waiter gets the lock when the holder lets go" || bad "(c) waiter did not get the lock"
kill "$live" 2>/dev/null; wait; rm -rf "$lockdir"

# (e) the EXIT trap removes a lock this process holds, and keeps a lock of another process.
( trap unlock EXIT; lock; exit 0 ); [ ! -d "$lockdir" ] && ok "(e) the exit trap removes the lock this process holds" || bad "(e) lock left behind"
rm -rf "$lockdir"; sleep 30 & other=$!; mkdir "$lockdir"; echo "$other" > "$lockdir/pid"
( trap unlock EXIT; exit 0 ); [ -d "$lockdir" ] && ok "(e) the exit trap keeps another process's lock" || bad "(e) another lock removed"
kill "$other" 2>/dev/null; rm -rf "$lockdir"

# (g) unlock removes only the lock this process holds. A stale holder must not remove a new lock.
mkdir "$lockdir"; sleep 30 & newh=$!; echo "$newh" > "$lockdir/pid"
( unlock ); [ -d "$lockdir" ] && [ "$(cat "$lockdir/pid")" = "$newh" ] \
  && ok "(g) unlock keeps a new holder's lock" || bad "(g) unlock removed another holder's lock"
kill "$newh" 2>/dev/null; rm -rf "$lockdir"
( lock; unlock ); [ ! -d "$lockdir" ] && ok "(g) unlock removes the lock this process holds" || bad "(g) own lock left"
rm -rf "$lockdir"

# (h) a lock with an empty pid file is "not yet written": a young one is waited for, an old one is taken.
mkdir "$lockdir"; : > "$lockdir/pid"
( OFFLOAD_LOCK_TRIES=5 lock ) 2>/dev/null && bad "(h) a young lock with an empty pid was taken" || ok "(h) a young lock with an empty pid is waited for"
touch -t 202001010000 "$lockdir"
( lock ) 2>/dev/null && ok "(h) an old lock with an empty pid is taken" || bad "(h) old empty-pid lock not taken"
rm -rf "$lockdir"
# Same for the put-back path of lock_take_stale: an empty pid there is not dead.
mkdir "$lockdir"; : > "$lockdir/pid"; sleep 30 & other=$!
# Simulate a lost put-back: a live lock stands at $lockdir, the renamed one has an empty pid.
mv "$lockdir" "$lockdir.stale.test"; mkdir "$lockdir"; echo "$other" > "$lockdir/pid"
lock_take_stale "999999" 2>/dev/null
[ -d "$lockdir.stale.test" ] && ok "(h) a renamed lock with an empty pid is kept while young" || bad "(h) young empty-pid lock removed"
kill "$other" 2>/dev/null; rm -rf "$lockdir" "$lockdir".stale.*

# (i) bash older than 4 is refused with a clear message.
( require_bash4 3 ) 2>"$d/err" && bad "(i) bash 3 was accepted" || { grep -q "needs bash 4" "$d/err" && grep -q "brew install bash" "$d/err" \
  && ok "(i) bash 3 is refused with a clear message" || bad "(i) refusal message missing: $(cat "$d/err")"; }
require_bash4 4 && ok "(i) bash 4 is accepted" || bad "(i) bash 4 refused"
if [ -x /bin/bash ] && [ "$(/bin/bash -c 'echo ${BASH_VERSINFO[0]}')" -lt 4 ]; then
  /bin/bash "$here/offload-review.sh" 1 2>"$d/err" && bad "(i) the script ran on old bash" \
    || { grep -q "needs bash 4" "$d/err" && ok "(i) the script refuses to run on /bin/bash" || bad "(i) script on /bin/bash: $(cat "$d/err")"; }
fi

# (d) --cleanup removes the per-PR refs, with a stub gh that says the PR is closed.
cat > "$d/bin/gh" <<'STUB'
#!/usr/bin/env bash
case "$1 $2" in
  "repo view") echo o/r ;;
  "api user") echo me ;;
  "pr view") echo CLOSED ;;
esac
STUB
chmod +x "$d/bin/gh"; rm -f "$d/bin/git"
fetch_pr_refs "$d/main" 1 main
# The script reads backends from the toplevel of the cwd, so the temp repo gets a copy of scripts/.
mkdir -p "$d/main/scripts" && cp -R "$here/offload-review.sh" "$here/offload-review" "$d/main/scripts/"
mkdir "$d/rules"; : > "$d/rules/offload-review-pr1-rules.md"
( cd "$d/main" && OFFLOAD_RULES_DIR=$d/rules PATH=$d/bin:$PATH "$d/main/scripts/offload-review.sh" 1 --cleanup >/dev/null 2>"$d/err" ) \
  || bad "(d) --cleanup failed: $(cat "$d/err")"
[ ! -e "$d/rules/offload-review-pr1-rules.md" ] && ok "(d) --cleanup removes the scratch rules file" || bad "(d) rules file left"
if git -C "$d/main" rev-parse -q --verify refs/offload-review/pr1/head >/dev/null \
  || git -C "$d/main" rev-parse -q --verify refs/offload-review/pr1/base >/dev/null; then bad "(d) per-PR refs left"
else ok "(d) --cleanup removes the per-PR refs"; fi
git -C "$d/main" rev-parse -q --verify refs/offload-review/pr2/head >/dev/null && ok "(d) --cleanup keeps another PR's refs" || bad "(d) pr2 ref removed"

[ "$fails" -eq 0 ] && echo "all passed" || { echo "$fails failed"; exit 1; }
