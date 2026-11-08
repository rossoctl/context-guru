#!/usr/bin/env bash
# part-b-setup.sh <settings.json> <scratch-dir> <port>: make the isolated Claude Code setup for the
# integration check's Part B (RELEASING.md, step 5).
#
# A PERSON runs this, with the `!` prefix in the integrator's session. It copies credentials,
# and an AI agent must not copy a credential itself. The script prints no value, only key names.
#
# It writes, under <scratch-dir>:
#   cc/settings.json   the env block of <settings.json>, minus ANTHROPIC_BASE_URL and
#                      ANTHROPIC_CUSTOM_HEADERS, with ANTHROPIC_BASE_URL set to the test proxy
#                      on <port>. So the session never reaches a production context-guru service
#                      or anyone's own proxy.
#   upstream.url       the plain model gateway for the test proxy's -anthropic-upstream, read from
#                      ANTHROPIC_BENCHMARK_BASE_URL, else ANTHROPIC_UPSTREAM, in that env block.
#   cc/.claude.json    trust for <scratch-dir>/work, so `claude -p` does not stop at the trust dialog.
#   state/, cache/, work/  the plugin state, the cache and the work directory.
# All files are mode 600. Remove <scratch-dir> after the check; the credential copies go first,
# right after the last run (`rm -f <scratch-dir>/cc/settings.json`).
set -euo pipefail
src=${1:?usage: part-b-setup.sh <settings.json> <scratch-dir> <port>}
dir=${2:?usage: part-b-setup.sh <settings.json> <scratch-dir> <port>}
port=${3:?usage: part-b-setup.sh <settings.json> <scratch-dir> <port>}
[[ $port =~ ^[0-9]+$ ]] || { echo "port must be a number" >&2; exit 2; }
umask 077
mkdir -p "$dir/cc" "$dir/state" "$dir/cache" "$dir/work"
python3 -I - "$src" "$dir" "$port" <<'PY'
import json, sys
src, d, port = sys.argv[1], sys.argv[2], sys.argv[3]
env = dict(json.load(open(src)).get("env", {}))
up = env.get("ANTHROPIC_BENCHMARK_BASE_URL") or env.get("ANTHROPIC_UPSTREAM")
if not up:
    sys.exit("no ANTHROPIC_BENCHMARK_BASE_URL or ANTHROPIC_UPSTREAM in the env block")
for k in ("ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS"):
    env.pop(k, None)
env["ANTHROPIC_BASE_URL"] = "http://127.0.0.1:%s" % port
json.dump({"env": env}, open(d + "/cc/settings.json", "w"), indent=1)
open(d + "/upstream.url", "w").write(up.rstrip("/") + "\n")
json.dump({"projects": {d + "/work": {"hasTrustDialogAccepted": True}}},
          open(d + "/cc/.claude.json", "w"))
print("wrote %s/cc/settings.json with keys: %s" % (d, ", ".join(sorted(env))))
print("wrote %s/upstream.url (the plain gateway; value not printed)" % d)
PY
