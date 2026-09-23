#!/usr/bin/env python3
"""Follow a running pass and apply the PRE-DECLARED futility gate for the bounded-thinking baseline.

THE CRITERION, fixed before the pass was launched. Seed 1's UNBOUNDED baseline solved 5 of 15:

    AcademicWarning, SetConfCrDdl, ExcelMarketResearch, UpdateMaterialInventory, PayableInvoiceChecker

Paired by environment as completions stream in, STOP once the bounded pass has lost 3 of those 5 net of
any gains. Losing 3 of 5 cannot recover to parity over the remaining tasks, so this is futility and not a
peek at a win -- stopping for futility does not inflate type-I error, stopping for a win does.

Prints one line per completed task, so the operator sees each one rather than a verdict at the end, and
exits non-zero the moment the gate trips. Exit 0 means the pass ran to completion without tripping.

Partial results survive a kill: LOCA writes `Final reward (accuracy)` per task as its workers finish, and
those lines are per-environment, so every COMPLETED task stays pairable against the pilot even though no
summary block or results.json is produced.
"""
import re
import sys
import time
from pathlib import Path

BASELINE_SOLVED = {"AcademicWarningS2LEnv", "SetConfCrDdlS2LEnv", "ExcelMarketResearchS2LEnv",
                   "UpdateMaterialInventoryS2LEnv", "PayableInvoiceCheckerS2LEnv"}
LOSS_LIMIT = 3   # of the five above, net of gains
TOTAL = 15


def main() -> int:
    tag = sys.argv[1] if len(sys.argv) > 1 else sys.exit("usage: gate028.py <tag>")
    log = Path.home() / "cg-loca" / f"i022loca-{tag}.log"
    deadline = time.time() + 6 * 3600
    seen, env = {}, {}
    pos = 0
    while time.time() < deadline:
        if log.exists():
            text = log.read_text(errors="replace")
            for m in re.finditer(r"\[Task(\d+)-Config\d+-Run0\] Environment: \S*?([A-Za-z0-9]+Env)", text):
                env[m.group(1)] = m.group(2)
            for m in re.finditer(r"\[Task(\d+)-Config\d+-Run0\] Final reward \(accuracy\): ([0-9.]+)", text):
                t, acc = m.group(1), float(m.group(2))
                if t in seen:
                    continue
                seen[t] = acc
                name = env.get(t, "Task" + t)
                lost = sorted(e for k, e in env.items() if k in seen and e in BASELINE_SOLVED and seen[k] == 0)
                gained = sorted(e for k, e in env.items()
                                if k in seen and e not in BASELINE_SOLVED and seen[k] > 0)
                net = len(lost) - len(gained)
                solved = sum(1 for a in seen.values() if a > 0)
                verdict = "STOP" if net >= LOSS_LIMIT else "continue"
                print("[%2d/%d] %-34s acc %.1f | solved %d | lost %d of the 5 (%s) | gained %d (%s) "
                      "| net %+d | %s"
                      % (len(seen), TOTAL, name, acc, solved, len(lost),
                         ",".join(e[:12] for e in lost) or "-", len(gained),
                         ",".join(e[:12] for e in gained) or "-", net, verdict), flush=True)
                if net >= LOSS_LIMIT:
                    print("GATE TRIPPED: bounded thinking has lost %d of the 5 environments the unbounded "
                          "pilot solved, net of %d gain(s). Kill the pass; completed tasks stay pairable "
                          "from this log." % (len(lost), len(gained)), flush=True)
                    return 2
            if len(seen) >= TOTAL:
                print("GATE PASSED: all %d tasks completed without tripping (%d solved)."
                      % (TOTAL, sum(1 for a in seen.values() if a > 0)), flush=True)
                return 0
        time.sleep(20)
    print("gate028: deadline reached with %d/%d completed" % (len(seen), TOTAL), flush=True)
    return 1


if __name__ == "__main__":
    sys.exit(main())
