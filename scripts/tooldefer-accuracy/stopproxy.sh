#!/bin/bash
D=${TD871_ROOT:-$(cd "$(dirname "$0")" && pwd)}; f=$D/$1.pid; [ -f "$f" ] || exit 0
p=$(cat $f); [ -n "$p" ] && kill $p 2>/dev/null; for i in $(seq 1 50); do kill -0 $p 2>/dev/null || break; sleep 0.1; done; rm -f $f
