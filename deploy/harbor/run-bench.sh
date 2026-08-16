#!/usr/bin/env bash
# Run a harbor benchmark through the context-guru proxy with a clean, explicit env.
# Usage: run-bench.sh <proxy_anthropic_url> <jobs_dir> <harbor args...>
# Forces the agent's ANTHROPIC_BASE_URL to the proxy (overriding any inherited gateway)
# and routes through the docker group.
#
# Credentials come from the environment at run time — the same two deployments cgenv.py
# handles for the Python harnesses:
#
#   CG_TOKEN unset  local single-tenant proxy. The agent sends the `sk-proxy`
#                   placeholder and the proxy injects its own upstream key.
#   CG_TOKEN set    hosted proxy. The agent forwards its OWN provider key
#                   (CG_GATEWAY_KEY) upstream, and the tenant token rides in
#                   x-context-guru-token — a dedicated header, because the auth slot is
#                   now occupied by the provider key and cannot carry both.
#
# Neither value is interpolated into the exec'd command: they are exported and
# referenced as "$CG_AGENT_KEY" / '${CG_AGENT_HEADERS}', so they stay out of argv (which
# any user on the box can read) and out of the run config harbor persists for resume.
set -euo pipefail
export PATH="$PATH:$HOME/.local/bin"
PROXY_URL="$1"; JOBS_DIR="$2"; shift 2
cd /home/vpcuser/projects/context-engineering/harbor

if [ -n "${CG_TOKEN:-}" ]; then
  : "${CG_GATEWAY_KEY:?CG_TOKEN is set, so the agent forwards its own provider key: set CG_GATEWAY_KEY too}"
  case "$CG_GATEWAY_KEY" in
    cg_live_*) echo "CG_GATEWAY_KEY holds a context-guru token, not a provider key" >&2; exit 1 ;;
  esac
  export CG_AGENT_KEY="$CG_GATEWAY_KEY"
  export CG_AGENT_HEADERS="x-context-guru-token: $CG_TOKEN"
  AE_AUTH="--ae ANTHROPIC_API_KEY='\${CG_AGENT_KEY}' --ae ANTHROPIC_AUTH_TOKEN='\${CG_AGENT_KEY}' --ae ANTHROPIC_CUSTOM_HEADERS='\${CG_AGENT_HEADERS}'"
else
  export CG_AGENT_KEY="sk-proxy"
  AE_AUTH="--ae ANTHROPIC_API_KEY='\${CG_AGENT_KEY}' --ae ANTHROPIC_AUTH_TOKEN='\${CG_AGENT_KEY}'"
fi

# clean env of any inherited gateway creds, set proxy target explicitly
export ANTHROPIC_BASE_URL="$PROXY_URL"
export ANTHROPIC_API_KEY="$CG_AGENT_KEY"
export ANTHROPIC_AUTH_TOKEN="$CG_AGENT_KEY"
unset CLAUDE_CODE_OAUTH_TOKEN CLAUDE_FORCE_OAUTH 2>/dev/null || true
exec setsid sg docker -c "ANTHROPIC_BASE_URL='$PROXY_URL' ANTHROPIC_API_KEY=\"\$CG_AGENT_KEY\" ANTHROPIC_AUTH_TOKEN=\"\$CG_AGENT_KEY\" PATH='$PATH' uv run harbor run $* --jobs-dir '$JOBS_DIR' --ae ANTHROPIC_BASE_URL='$PROXY_URL' $AE_AUTH"
