#!/usr/bin/env bash
# Start a SINGLE-TENANT context-guru-proxy in front of the IBM LiteLLM gateway, forcing
# the agent model. This is the local contributor path: the proxy holds the upstream key
# and the agent sends a placeholder. For the hosted (multi-tenant) proxy, see
# docs/hosted.md — there the key travels with the caller and this script does not apply.
#
# Env knobs: CG_PRESET (default off), CG_PIPELINE (comma list, wins over preset),
# CG_CONFIG_YAML (full yaml, wins over both), CG_MODEL (default aws/claude-sonnet-5),
# CG_DUMP (dump file), CG_PORT (default 4000),
# CG_GATEWAY_BASE / CG_GATEWAY_KEY (the upstream; otherwise read from ~/.claude/settings.json).
set -euo pipefail
export PATH=$PATH:/usr/local/go/bin
CG=/home/vpcuser/projects/context-engineering/context-guru
G=${CG_GATEWAY_BASE:-}; T=${CG_GATEWAY_KEY:-}
if [ -z "$G" ] || [ -z "$T" ]; then
  creds=$(python3 -c 'import json,os;e=json.load(open(os.path.expanduser("~/.claude/settings.json")))["env"];print(e["ANTHROPIC_BASE_URL"]+"|"+e["ANTHROPIC_AUTH_TOKEN"])')
  G=${G:-${creds%%|*}}; T=${T:-${creds#*|}}
fi
# Refuse to forward to another context-guru. Once your own Claude Code routes through the
# service, settings.json names the service rather than the gateway, and this proxy would
# forward to it — a loop whose only symptoms are doubled latency and savings measured
# over traffic that was already compacted once.
case "$T" in cg_live_*)
  echo "the gateway key is a context-guru token; set CG_GATEWAY_KEY to your provider key" >&2; exit 1 ;;
esac
case "${G%/}" in */anthropic|*/openai/v1|*/openai)
  echo "the gateway base URL ($G) is a context-guru route; set CG_GATEWAY_BASE to the real gateway" >&2; exit 1 ;;
esac
PORT=${CG_PORT:-4000}; MODEL=${CG_MODEL:-aws/claude-sonnet-5}
export ANTHROPIC_UPSTREAM="$G" ANTHROPIC_API_KEY="$T" OPENAI_UPSTREAM="$G" OPENAI_API_KEY="$T" \
       FORCE_MODEL="$MODEL" LISTEN_ADDR=":$PORT" CONTEXT_GURU_DEBUG=1
[ -n "${CG_DUMP:-}" ] && export CONTEXT_GURU_DUMP="$CG_DUMP"
cd "$CG"
if [ -n "${CG_CONFIG_YAML:-}" ]; then printf '%s' "$CG_CONFIG_YAML" > /tmp/cg-run.yaml; exec "${BIN:-./bin/context-guru-proxy}" --config /tmp/cg-run.yaml
elif [ -n "${CG_PIPELINE:-}" ]; then printf 'pipeline: [%s]\n' "$CG_PIPELINE" > /tmp/cg-run.yaml; exec "${BIN:-./bin/context-guru-proxy}" --config /tmp/cg-run.yaml
else exec "${BIN:-./bin/context-guru-proxy}" --preset "${CG_PRESET:-off}"; fi
