#!/usr/bin/env python3
"""Iteration 025 Amendment 1 drift check: is iteration 024's arm A still a valid control?

A-against-A on identical config, so the expected difference is ZERO. Criterion fixed in the
preregistration BEFORE this run: two-sided exact McNemar on the discordant tasks at alpha=0.05, plus
mean per-task cost within +/-30%. Pass => pair seeds 2-5 against i024As1. Fail => run arm A at seeds
2-5 and draw no B-A conclusion until it does.

Usage: drift_check.py <i024As1_results.json> <i025As1_results.json>
"""
import json, sys, math, os


def load(p):
    d = json.load(open(p))
    pc = d["per_config"]
    return ({k: (v.get("avg_accuracy") or 0.0) for k, v in pc.items()},
            {k: (v.get("avg_cost_usd") or 0.0) for k, v in pc.items()},
            {k: (v.get("avg_steps") or 0.0) for k, v in pc.items()})


def binom_two_sided(b, c):
    """Exact two-sided McNemar: P(|X - n/2| >= |b - n/2|) for X ~ Bin(n, 0.5), n = b + c."""
    n = b + c
    if n == 0:
        return 1.0
    obs = abs(b - n / 2.0)
    tot = 0.0
    for k in range(n + 1):
        if abs(k - n / 2.0) >= obs - 1e-12:
            tot += math.comb(n, k)
    return min(1.0, tot / (2.0 ** n))


def main(old_p, new_p):
    a_old, c_old, s_old = load(old_p)
    a_new, c_new, s_new = load(new_p)
    tasks = sorted(set(a_old) & set(a_new))
    missing = sorted(set(a_old) ^ set(a_new))
    if missing:
        print("REFUSING: task sets differ, so pairing is not total:", missing)
        return 2
    print(f"{len(tasks)} paired tasks (A vs A, identical config -- expected difference is zero)\n")

    b = c = 0   # b: old solved & new not, c: new solved & old not
    print("| task | i024As1 | i025As1 | cost then | cost now |")
    print("|---|---|---|---|---|")
    for t in tasks:
        o, n = a_old[t], a_new[t]
        flag = ""
        if o > n:
            b += 1; flag = "  <-- regressed"
        elif n > o:
            c += 1; flag = "  <-- improved"
        print(f"| `{t}` | {o} | {n} | ${c_old[t]:.2f} | ${c_new[t]:.2f} |{flag}")

    p = binom_two_sided(b, c)
    so, sn = sum(a_old.values()), sum(a_new.values())
    mo = sum(c_old.values()) / len(tasks)
    mn = sum(c_new.values()) / len(tasks)
    dc = (mn - mo) / mo if mo else 0.0

    print(f"\nsolved-equivalent: iter024 {so:.3f} -> iter025 {sn:.3f}  of {len(tasks)}")
    print(f"discordant: regressed={b} improved={c}   exact two-sided McNemar p = {p:.4f}")
    print(f"mean per-task cost: ${mo:.3f} -> ${mn:.3f}  ({dc:+.1%})")
    print(f"mean steps: {sum(s_old.values())/len(tasks):.1f} -> {sum(s_new.values())/len(tasks):.1f}")

    acc_ok, cost_ok = p >= 0.05, abs(dc) <= 0.30
    print("\n  accuracy criterion (p >= 0.05): " + ("PASS" if acc_ok else "FAIL"))
    print("  cost criterion (within +/-30%): " + ("PASS" if cost_ok else "FAIL"))
    if acc_ok and cost_ok:
        print("\nDRIFT CHECK PASSES -- iteration 024's arm A is a valid control; pair seeds 2-5 "
              "against it as registered.")
        return 0
    print("\nDRIFT CHECK FAILS -- the reused baseline is contaminated. Per the preregistration: run "
          "arm A at seeds 2-5 (+~$170) and draw NO B-A conclusion until it completes.")
    return 1


if __name__ == "__main__":
    if len(sys.argv) != 3:
        print(__doc__)
        sys.exit(64)
    sys.exit(main(sys.argv[1], sys.argv[2]))
