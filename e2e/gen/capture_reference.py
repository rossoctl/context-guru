# e2e/gen/capture_reference.py
"""Run ONCE on the host. Records which entry types real transcripts contain and the top-level keys
of each, so generated data can be checked against the real format. Keys only: no values, no content."""
import collections, glob, json, os, sys

types = collections.defaultdict(set)
for f in glob.glob(os.path.expanduser("~/.claude/projects/*/*.jsonl")):
    for line in open(f, errors="replace"):
        try:
            e = json.loads(line)
        except ValueError:
            continue
        types[e.get("type", "?")] |= set(e.keys())
        if e.get("type") == "assistant":
            types["assistant.message"] |= set((e.get("message") or {}).keys())
            types["assistant.message.usage"] |= set(((e.get("message") or {}).get("usage") or {}).keys())
json.dump({t: sorted(k) for t, k in sorted(types.items())}, sys.stdout, indent=1)
