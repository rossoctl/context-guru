#!/bin/bash
# usage: startproxy.sh <name> <port> <preset> <db|-> [extra flags...]
# Starts a local proxy -> tap (18970). Writes logs/<name>.log and <name>.pid. db "-" means no dashboard.
D=${TD871_ROOT:-$(cd "$(dirname "$0")" && pwd)}; n=$1; port=$2; preset=$3; db=$4; shift 4
fl=(--listen 127.0.0.1:$port --preset $preset --anthropic-upstream http://127.0.0.1:18970)
if [ "$db" != "-" ]; then fl+=(--dashboard --dashboard-db "$db"); fi
cd $D
env -i HOME="$HOME" PATH="$PATH" CG_LOG_LEVEL=debug "${TD871_PROXY:-$D/bin/proxy}" "${fl[@]}" "$@" >> $D/logs/$n.log 2>&1 &
echo $! > $D/$n.pid
for i in $(seq 1 100); do curl -s -o /dev/null http://127.0.0.1:$port/stats && break; sleep 0.1; done
sleep 0.5
