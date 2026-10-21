# Wall-clock bounds for e2e phases. Sourced by phases.sh (and by tests/test_timeout.py).
# Elapsed time is counted in sleep ticks, not read from the clock: the VM's clock was once stepped 18h forward
# mid-run (chronyd), and a clock-based budget would have fired on that step instead of on a real hang.

phase_budget() {  # phase_budget <tier> <phase> -> seconds; E2E_TIMEOUT_<phase>=N overrides (to force a timeout)
  local o="E2E_TIMEOUT_$2"; [ -n "${!o:-}" ] && { echo "${!o}"; return; }
  case "$1" in smoke) echo 600 ;; full) echo 1800 ;; *) echo 3600 ;; esac
}

_descendants() { local c; for c in $(pgrep -P "$1"); do _descendants "$c"; echo "$c"; done; }

kill_tree() {  # kill_tree <pid>: TERM the pid and every descendant, KILL what survives 3s
  local pids; pids="$(_descendants "$1") $1"
  kill -TERM $pids 2>/dev/null
  for _ in 1 2 3; do kill -0 $pids 2>/dev/null || return 0; sleep 1; done
  kill -KILL $pids 2>/dev/null; return 0
}

# run_bounded <seconds> <function> <log> <capture command>: run the function with output to the log; if it is
# still running after the budget, eval the capture command (evidence, while everything is still alive), then
# kill the whole tree and return 124. Otherwise return the function's own exit code.
run_bounded() {
  local budget="$1" fn="$2" log="$3" capture="$4" pid ticks=0
  "$fn" >"$log" 2>&1 &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$ticks" -ge "$budget" ]; then
      eval "$capture"
      kill_tree "$pid"; wait "$pid" 2>/dev/null
      return 124
    fi
    sleep 1; ticks=$((ticks + 1))
  done
  wait "$pid"
}

# check_clock <host epoch> <max seconds>: fail (return 1) when this clock is more than max seconds off the host's.
# A VM clock hours behind gets stepped by chronyd mid-run, which corrupts every timestamp after the step.
# An empty host epoch (image run by hand, not via run.sh) is not judged.
check_clock() {
  [ -n "$1" ] || return 0
  local skew=$(( $(date +%s) - $1 ))
  [ "${skew#-}" -le "$2" ] && return 0
  echo "clock skew ${skew}s"; return 1
}
