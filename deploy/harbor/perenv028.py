#!/usr/bin/env python3
"""Per-environment accuracy for one iteration-028 pass, read off LOCA's own summary block.

WHY THIS IS NOT THE MEAN. Fifteen environments carry the reward signal for a seed, and three of them
(CanvasArrangeExamS2L, CanvasListTestS2L, WoocommerceNewWelcomeS2L) scored a constant zero across all
nineteen iteration-024 passes. A mean over fifteen therefore reports a number whose denominator is
partly noise-free zero, and two agents with the same mean can differ entirely in WHICH environments they
solve. The capability question -- would a cheaper agent model still finish this work -- is answered by
the count of environments that come off zero, so that is what this prints.

It parses the "Per-Group Results" block rather than the per-task lines, because that block is the one
place LOCA states the environment class next to its accuracy.
"""
import os
import re
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import locaterm

# Shared with readout028.py, so the same pass cannot yield two different non-degenerate counts.
DEGENERATE = locaterm.DEGENERATE


def parse(path: Path) -> list[tuple[str, float, float, float]]:
    text = path.read_text(errors="replace")
    # Anchor on the summary block so a mid-run "Environment:" line in debug output cannot be read as a
    # result. Without the anchor a truncated log yields a partial table that looks complete.
    cut = text.rfind("Per-Group Results:")
    if cut < 0:
        sys.exit(f"{path.name}: no 'Per-Group Results:' block -- the pass did not reach its summary, so "
                 f"there are no per-environment numbers to report (timeout? crash? check the log tail).")
    rows = []
    for blk in text[cut:].split("\n  Group ")[1:]:
        env = re.search(r"Environment: \S*?([A-Za-z0-9]+Env)\b", blk)
        acc = re.search(r"Avg Accuracy: ([0-9.]+)", blk)
        stp = re.search(r"Avg Steps: ([0-9.]+)", blk)
        cst = re.search(r"Avg Cost: \$([0-9.]+)", blk)
        if env and acc:
            rows.append((env.group(1), float(acc.group(1)),
                         float(stp.group(1)) if stp else float("nan"),
                         float(cst.group(1)) if cst else float("nan")))
    if not rows:
        sys.exit(f"{path.name}: the summary block parsed to zero environments. Refusing to report an "
                 f"empty table as a result.")
    return rows


def main() -> None:
    tag = sys.argv[1] if len(sys.argv) > 1 else sys.exit("usage: perenv028.py <tag>")
    log = Path.home() / "cg-loca" / f"i022loca-{tag}.log"
    if not log.exists():
        sys.exit(f"no log at {log}")
    rows = parse(log)

    print(f"{'environment':<40} {'acc':>6} {'steps':>7} {'cost':>8}")
    solved = degen_solved = 0
    for env, acc, stp, cst in sorted(rows, key=lambda r: -r[1]):
        mark = " (known-zero)" if env in DEGENERATE else ""
        print(f"{env:<40} {acc:>6.3f} {stp:>7.1f} {cst:>8.2f}{mark}")
        if acc > 0:
            solved += 1
            if env in DEGENERATE:
                degen_solved += 1
    n = len(rows)
    live = [r for r in rows if r[0] not in DEGENERATE]
    print(f"\n{n} environments, {len(live)} of them not known-zero.")
    print(f"came off zero: {solved}/{n} overall, {solved - degen_solved}/{len(live)} of the live ones.")
    if live:
        print(f"mean accuracy: {sum(r[1] for r in rows)/n:.4f} over all, "
              f"{sum(r[1] for r in live)/len(live):.4f} over the live ones.")
    print(f"total cost: ${sum(r[3] for r in rows):.2f}")
    if degen_solved:
        print(f"\nNOTE: {degen_solved} environment(s) marked known-zero scored above zero here. The "
              f"degenerate list is from iteration 024 and no longer describes this configuration; "
              f"readout028.py excludes the same three and its power arithmetic is now wrong too.")


if __name__ == "__main__":
    main()
