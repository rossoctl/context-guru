#!/usr/bin/env python3
"""Summarize sweep-results.csv into a comparison table.

Per config: cells run, #reward=1, mean within-run token savings% (the honest
metric — before/after are the SAME request stream, immune to per-run trajectory
variance), total tokens saved. Also a reward-parity view restricted to tasks the
baseline actually resolved, so 'did context-guru hurt correctness' is answerable.
"""
import csv, os, statistics as st
from collections import defaultdict

CSV = os.path.join(os.path.dirname(__file__), "sweep-results.csv")


def num(x):
    try:
        return float(x)
    except Exception:
        return None


def main():
    rows = [r for r in csv.DictReader(open(CSV)) if not str(r["note"]).startswith("up-failed")]
    by_cfg = defaultdict(list)
    for r in rows:
        by_cfg[r["config"]].append(r)

    # baseline-resolved task set (reward==1) for honest parity
    base_ok = {r["task"] for r in by_cfg.get("baseline", []) if num(r["reward"]) == 1}

    order = ["baseline", "cg-format", "cg-dedup", "cg-cmdfilter", "cg-cacheinject", "cg-conservative", "cg-balanced"]
    print(f"{'config':<16}{'cells':>6}{'reward=1':>9}{'mean_save%':>11}{'tot_saved':>10}"
          f"{'parity(base✓)':>15}")
    for cfg in order + [c for c in by_cfg if c not in order]:
        rs = by_cfg.get(cfg)
        if not rs:
            continue
        cells = len(rs)
        won = sum(1 for r in rs if num(r["reward"]) == 1)
        saves = [num(r["gw_pct"]) for r in rs if num(r["gw_pct"]) is not None]
        tot = sum(int(float(r["gw_saved"])) for r in rs if num(r["gw_saved"]) is not None)
        meansave = f"{st.mean(saves):.1f}" if saves else "-"
        # reward on tasks baseline resolved
        par = [r for r in rs if r["task"] in base_ok]
        par_won = sum(1 for r in par if num(r["reward"]) == 1)
        parity = f"{par_won}/{len(par)}" if par else "-"
        print(f"{cfg:<16}{cells:>6}{won:>9}{meansave:>11}{tot:>10}{parity:>15}")

    print("\nPer-task within-run savings% (cg-conservative, or cg-balanced in older CSVs) and reward (baseline vs preset):")
    tasks = sorted({r["task"] for r in rows})
    bal = {r["task"]: r for r in by_cfg.get("cg-conservative") or by_cfg.get("cg-balanced", [])}
    base = {r["task"]: r for r in by_cfg.get("baseline", [])}
    for t in tasks:
        b, bl = base.get(t), bal.get(t)
        print(f"  {t:<34} baseline_reward={b['reward'] if b else '-':<4} "
              f"preset_reward={bl['reward'] if bl else '-':<4} "
              f"preset_save%={bl['gw_pct'] if bl else '-'}")


if __name__ == "__main__":
    main()
