# Backend: IBM Bob Shell. Sourced by scripts/offload-review.sh.
#
# A backend defines three functions. Each one runs inside the reviewer's worktree.
#   backend_label            one line for the identity table
#   backend_has_session      exit 0 when this worktree has an earlier session to resume
#   backend_run <prompt> <resume:0|1>   run one review; stdout and stderr go to the log
#
# Bob keys its sessions by project path (sha256 of the real path under ~/.bob/tmp), so each
# reviewer's fixed worktree path keeps its own session list.

backend_label() {
  echo "IBM Bob Shell $(bob --version)"
}

backend_has_session() {
  bob --list-sessions 2>/dev/null | grep -q '^ *1\.'
}

backend_run() {
  local prompt=$1 resume=()
  [ "$2" = 1 ] && resume=(--resume latest)
  # -p, not a positional prompt: Bob 1.0.3 exits 1 on --resume plus a positional prompt.
  bob --accept-license --trust --yolo --max-coins "${OFFLOAD_MAX_COINS:-10}" \
    "${resume[@]}" -p "$(cat "$prompt")"
}
