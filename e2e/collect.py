"""Read the dashboard API into actual-api.json, for every root the oracle names plus the pooled view."""
from __future__ import annotations

import argparse
import json
import subprocess
import sys
import urllib.parse
import urllib.request


def _get(base: str, path: str):
    with urllib.request.urlopen(base.rstrip("/") + path, timeout=30) as r:
        return json.loads(r.read())


def collect(base: str, exp: dict, skill_cmd) -> dict:
    roots = sorted(set(exp["cleanup"]["picker"]) | set(exp["stats"]["requests"]))
    q = lambda r: urllib.parse.quote(r, safe="")
    out = {
        "history": _get(base, "/api/history"),
        "facets_measured": _get(base, "/api/facets?measured=1"),
        "facets_all": _get(base, "/api/facets"),
        "insights": {"": _get(base, "/api/insights?timeline=1")},
        "stats": {},
    }
    for r in roots:
        out["insights"][r] = _get(base, f"/api/insights?timeline=1&project={q(r)}")
        out["stats"][r] = _get(base, f"/api/stats?project={q(r)}")
    if skill_cmd:
        p = subprocess.run(skill_cmd, capture_output=True, text=True)
        out["skill"] = {"exit": p.returncode, "stdout": p.stdout, "stderr": p.stderr}
    return out


def main(argv=None) -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", required=True)
    ap.add_argument("--expected", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--skill", default="")
    a = ap.parse_args(argv)
    exp = json.load(open(a.expected))
    got = collect(a.base, exp, [a.skill, "--no-open"] if a.skill else None)
    json.dump(got, open(a.out, "w"), indent=1, sort_keys=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
