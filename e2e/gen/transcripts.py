# e2e/gen/transcripts.py
"""Manifest -> Claude Code transcript files, in the real entry shapes.

Each session file interleaves the entry types a real one has (user, assistant, attachment, and the
small bookkeeping types) so the importer meets realistic noise, and the session at TRUNCATED_SESSION_INDEX ends
in a half-written assistant line, as a live session's often does."""
from __future__ import annotations

import argparse
import datetime as dt
import json
import pathlib
import sys

from e2e.gen import manifest as mf


def _iso(ms: int) -> str:
    return dt.datetime.fromtimestamp(ms / 1000, dt.timezone.utc).isoformat().replace("+00:00", "Z")


def _blocks(t: mf.Turn, caps: dict) -> list:
    out = []
    for i, inv in enumerate(t.invocations):
        c = caps[inv.cap]
        if c.kind in ("plugin_skill", "marketplace_skill", "bundled_skill"):
            out.append({"type": "tool_use", "id": f"tu{i}", "name": "Skill", "input": {"skill": c.name}})
        else:
            out.append({"type": "tool_use", "id": f"tu{i}", "name": c.name, "input": {}})
    return out


def _lines(s: mf.Session, caps: dict, m: mf.Manifest, stamps) -> list:
    out = [json.dumps({"type": "ai-title", "sessionId": s.id, "aiTitle": "work"})]
    for t in s.turns:
        ts = t.ts
        if stamps is not None and mf.measured(t, m):
            # The proxy stamped this request's row with its real arrival time; the transcript of the
            # same request carries that same time.
            key = f"{s.id}@{t.ts}"
            if key not in stamps:
                raise KeyError(f"post-install turn {key} has no stamp: replay did not send it")
            ts = stamps[key]
        out.append(json.dumps({"type": "user", "sessionId": s.id, "cwd": t.cwd, "timestamp": _iso(ts - 1000),
                               "isSidechain": False, "entrypoint": "cli", "gitBranch": "main",
                               "message": {"role": "user", "content": "continue"}}))
        out.append(json.dumps({
            "type": "assistant", "sessionId": s.id, "cwd": t.cwd, "timestamp": _iso(ts),
            "isSidechain": False, "entrypoint": "cli", "gitBranch": "main",
            "message": {"model": mf.MODEL, "role": "assistant", "content": _blocks(t, caps),
                        "usage": {"input_tokens": t.fresh, "cache_read_input_tokens": t.cache_read,
                                  "cache_creation_input_tokens": t.cache_write, "output_tokens": t.output}}}))
    out.append(json.dumps({"type": "last-prompt", "sessionId": s.id, "lastPrompt": "continue", "leafUuid": "x"}))
    return out


def write(m: mf.Manifest, home: str, stamps: dict | None = None) -> dict:
    caps = {c.name: c for c in m.capabilities}
    projs = {p.key: p for p in m.projects}
    base = pathlib.Path(home) / ".claude" / "projects"
    turns = 0
    truncated = m.sessions[mf.TRUNCATED_SESSION_INDEX]
    for s in m.sessions:
        d = base / projs[s.project].transcript_dir
        d.mkdir(parents=True, exist_ok=True)
        lines = _lines(s, caps, m, stamps)
        if s is truncated:
            # Killed mid-write: the final assistant line is cut to its first half and ends the file.
            lines = lines[:-1]  # no last-prompt bookkeeping line
            body = "\n".join(lines[:-1]) + "\n" + lines[-1][: len(lines[-1]) // 2]
        else:
            body = "\n".join(lines) + "\n"
        (d / f"{s.id}.jsonl").write_text(body)
        turns += len(s.turns)
    for p in m.projects:  # folders that exist on disk; deleted-proj stays absent
        if p.exists and p.root.startswith(home):
            for f in p.folders:
                if f.startswith(home):
                    pathlib.Path(f).mkdir(parents=True, exist_ok=True)
    return {"files": len(m.sessions), "turns": turns, "truncated_lines": 1}


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tier", required=True)
    ap.add_argument("--home", required=True)
    ap.add_argument("--stamps", default="", help="replay's send-time map; post-install turns take these times")
    a = ap.parse_args(argv)
    stamps = json.load(open(a.stamps)) if a.stamps else None
    print(json.dumps(write(mf.build(a.tier, home=a.home), a.home, stamps)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
