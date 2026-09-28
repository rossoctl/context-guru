#!/usr/bin/env python3
"""Consolidates every run this night's experiments produced into ONE machine-readable
JSON -- every number behind every plot, plus everything the plots don't show (full pooled
bootstrap tables, per-tenant breakdowns, the reusemodel+stop_cluster side experiment, the
ping-budget sweep, thresholds) -- so a report generator (or a reviewer) never has to rerun
anything. Pure concatenation: no number here is computed, only copied and labelled with the
script and command that produced it.
"""
from __future__ import annotations
import argparse, json, os


def load(path):
    if not path or not os.path.exists(path):
        return None
    with open(path) as fh:
        return json.load(fh)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--runs-dir", required=True)
    ap.add_argument("--plots-aggregates", required=True)
    ap.add_argument("--reusemodel-result", default=None)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    r = args.runs_dir
    ka = load(os.path.join(r, "ka_arms_baseline.json"))
    ext = load(os.path.join(r, "arms_ext_result.json"))
    sweep = load(os.path.join(r, "ping_budget_sweep.json"))
    ping_cost = load(os.path.join(r, "ping_cost_distribution.json"))
    funnel = load(os.path.join(r, "funnel.json"))
    lc = load(os.path.join(r, "learning_curves.json"))
    plots = load(args.plots_aggregates)
    reuse = load(args.reusemodel_result)

    out = {
        "provenance": {
            "note": "Every block below is copied verbatim from a committed script's own "
                    "output; nothing here is recomputed. See docs/results and reports3/ "
                    "for the config cards (train/test windows, seeds, hyperparameters).",
        },
        "figures": (plots or {}).get("figures", {}),
        "runs": {
            "ka_arms_baseline.json (established arms, reproduced)": ka,
            "arms_ext_result.json (this study's new arms)": ext,
            "ping_budget_sweep.json (net $ vs max_pings, fleet-wide)": sweep,
            "ping_cost_distribution.json (per-ping $ percentiles, full)": ping_cost,
            "funnel.json": funnel,
            "learning_curves.json": lc,
            "reusemodel_stopcluster_result.json (issue #326, feature-value only)": reuse,
        },
    }
    with open(args.out, "w") as fh:
        json.dump(out, fh, indent=2, default=str)
    print(f"wrote {args.out} ({os.path.getsize(args.out):,} bytes)")


if __name__ == "__main__":
    main()
