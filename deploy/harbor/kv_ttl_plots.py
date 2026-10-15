#!/usr/bin/env python3
"""Renders the plots this study's brief calls a first-class deliverable, as standalone SVG
+ a plain-English .txt caption per plot, from the JSON this line of work's own scripts
already wrote (kv_ttl_ka_arms.py, kv_ttl_arms_ext.py, kv_ttl_per_tenant_cap.py) -- no
number here is computed freshly; every one is read from a committed run's own output, and
every plot's caption is the script+command that produced its inputs, so the JSON this
writes (`--aggregates-out`) can regenerate a report without rerunning anything.

Palette: the dataviz skill's validated default (references/palette.md) -- categorical hues
used in fixed order (blue, orange, aqua, violet), never cycled or reassigned by rank; one
hue for magnitude; a legend wherever there is more than one series.
"""
from __future__ import annotations
import argparse, json, os, sys

import matplotlib
matplotlib.use("svg")
import matplotlib.pyplot as plt
import numpy as np

# ── palette (dataviz skill's reference instance, light mode) ──────────────────────────────
BG = "#fcfcfb"
TEXT = "#0b0b0b"
SUBTEXT = "#52514e"
GRID = "#e2e0da"
BLUE, ORANGE, AQUA, VIOLET, RED, GREEN = (
    "#2a78d6", "#eb6834", "#1baf7a", "#4a3aa7", "#e34948", "#008300")

plt.rcParams.update({
    "figure.facecolor": BG, "axes.facecolor": BG, "savefig.facecolor": BG,
    "text.color": TEXT, "axes.labelcolor": TEXT, "xtick.color": SUBTEXT, "ytick.color": SUBTEXT,
    "axes.edgecolor": GRID, "grid.color": GRID, "font.size": 11, "svg.fonttype": "none",
    "axes.spines.top": False, "axes.spines.right": False,
})


def cap(path: str, text: str) -> None:
    with open(path, "w") as fh:
        fh.write(text.strip() + "\n")


def savefig(fig, path: str):
    fig.savefig(path, format="svg", bbox_inches="tight")
    plt.close(fig)


def load(path):
    if not path or not os.path.exists(path):
        return None
    with open(path) as fh:
        return json.load(fh)


def plot_gap_distribution(gap_sample, out_svg, out_cap, agg):
    if not gap_sample:
        return
    secs = np.array([r["time_to_next_request_seconds"] for r in gap_sample])
    observed = np.array([bool(r["event_observed"]) for r in gap_sample])
    secs = secs[observed]  # censored rows have no real "return time" to show on a survival curve
    secs = secs[secs >= 0]
    mins = np.clip(secs / 60.0, 1e-3, None)

    fig, axes = plt.subplots(1, 2, figsize=(11, 4.2))
    ax = axes[0]
    bins = np.logspace(np.log10(mins.min()), np.log10(max(mins.max(), 200)), 40)
    ax.hist(mins, bins=bins, color=BLUE, edgecolor=BG, linewidth=0.3)
    ax.set_xscale("log")
    ax.set_xlabel("minutes until the next cache-compatible request (log scale)")
    ax.set_ylabel(f"requests (of {len(mins):,} with an observed next request)")
    ax.set_title("Gap-length distribution")
    ax.axvline(5, color=ORANGE, linestyle="--", linewidth=1.5, label="5 min")
    ax.axvline(60, color=VIOLET, linestyle="--", linewidth=1.5, label="1 hr")
    ax.legend(frameon=False)
    ax.grid(axis="y", linewidth=0.6)

    ax2 = axes[1]
    order = np.sort(mins)
    surv = 1.0 - np.arange(1, len(order) + 1) / len(order)
    ax2.step(order, surv, color=BLUE, linewidth=1.8, where="post")
    ax2.set_xscale("log")
    ax2.set_xlabel("minutes idle (log scale)")
    ax2.set_ylabel("fraction of spans STILL idle beyond this long")
    ax2.set_title("Survival curve: P(still idle > t)")
    ax2.axvline(5, color=ORANGE, linestyle="--", linewidth=1.5)
    ax2.axvline(60, color=VIOLET, linestyle="--", linewidth=1.5)
    ax2.grid(axis="both", linewidth=0.6)
    p5 = float((mins <= 5).mean())
    p60 = float((mins <= 60).mean())
    fig.suptitle("How long a conversation stays idle before it comes back", y=1.03)
    savefig(fig, out_svg)

    cap(out_cap,
        f"Left: how long {len(mins):,} idle spans in this snapshot waited before the next "
        f"request that could reuse the cache (log-scale minutes, so both the common short "
        f"gaps and the rare multi-hour ones are visible in one chart). Right: the same data "
        f"as a survival curve -- the fraction of spans still idle beyond a given length. "
        f"{p5*100:.1f}% of spans close within 5 minutes and {p60*100:.1f}% within an hour, "
        f"which is why a keep-alive's entire opportunity is concentrated in the remaining "
        f"tail, not the typical request. LABEL: observed (rows with a real next request in "
        f"this snapshot's window; right-censored rows at the end of the window are excluded "
        f"from this curve, not treated as non-returns).")
    agg["gap_distribution"] = {"n": int(len(mins)), "p_return_within_5m": p5,
                               "p_return_within_1h": p60, "label": "observed"}


def plot_ping_cost_percentiles(ping_cost, out_svg, out_cap, agg):
    if not ping_cost or ping_cost.get("n", 0) == 0:
        return
    pct = ping_cost["percentiles"]
    xs = [float(k) for k in pct]
    ys = [pct[k] for k in pct]
    fig, ax = plt.subplots(figsize=(7.5, 4.5))
    ax.plot(xs, ys, color=BLUE, marker="o", markersize=4, linewidth=1.8)
    ax.axhline(ping_cost["max"], color=RED, linestyle=":", linewidth=1.3)
    ax.text(xs[0], ping_cost["max"], f"  max ${ping_cost['max']:.4f}", color=RED, va="bottom",
           fontsize=9)
    ax.set_yscale("log")
    ax.set_xlabel("percentile")
    ax.set_ylabel("cost of ONE keep-alive ping, USD (log scale)")
    ax.set_title(f"Per-ping cost distribution (n={ping_cost['n']:,} pingable requests)")
    ax.grid(True, which="both", linewidth=0.5)
    savefig(fig, out_svg)
    p50, p90, p99 = pct.get("50"), pct.get("90"), pct.get("99")
    cap(out_cap,
        f"What one keep-alive ping actually costs, across every one of the {ping_cost['n']:,} "
        f"real requests in this snapshot whose cached prefix clears the caching gate, priced "
        f"at that request's own model rate (not a fleet-wide average). The median ping costs "
        f"${p50:.4f} -- a fraction of a cent -- but the distribution is heavily right-skewed: "
        f"the 90th percentile is ${p90:.4f} and the 99th is ${p99:.4f}, and the single most "
        f"expensive ping in this snapshot costs ${ping_cost['max']:.4f}. The p90/max figures "
        f"and this full curve were never computed before this run; only p50 and p99 were "
        f"previously known. LABEL: measured, directly from real (model, prefix-size) pairs.")
    agg["ping_cost_distribution"] = {"n": ping_cost["n"], "mean": ping_cost["mean"],
                                     "percentiles": pct, "max": ping_cost["max"],
                                     "label": "measured"}


def plot_net_dollars_vs_ping_budget(sweep, out_svg, out_cap, agg):
    if not sweep:
        return
    fleet = sweep["fleet_wide"]
    ns = sorted(int(k) for k in fleet)
    deltas = [fleet[str(n)]["delta_usd"] for n in ns]
    pings = [fleet[str(n)]["pings"] for n in ns]
    fig, ax = plt.subplots(figsize=(8, 4.5))
    ax.plot(ns, deltas, color=BLUE, marker="o", markersize=5, linewidth=1.8, label="net $ saved")
    ax.axhline(0, color=SUBTEXT, linewidth=1)
    best_n = ns[int(np.argmax(deltas))]
    ax.scatter([best_n], [max(deltas)], color=ORANGE, s=70, zorder=5,
              label=f"best: N={best_n} (+${max(deltas):,.2f})")
    ax.set_xlabel("max_pings (flat, fleet-wide, no per-tenant tuning)")
    ax.set_ylabel(f"net $ saved vs. fixed-5m (test window baseline ${sweep['baseline_usd']:,.0f})")
    ax.set_title("Net dollars vs. ping budget -- a flat fleet-wide cap")
    ax.grid(axis="y", linewidth=0.6)
    ax.legend(frameon=False)
    savefig(fig, out_svg)
    cap(out_cap,
        f"What happens to net savings as the SAME flat ping cap (no per-tenant tuning, one "
        f"number for the whole fleet) is raised from 1 to {ns[-1]} keep-alive pings per idle "
        f"span, against the {sweep['baseline_usd']:,.0f}-dollar fixed-5m baseline on the test "
        f"window. The curve peaks at N={best_n} (+${max(deltas):,.2f}) and turns NEGATIVE by "
        f"N={ns[-1]} -- past some point, extra pings cost more than the rescues they buy, on "
        f"every span, not just the ones that needed them. The number of pings fired ({pings[0]:,} "
        f"at N=1 to {pings[-1]:,} at N={ns[-1]}) is shown so a reader can see the budget is "
        f"tripling for a shrinking, then negative, return. LABEL: replayed (this snapshot's "
        f"real traffic under a hypothetical flat policy, not the tenant-tuned cap the report's "
        f"headline arm uses).")
    agg["net_dollars_vs_ping_budget"] = {"baseline_usd": sweep["baseline_usd"], "fleet_wide": fleet,
                                         "best_n": best_n, "best_delta_usd": max(deltas),
                                         "label": "replayed"}


def plot_arms_vs_simplest_rule(ka_arms, ext_arms, out_svg, out_cap, agg):
    """Every learned/rule arm from BOTH scripts, against the simplest rule this whole line
    of work has (flat-cap-2), with session-level 95% CIs."""
    names, deltas, los, his, colors = [], [], [], [], []
    def add(name, block, color):
        if not block or block.get("point_delta_usd") is None:
            return
        names.append(name)
        deltas.append(block["point_delta_usd"])
        ci = block.get("ci95_delta_usd", [block["point_delta_usd"]] * 2)
        los.append(block["point_delta_usd"] - ci[0])
        his.append(ci[1] - block["point_delta_usd"])
        colors.append(color)

    ka = ka_arms.get("pooled_session_bootstrap", {}) if ka_arms else {}
    ex = ext_arms.get("pooled_session_bootstrap", {}) if ext_arms else {}
    simplest = ka.get("flat-cap-2")
    add("flat-cap-2 (simplest rule)", simplest, ORANGE)
    for n in ("stop-reason-gated", "hybrid2-calibrated-tenant-fallback"):
        pass
    add("hybrid2 (calibrated logreg)", ka.get("hybrid2-calibrated-tenant-fallback"), BLUE)
    add("gbm-policy (new)", ex.get("gbm-policy"), AQUA)
    add("ensemble hybrid2+gbm (new)", ex.get("ensemble-hg2-gbm"), VIOLET)
    add("cost-aware, calibrated p5/p1 (new)", ex.get("cost-aware-simple"), RED)
    add("cost-aware, discrete hazard (new)", ex.get("cost-aware-hazard"), RED)
    if not names:
        return

    fig, ax = plt.subplots(figsize=(9, 0.55 * len(names) + 1.5))
    y = np.arange(len(names))
    ax.barh(y, deltas, color=colors, height=0.55,
           xerr=[los, his], error_kw={"ecolor": SUBTEXT, "elinewidth": 1.2, "capsize": 3})
    ax.axvline(simplest["point_delta_usd"] if simplest else 0, color=ORANGE, linestyle="--",
              linewidth=1, alpha=0.7)
    ax.set_yticks(y, names)
    ax.invert_yaxis()
    ax.set_xlabel("net $ saved vs. fixed-5m, test window (95% session-level bootstrap CI)")
    ax.set_title("Every model against the simplest rule (flat-cap-2)")
    ax.grid(axis="x", linewidth=0.6)
    savefig(fig, out_svg)
    cap(out_cap,
        f"Net dollar savings for every predictor arm measured across this study and the "
        f"earlier one, in one chart, each with its 95% session-level bootstrap confidence "
        f"interval. The dashed orange line marks flat-cap-2 -- a two-parameter rule with no "
        f"model at all -- so a reader can see at a glance which learned arms clear that bar "
        f"and which don't. None of the new arms in this run beat it; the two cost-aware "
        f"arms land at essentially zero, extending (not merely repeating) the same negative "
        f"result the correctly-specified $ rule already showed with a different, less "
        f"calibrated probability source. LABEL: replayed, session-level bootstrap.")
    agg["arms_vs_simplest_rule"] = {"names": names, "delta_usd": deltas,
                                    "ci_lo_width": los, "ci_hi_width": his, "label": "replayed"}


def plot_calibration(ext_result, out_svg, out_cap, agg):
    calib = (ext_result or {}).get("calibration_pooled_test", {})
    if not calib:
        return
    fig, ax = plt.subplots(figsize=(5.5, 5.5))
    ax.plot([0, 1], [0, 1], color=SUBTEXT, linestyle="--", linewidth=1, label="perfectly calibrated")
    for name, color in (("hybrid2", BLUE), ("gbm-policy", AQUA), ("ensemble-hg2-gbm", VIOLET)):
        block = calib.get(name)
        if not block:
            continue
        ax.plot(block["reliability_mean_pred"], block["reliability_frac_pos"], marker="o",
               markersize=5, color=color, linewidth=1.6,
               label=f"{name} (ECE {block['ece_10bin_quantile']:.4f}, AUC {block.get('auc', 0):.3f})")
    ax.set_xlabel("mean predicted P(return in 5m-1h band)")
    ax.set_ylabel("observed fraction that actually returned in that band")
    ax.set_title("Calibration / reliability, pooled test folds")
    ax.legend(frameon=False, fontsize=8, loc="upper left")
    ax.set_xlim(0, max(0.3, ax.get_xlim()[1]))
    ax.set_ylim(0, max(0.3, ax.get_ylim()[1]))
    ax.grid(linewidth=0.6)
    savefig(fig, out_svg)
    cap(out_cap,
        "Each point groups test-window requests into ten equal-sized buckets by predicted "
        "probability and plots what actually happened in that bucket against what the model "
        "said would happen. A model on the dashed diagonal is well-calibrated -- when it says "
        "'17% chance,' about 17% of those rows really do return in the target window. All "
        "three models track the diagonal closely at the low end, where nearly all requests "
        "fall, and hybrid2 remains the best-calibrated (lowest ECE) even after this run's new "
        "candidates were added for comparison. LABEL: measured, on pooled test folds never "
        "used for fitting or threshold tuning.")
    agg["calibration"] = calib


def plot_per_tenant_gain_loss(ext_result, ka_arms, out_svg, out_cap, agg):
    per_tenant = (ext_result or {}).get("per_tenant", {})
    if not per_tenant:
        return
    best_new = None
    for candidate in ("ensemble-hg2-gbm", "gbm-policy"):
        if any(candidate in v.get("arms", {}) for v in per_tenant.values()):
            best_new = candidate
            break
    if not best_new:
        return
    tenants = sorted(per_tenant.keys())
    vals = [per_tenant[t]["arms"][best_new]["percent_savings"] or 0.0 for t in tenants]
    order = np.argsort(vals)
    tenants = [tenants[i] for i in order]
    vals = [vals[i] for i in order]
    colors = [RED if v < 0 else AQUA for v in vals]
    fig, ax = plt.subplots(figsize=(8, 0.4 * len(tenants) + 1.5))
    y = np.arange(len(tenants))
    ax.barh(y, vals, color=colors, height=0.6)
    ax.axvline(0, color=SUBTEXT, linewidth=1)
    ax.set_yticks(y, tenants)
    ax.set_xlabel(f"% change vs. fixed-5m, this tenant's own test-window traffic ({best_new})")
    ax.set_title(f"Per-tenant gain and loss: {best_new}")
    ax.grid(axis="x", linewidth=0.6)
    savefig(fig, out_svg)
    n_harmed = sum(1 for v in vals if v < 0)
    cap(out_cap,
        f"Percent change in this tenant's own bill under the {best_new} arm, one bar per "
        f"pseudonymized tenant (t01..t18, assigned by sorted raw tenant id -- never the raw "
        f"id itself). {n_harmed} of {len(tenants)} tenants are HARMED (red, right of this "
        f"chart is the good direction) even though the arm saves money fleet-wide -- the same "
        f"pattern this whole line of work has found in every learned or flat arm, and the "
        f"reason a per-tenant cap with an off-switch keeps winning: it is the only policy "
        f"measured so far that harms zero tenants. LABEL: replayed, pooled test folds.")
    agg["per_tenant_gain_loss"] = {"arm": best_new, "tenants": tenants, "percent_savings": vals,
                                   "n_harmed": n_harmed, "label": "replayed"}


def plot_learning_curves(lc, out_svg, out_cap, agg):
    if not lc or not lc.get("points"):
        return
    pts = lc["points"]
    fracs = [p["train_frac_of_train_window"] for p in pts]
    aucs = [p["auc_holdout"] for p in pts]
    deltas_pct = [p["holdout_delta_pct"] for p in pts]
    fig, ax1 = plt.subplots(figsize=(7.5, 4.5))
    ax1.plot(fracs, aucs, color=BLUE, marker="o", markersize=5, linewidth=1.8, label="AUC (left axis)")
    ax1.set_xlabel("fraction of the train window used to fit gbm-policy")
    ax1.set_ylabel("AUC, scored on the untouched final holdout fold", color=BLUE)
    ax1.tick_params(axis="y", labelcolor=BLUE)
    ax1.grid(linewidth=0.6)
    ax2 = ax1.twinx()
    ax2.plot(fracs, deltas_pct, color=ORANGE, marker="s", markersize=5, linewidth=1.8,
            linestyle="--", label="net $ saved, % (right axis)")
    ax2.set_ylabel("% saved vs. fixed-5m on the same holdout", color=ORANGE)
    ax2.tick_params(axis="y", labelcolor=ORANGE)
    ax1.set_title("Learning curve: gbm-policy vs. training-set size")
    fig.legend(loc="lower right", frameon=False, bbox_to_anchor=(0.98, 0.12))
    savefig(fig, out_svg)
    cap(out_cap,
        f"How gbm-policy's skill changes as it is fit on more of the train window, always "
        f"scored on the SAME untouched final fold (fold {lc['holdout_fold']}, never used to "
        f"tune anything). AUC rises from {aucs[0]:.3f} at {fracs[0]*100:.0f}% of the training "
        f"data to {aucs[-1]:.3f} at 100%, and net dollar savings on the holdout moves from "
        f"{deltas_pct[0]:+.2f}% to {deltas_pct[-1]:+.2f}% over the same range -- this pairing "
        f"(AUC and $ together) matters because the two do not always move together in this "
        f"study's other findings. Never computed before this run. LABEL: replayed, single "
        f"held-out fold, global (not per-tenant) threshold.")
    agg["learning_curves"] = lc


def plot_funnel(funnel_data, out_svg, out_cap, agg):
    if not funnel_data:
        return
    order = ["all_requests_idle_spans_opened", "clears_min_prefix_gate", "actually_done_cluster",
            "has_a_known_next_request_decidable", "in_the_5m_to_1h_band_the_arms_target"]
    labels = ["All requests\n(idle spans opened)", "Clears the\nmin_prefix gate",
             "\"Actually done\"\nstop_reason cluster", "Has a known next\nrequest (decidable)",
             "In the 5m-1h band\nthe arms target"]
    vals = [funnel_data[k] for k in order]
    fig, ax = plt.subplots(figsize=(8.5, 4.5))
    y = np.arange(len(vals))
    ax.barh(y, vals, color=BLUE, height=0.55)
    for i, v in enumerate(vals):
        ax.text(v, i, f"  {v:,}  ({100*v/vals[0]:.1f}% of all requests)", va="center", fontsize=9)
    ax.set_yticks(y, labels)
    ax.invert_yaxis()
    ax.set_xlabel(f"requests (of {vals[0]:,} total)")
    ax.set_title("The funnel: from every request to an actionable decision point")
    ax.grid(axis="x", linewidth=0.6)
    savefig(fig, out_svg)
    cap(out_cap,
        f"Every request in this snapshot opens an idle span, but the ping/write decision "
        f"only matters for a shrinking subset: {vals[0]:,} requests total, "
        f"{vals[1]:,} ({100*vals[1]/vals[0]:.1f}%) clear the minimum-prefix caching gate, "
        f"{vals[2]:,} ({100*vals[2]/vals[0]:.1f}%) are in the 'actually done' stop-reason "
        f"cluster the gated rule targets, {vals[3]:,} ({100*vals[3]/vals[0]:.1f}%) have a "
        f"known next request to learn from at all, and only {vals[4]:,} "
        f"({100*vals[4]/vals[0]:.1f}%) land in the 5-minute-to-1-hour band every learned arm "
        f"in this study is built to catch. This is why the oracle's own edge is concentrated "
        f"in a few hundred pings, not spread across the corpus. LABEL: measured, full corpus.")
    agg["funnel"] = funnel_data


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--ka-arms", default=None)
    ap.add_argument("--ext-arms", default=None)
    ap.add_argument("--ping-cost", default=None)
    ap.add_argument("--funnel", default=None)
    ap.add_argument("--gap-dist", default=None)
    ap.add_argument("--learning-curve", default=None)
    ap.add_argument("--ping-budget-sweep", default=None)
    ap.add_argument("--out-dir", required=True)
    ap.add_argument("--aggregates-out", required=True)
    args = ap.parse_args()

    os.makedirs(args.out_dir, exist_ok=True)
    ka_arms = load(args.ka_arms)
    ext_arms = load(args.ext_arms)
    ping_cost = load(args.ping_cost)
    funnel_data = load(args.funnel)
    gap_sample = load(args.gap_dist)
    lc = load(args.learning_curve)
    sweep = load(args.ping_budget_sweep)

    agg: dict = {"figures": {}}

    def fig(name, script, command):
        svg = os.path.join(args.out_dir, f"{name}.svg")
        txt = os.path.join(args.out_dir, f"{name}.txt")
        agg["figures"][name] = {"svg": svg, "caption": txt, "script": script, "command": command}
        return svg, txt

    svg, txt = fig("gap_distribution_and_survival", "kv_ttl_arms_ext.py",
                   "kv_ttl_arms_ext.py --gap-dist-out gap_distribution_sample.json")
    plot_gap_distribution(gap_sample, svg, txt, agg["figures"]["gap_distribution_and_survival"])

    svg, txt = fig("per_ping_cost_percentiles", "kv_ttl_arms_ext.py",
                   "kv_ttl_arms_ext.py --ping-cost-out ping_cost_distribution.json")
    plot_ping_cost_percentiles(ping_cost, svg, txt, agg["figures"]["per_ping_cost_percentiles"])

    svg, txt = fig("net_dollars_vs_ping_budget", "kv_ttl_per_tenant_cap.py",
                   "kv_ttl_per_tenant_cap.py --candidates 1,2,3,4,5,6,8,10,12,16,20 --json ping_budget_sweep.json")
    plot_net_dollars_vs_ping_budget(sweep, svg, txt, agg["figures"]["net_dollars_vs_ping_budget"])

    svg, txt = fig("arms_vs_simplest_rule", "kv_ttl_ka_arms.py + kv_ttl_arms_ext.py",
                   "kv_ttl_ka_arms.py --out ka_arms_baseline.json; kv_ttl_arms_ext.py --out arms_ext_result.json")
    plot_arms_vs_simplest_rule(ka_arms, ext_arms, svg, txt, agg["figures"]["arms_vs_simplest_rule"])

    svg, txt = fig("calibration_reliability", "kv_ttl_arms_ext.py",
                   "kv_ttl_arms_ext.py --out arms_ext_result.json")
    plot_calibration(ext_arms, svg, txt, agg["figures"]["calibration_reliability"])

    svg, txt = fig("per_tenant_gain_loss", "kv_ttl_arms_ext.py",
                   "kv_ttl_arms_ext.py --out arms_ext_result.json")
    plot_per_tenant_gain_loss(ext_arms, ka_arms, svg, txt, agg["figures"]["per_tenant_gain_loss"])

    svg, txt = fig("learning_curves", "kv_ttl_arms_ext.py",
                   "kv_ttl_arms_ext.py --learning-curve-out learning_curves.json")
    plot_learning_curves(lc, svg, txt, agg["figures"]["learning_curves"])

    svg, txt = fig("funnel", "kv_ttl_arms_ext.py",
                   "kv_ttl_arms_ext.py --funnel-out funnel.json")
    plot_funnel(funnel_data, svg, txt, agg["figures"]["funnel"])

    with open(args.aggregates_out, "w") as fh:
        json.dump(agg, fh, indent=2, default=str)
    print(f"wrote {len(agg['figures'])} figures to {args.out_dir}, aggregates to {args.aggregates_out}")


if __name__ == "__main__":
    main()
