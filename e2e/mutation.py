"""mutate.sh's judge: was a mutation caught, and did the round judge every mutation."""
from __future__ import annotations

import json
import pathlib
import sys


def judge(i, f, e, run, base, red, bred, cr, bcr) -> dict:
    """Caught only if the expected check (or, for an id ending in ".", any check under it) turned red against a
    baseline where it was green, with no crashed phase: a crash shows the break did something, not that the
    check aimed at it can see it."""
    red = [x for x in red.split(",") if x]; bred = [x for x in bred.split(",") if x]
    hit = lambda ids: [x for x in ids if (x.startswith(e) if e.endswith(".") else x == e)]
    base_ok, crashed = not hit(bred), int(cr) > 0
    return {"id": i, "file": f, "expect": e, "caught": base_ok and bool(hit(red)) and not crashed,
            "crashed_phase": crashed, "baseline_crashed_phase": int(bcr) > 0,
            "expect_failed_in_baseline": not base_ok, "expect_matched": hit(red),
            "turned_red": [x for x in red if x not in bred], "run": run}


def summarize(out_dir, base, n) -> int:
    """Write mutation.json; non-zero if any mutation was missed or the round judged fewer than n of them."""
    o = pathlib.Path(out_dir)
    items = [json.loads(l) for l in (o / "mutation.json.items").read_text().splitlines() if l.strip().startswith("{")]
    (o / "mutation.json").write_text(json.dumps({"baseline": base, "mutations": items}, indent=1))
    if len(items) != int(n):
        print(f"judged {len(items)} of {n} mutations")
    missed = sum(not m["caught"] for m in items)
    print("missed:", missed)
    return missed if len(items) == int(n) else max(missed, 1)


if __name__ == "__main__":
    # judge <id> <file> <expect> <run> <base run> <red> <base red> <crashed> <base crashed>  -> one JSON line
    # summarize <out dir> <base run> <number of mutations>                                  -> exit code
    if sys.argv[1] == "judge":
        print(json.dumps(judge(*sys.argv[2:11])))
    else:
        sys.exit(summarize(*sys.argv[2:5]))
