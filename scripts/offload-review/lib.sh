# Helpers for scripts/offload-review.sh. Source this file. It has its own file so a test can use it.
# The caller sets $lockdir before it calls lock. It needs bash 4 or newer (see require_bash4).

# require_bash4 [major]: stop with a clear message when bash is older than 4. Bash 3.2 (macOS
# /bin/bash) has no BASHPID, so a subshell there reports the pid of the script that started it.
# Then the lock cannot tell a subshell from the script. The optional argument is for a test.
require_bash4() {
  local major=${1:-${BASH_VERSINFO[0]}}
  [ "$major" -ge 4 ] && return 0
  echo "offload-review needs bash 4 or newer. This is bash $major. On macOS, install it with 'brew install bash' and put it first on PATH: the '#!/usr/bin/env bash' line then finds it." >&2
  return 1
}

# self_pid: the pid of the current shell (BASHPID: the pid of a subshell, not of the script).
# Do not call it in $(...): that runs a new subshell, with another BASHPID.
self_pid() { echo "$BASHPID"; }

# lock_age <dir>: the age of the directory in seconds.
lock_age() {
  echo $(( $(date +%s) - $(stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || date +%s) ))
}

# lock: take the worktree lock, a directory with the holder's pid in it. A lock whose holder is
# dead, or that has no pid file for over 60 s, is stale: lock removes it and tries again. A lock
# with a live holder is waited for, for up to 60 s (OFFLOAD_LOCK_TRIES x 0.1 s).
lock() {
  local n=0 tries=${OFFLOAD_LOCK_TRIES:-600} pid age
  until mkdir "$lockdir" 2>/dev/null; do
    pid=$(cat "$lockdir/pid" 2>/dev/null || true)
    if [ -n "$pid" ]; then
      kill -0 "$pid" 2>/dev/null || { lock_take_stale "$pid"; continue; }
    elif [ -d "$lockdir" ]; then
      age=$(lock_age "$lockdir")
      if [ "$age" -gt 60 ]; then lock_take_stale ""; continue; fi
    fi
    n=$((n + 1))
    if [ "$n" -ge "$tries" ]; then
      echo "The worktree lock $lockdir is held for over $((tries / 10)) s by pid ${pid:-unknown}. Remove it if no review is running." >&2
      return 1
    fi
    sleep 0.1
  done
  self_pid > "$lockdir/pid"
}

# lock_take_stale <pid>: remove the stale lock. It renames the lock first, so only one waiter wins.
# If the renamed lock is not the stale one (another waiter took it over and a new run locked it
# again), it goes back.
lock_take_stale() {
  local seen=$1 gone="$lockdir.stale.$(self_pid).$RANDOM" now
  mv "$lockdir" "$gone" 2>/dev/null || return 0
  now=$(cat "$gone/pid" 2>/dev/null || true)
  if [ "$now" = "$seen" ] || { [ -z "$seen" ] && [ -z "$now" ]; }; then
    echo "offload-review: removed the stale worktree lock ${lockdir} (holder pid ${seen:-unknown})" >&2
    rm -rf -- "$gone"
  else
    # Put it back. If that fails, a new lock exists. Remove the renamed one only when its holder is
    # dead. A live one stays: its ".stale.*" name blocks nobody.
    # An empty pid file means "not yet written", not "dead": that one stays until it is 60 s old.
    mv "$gone" "$lockdir" 2>/dev/null || {
      if [ -n "$now" ]; then kill -0 "$now" 2>/dev/null || rm -rf -- "$gone"
      elif [ "$(lock_age "$gone")" -gt 60 ]; then rm -rf -- "$gone"; fi
    }
  fi
}

# unlock: remove the lock only when this process holds it (the pid file has its pid). A holder
# that lost its lock as stale must not remove the lock of a new holder. It also works as an EXIT trap.
unlock() {
  [ -n "${lockdir:-}" ] || return 0
  if [ "$(cat "$lockdir/pid" 2>/dev/null || true)" = "$BASHPID" ]; then rm -rf -- "$lockdir"; fi
  return 0
}

# fetch_pr_refs <main> <pr> <base_ref>: fetch the PR head and the base branch into refs that only
# this PR's run writes (refs/offload-review/pr<N>/head and /base). --refmap= stops git from also
# updating refs/remotes/origin/<base>, and --no-write-fetch-head stops it writing FETCH_HEAD. So
# runs for different PRs share no ref lock. Other fetches in the repo can still lock a shared ref
# for a moment, so a "cannot lock ref" error is tried again (5 tries, OFFLOAD_FETCH_RETRY_SLEEP
# seconds apart, default 2).
fetch_pr_refs() {
  local main=$1 pr=$2 base_ref=$3 out rc i
  for i in 1 2 3 4 5; do
    rc=0
    out=$(git -C "$main" fetch -q --no-write-fetch-head --refmap= origin \
      "+refs/pull/$pr/head:refs/offload-review/pr$pr/head" \
      "+refs/heads/$base_ref:refs/offload-review/pr$pr/base" 2>&1) || rc=$?
    [ "$rc" -eq 0 ] && return 0
    case $out in *"cannot lock ref"*) [ "$i" -lt 5 ] && { sleep "${OFFLOAD_FETCH_RETRY_SLEEP:-2}"; continue; } ;; esac
    printf '%s\n' "$out" >&2
    return "$rc"
  done
}

# delete_pr_refs <main> <pr>: remove the refs fetch_pr_refs made.
delete_pr_refs() {
  local ref
  for ref in "refs/offload-review/pr$2/head" "refs/offload-review/pr$2/base"; do
    git -C "$1" update-ref -d "$ref" 2>/dev/null || true
  done
}
