# e2e/gen/shape.py
"""Generated transcripts vs the real format: every generated entry type must exist in the reference,
every key a generated entry carries must be one the real format uses, and every line must be compact
JSON as Claude Code writes it (the product filters raw bytes before it parses a line)."""
from __future__ import annotations

import argparse
import json
import pathlib
import sys


def check(home: str, reference: dict) -> list:
    problems = []
    for f in sorted((pathlib.Path(home) / ".claude" / "projects").glob("*/*.jsonl")):
        for n, line in enumerate(f.read_text().splitlines(), 1):
            try:
                e = json.loads(line)
            except ValueError:
                continue  # the deliberately truncated line; the importer must skip it too
            if line != json.dumps(e, separators=(",", ":"), ensure_ascii=False):
                problems.append(f"{f.name}:{n} line is not compact JSON, unlike every real transcript line")
                continue
            t = e.get("type", "?")
            if t not in reference:
                problems.append(f"{f.name}:{n} entry type {t!r} never appears in real transcripts")
                continue
            extra = set(e) - set(reference[t])
            if extra:
                problems.append(f"{f.name}:{n} {t} has keys real transcripts never use: {sorted(extra)}")
            if t == "assistant":
                msg = e.get("message") or {}
                extra = set(msg) - set(reference.get("assistant.message", ()))
                if extra:
                    problems.append(f"{f.name}:{n} assistant.message keys not in real data: {sorted(extra)}")
    return problems


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--home", required=True)
    a = ap.parse_args(argv)
    ref = json.loads((pathlib.Path(__file__).parent / "reference_keys.json").read_text())
    p = check(a.home, ref)
    for line in p[:20]:
        print(line)
    return 1 if p else 0


if __name__ == "__main__":
    sys.exit(main())
