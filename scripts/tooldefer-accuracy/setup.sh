#!/bin/bash
# setup.sh: make the files that hold the harness root path, the work dirs and the git template.
# Run it once after you copy the folder. It never calls a model.
D=${TD871_ROOT:-$(cd "$(dirname "$0")" && pwd)}; [ -n "$D" ] || exit 1
cd "$D" || exit 1
mkdir -p bin cap logs runs work cfg snap
chmod +x "$(dirname "$0")"/*.sh 2>/dev/null || true
cat > mcp.json <<J
{"mcpServers":{"probe":{"command":"python3","args":["-I","$D/mcp_probe.py"]},"ops":{"command":"python3","args":["-I","$D/mcp_ops.py"]}}}
J
cat > mcp-noops.json <<J
{"mcpServers":{"probe":{"command":"python3","args":["-I","$D/mcp_probe.py"]}}}
J
cat > settings.json <<J
{"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"TD871_ROOT=$D python3 -I $D/hook.py"}]}]}}
J
if [ ! -d tmpl/.git ]; then
  mkdir -p tmpl && cd tmpl && git init -q -b main \
  && printf '2026-11-03 INFO start\n2026-11-03 INFO ready\n' > server.log \
  && python3 -c "
import json
nb={'cells':[{'cell_type':'code','execution_count':None,'id':'c1','metadata':{},'outputs':[],'source':['print(\'hi\')']},{'cell_type':'markdown','id':'c2','metadata':{},'source':['# Notes']}],'metadata':{},'nbformat':4,'nbformat_minor':5}
json.dump(nb,open('analysis.ipynb','w'),indent=1)" \
  && printf 'host=localhost\nport=80\n' > config.ini \
  && printf 'import time\nfor i in range(3):\n    print("step", i); time.sleep(1)\nprint("DONE")\n' > worker.py \
  && git add -A && git -c user.email=t@t -c user.name=t commit -qm init
fi
echo "setup done in $D"
