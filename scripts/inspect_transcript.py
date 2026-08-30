#!/usr/bin/env python3
"""Structural probe: does a Claude Code transcript carry the request prefix?

Answers one question for the keepalive analysis — can a plugin reconstruct the
byte-exact `tools` + `system` + `messages` a cache-read ping must reproduce?
Prints only structure (keys, record types, counts), never content.
"""
import json
import sys
from collections import Counter

path = sys.argv[1]
keys = set()
types = Counter()
roles = Counter()
sys_hits = set()
tool_hits = set()

for line in open(path):
    line = line.strip()
    if not line:
        continue
    try:
        d = json.loads(line)
    except Exception:
        continue
    if not isinstance(d, dict):
        continue
    keys |= set(d.keys())
    types[d.get("type")] += 1
    msg = d.get("message")
    if isinstance(msg, dict):
        roles[msg.get("role")] += 1
        for k in msg:
            if "system" in k.lower():
                sys_hits.add("message." + k)
            if "tool" in k.lower():
                tool_hits.add("message." + k)
    for k in d:
        if "system" in k.lower():
            sys_hits.add(k)
        if "tool" in k.lower():
            tool_hits.add(k)

print("record types:      ", dict(types))
print("message roles:     ", dict(roles))
print("top-level keys:    ", sorted(keys))
print("system-ish keys:   ", sorted(sys_hits) or "NONE")
print("tool-schema-ish:   ", sorted(tool_hits) or "NONE")
