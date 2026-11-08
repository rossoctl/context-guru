#!/usr/bin/env bash
# part-b-setup.sh <settings.json> <scratch-dir> <port> <upstream-var> [<model>]: make the isolated
# Claude Code setup for the integration check's Part B (RELEASING.md, step 5).
#
# A PERSON runs this, with the `!` prefix in the integrator's session. It copies credentials,
# and an AI agent must not copy a credential itself. The script prints no value, only key names.
#
# <upstream-var> names the variable in the env block that holds the plain model gateway, for
# example ANTHROPIC_UPSTREAM. Which gateway accepts the key differs per maintainer, so it is an
# argument, not a guess. <model> is the session's model (default claude-sonnet-5-5, to keep the
# check cheap).
#
# It writes, under <scratch-dir>:
#   cc/settings.json   the env block of <settings.json>, minus ANTHROPIC_BASE_URL and
#                      ANTHROPIC_CUSTOM_HEADERS, with ANTHROPIC_BASE_URL set to the test proxy's
#                      Anthropic route, http://127.0.0.1:<port>/anthropic. So the session never
#                      reaches a production context-guru service or anyone's own proxy. Also the
#                      model, and apiKeyHelper when the source has one.
#   upstream.url       the gateway for the test proxy's -anthropic-upstream.
#   cc/.claude.json    onboarding done, and trust for <scratch-dir>/work under both its given and
#                      its resolved path (on macOS /tmp is /private/tmp), so `claude -p` does not stop.
#   state/, cache/, work/  the plugin state, the cache and the work directory.
# All files are mode 600. Remove <scratch-dir> after the check; the credential copies go first,
# right after the last run (`rm -f <scratch-dir>/cc/settings.json`).
set -euo pipefail
usage="usage: part-b-setup.sh <settings.json> <scratch-dir> <port> <upstream-var> [<model>]"
src=${1:?$usage}
dir=${2:?$usage}
port=${3:?$usage}
upvar=${4:?$usage}
model=${5:-claude-sonnet-5-5}
[[ $port =~ ^[0-9]+$ ]] || { echo "port must be a number" >&2; exit 2; }
umask 077
mkdir -p "$dir/cc" "$dir/state" "$dir/cache" "$dir/work"
python3 -I - "$src" "$dir" "$port" "$upvar" "$model" <<'PY'
import json, os, sys
src, d, port, upvar, model = sys.argv[1:6]
cfg = json.load(open(src))
env = dict(cfg.get("env", {}))
up = env.get(upvar)
if not up:
    sys.exit("the env block has no %s" % upvar)
for k in ("ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS"):
    env.pop(k, None)
env["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:%s/anthropic" % port
out = {"env": env, "model": model}
if cfg.get("apiKeyHelper"):
    out["apiKeyHelper"] = cfg["apiKeyHelper"]
json.dump(out, open(d + "/cc/settings.json", "w"), indent=1)
open(d + "/upstream.url", "w").write(up.rstrip("/") + "\n")
work = os.path.abspath(d + "/work")
trust = {p: {"hasTrustDialogAccepted": True} for p in {work, os.path.realpath(work)}}
json.dump({"hasCompletedOnboarding": True, "projects": trust}, open(d + "/cc/.claude.json", "w"))
print("wrote %s/cc/settings.json: model %s, env keys %s%s" % (
    d, model, ", ".join(sorted(env)), ", apiKeyHelper" if "apiKeyHelper" in out else ""))
print("wrote %s/upstream.url from %s (value not printed)" % (d, upvar))
PY
