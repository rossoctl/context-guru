#!/usr/bin/env python3
"""Draw the fictive dashboard screenshots used in spec.md section 6.

Every number here is invented. Each image carries a FICTIVE DATA mark. The images show
layout and wording, not results. Run: python3 gen_mockups.py (writes ../img/dash-*.png).
"""
import os

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt
import numpy as np
from matplotlib.patches import FancyBboxPatch

OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "img")
BG, PANEL, EDGE, TXT, DIM = "#111217", "#181b1f", "#2a2d33", "#ccccdc", "#8e8e9e"
GREEN, RED, BLUE, YEL, ORG, PUR, TEAL = "#73BF69", "#F2495C", "#5794F2", "#FADE2A", "#FF9830", "#B877D9", "#4ECDC4"
rng = np.random.default_rng(886)


def canvas(title, w=16, h=10):
    fig = plt.figure(figsize=(w, h), dpi=100, facecolor=BG)
    fig.text(0.015, 0.972, title, color=TXT, fontsize=17, weight="bold", va="center")
    chips = ["proxy: All", "machine: All", "model: All", "Last 7 days"]
    for x, c in zip([0.575, 0.675, 0.785, 0.885], chips):
        fig.text(x, 0.972, f" {c} ", color=TXT, fontsize=9.5, va="center",
                 bbox=dict(boxstyle="round,pad=0.35", fc="#22252b", ec=EDGE))
    fig.text(0.985, 0.012, "FICTIVE DATA  -  layout only, not a result", color=ORG, fontsize=10,
             ha="right", va="bottom", weight="bold")
    return fig


def panel(fig, rect, title, sub=None):
    ax = fig.add_axes(rect, facecolor=PANEL)
    for s in ax.spines.values():
        s.set_color(EDGE)
    ax.tick_params(colors=DIM, labelsize=8, length=0)
    ax.set_title(title, color=TXT, fontsize=10.5, loc="left", pad=7)
    if sub:
        ax.text(1, 1.035, sub, color=DIM, fontsize=8, ha="right", transform=ax.transAxes)
    ax.grid(axis="y", color="#262a30", lw=0.8)
    ax.set_axisbelow(True)
    return ax


def stat(fig, rect, title, value, color=TXT, note=None, note_color=DIM, big=False):
    ax = fig.add_axes(rect, facecolor=PANEL)
    ax.set_xticks([]); ax.set_yticks([])
    for s in ax.spines.values():
        s.set_color(EDGE)
    ax.text(0.04, 0.88, title, color=DIM, fontsize=10, va="top", transform=ax.transAxes)
    ax.text(0.04, 0.34, value, color=color, fontsize=40 if big else 28, weight="bold", va="center",
            transform=ax.transAxes)
    if note:
        ax.text(0.04, 0.08, note, color=note_color, fontsize=9, transform=ax.transAxes)
    return ax


def hours(ax, n):
    ax.set_xticks(np.linspace(0, n - 1, 7))
    ax.set_xticklabels(["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"][:7], color=DIM, fontsize=8)


def usd(v):
    return f"-${abs(v):,.0f}" if v < 0 else f"${v:,.0f}"


def bars_h(ax, names, vals, colors=None, fmt=usd):
    y = np.arange(len(names))[::-1]
    colors = colors or [GREEN if v >= 0 else RED for v in vals]
    ax.barh(y, vals, color=colors, height=0.62)
    ax.set_yticks([])
    ax.axvline(0, color=EDGE, lw=1)
    lo, hi = min(0, min(vals)), max(0, max(vals))
    span = hi - lo
    # names sit inside the panel, in a column on the left; the bars start at 30% of the width
    ax.set_xlim(lo - span * 0.45, hi + span * 0.2)
    for yi, n, v in zip(y, names, vals):
        ax.text(0.02, yi, n, color=TXT, fontsize=9, va="center", transform=ax.get_yaxis_transform())
        ax.text(max(v, 0) + span * 0.02, yi, fmt(v), color=RED if v < 0 else TXT, fontsize=8.5, va="center")
    ax.grid(axis="y", visible=False)
    ax.grid(axis="x", visible=False)
    ax.set_ylim(-0.6, len(names) - 0.4)
    ax.tick_params(axis="x", labelbottom=False)


def save(fig, name):
    os.makedirs(OUT, exist_ok=True)
    fig.savefig(os.path.join(OUT, name), facecolor=BG)
    plt.close(fig)


def fleet():
    fig = canvas("Fleet overview", h=10.6)
    # Row 1: money first
    stat(fig, [0.015, 0.785, 0.30, 0.14], "NET SAVED TODAY", "$ 41.20", GREEN, "+12% vs yesterday",
         GREEN, big=True)
    stat(fig, [0.325, 0.785, 0.20, 0.14], "Gross saved today", "$ 58.90", TXT, "saved_usd")
    stat(fig, [0.535, 0.785, 0.20, 0.14], "Added cost today", "$ 17.70", ORG, "added_usd (cost of our changes)")
    stat(fig, [0.745, 0.785, 0.24, 0.14], "Spend today", "$ 212.40", TXT, "what the providers billed")
    # Row 2: net saved per day
    ax = panel(fig, [0.04, 0.555, 0.945, 0.19], "Net saved per day", "last 30 days")
    d = np.clip(np.linspace(8, 40, 30) + rng.normal(0, 5, 30), 2, None)
    d[[5, 6, 12, 13, 19, 20, 26, 27]] *= 0.35  # weekends
    ax.bar(range(30), d, color=GREEN, width=0.7)
    ax.bar(29, d[-1], color="#9be08f", width=0.7)
    ax.set_xticks([0, 7, 14, 21, 29]); ax.set_xticklabels(["Oct 13", "Oct 20", "Oct 27", "Nov 3", "Nov 11"],
                                                        color=DIM, fontsize=8)
    ax.set_yticks([0, 20, 40]); ax.set_yticklabels(["$0", "$20", "$40"], color=DIM)
    # Row 3
    ax = panel(fig, [0.04, 0.265, 0.445, 0.215], "Saved vs added", "USD per hour")
    t = np.arange(168)
    day = 0.5 + 0.5 * np.sin((t % 24 - 9) / 24 * 2 * np.pi)
    saved = (2.8 * np.clip(day, 0, None) + rng.normal(0, 0.15, 168)).clip(0)
    added = (0.9 * np.clip(day, 0, None) + rng.normal(0, 0.1, 168)).clip(0)
    ax.fill_between(t, saved, color=GREEN, alpha=0.35); ax.plot(t, saved, color=GREEN, lw=1.4, label="saved")
    ax.fill_between(t, added, color=ORG, alpha=0.35); ax.plot(t, added, color=ORG, lw=1.4, label="added")
    hours(ax, 168)
    ax.legend(loc="upper left", fontsize=8, frameon=False, labelcolor=TXT, ncol=2)
    ax = panel(fig, [0.53, 0.265, 0.455, 0.215], "Net saved by machine", "top 8, last 7 days")
    names = [f"m-{h}" for h in ["1c4e", "9a02", "e7d1", "40bb", "7f3c", "b218", "2d96", "c05a"]]
    bars_h(ax, names, [212, 188, 160, 131, 96, 71, 38, -12])
    # Row 4: health strip
    xs = [0.015, 0.215, 0.415, 0.615, 0.815]
    cells = [("Machines reporting", "23", TXT, "of 24 seen this week"),
             ("Silent machines", "1", YEL, "no push for 20 min"),
             ("5xx error rate", "0.4%", GREEN, "threshold 2%"),
             ("Requests per minute", "118", TXT, "cortex 71 / context-guru 47"),
             ("Alerts firing", "0", GREEN, "none")]
    for x, (a, b, c, e) in zip(xs, cells):
        stat(fig, [x, 0.04, 0.17, 0.17], a, b, c, e)
    save(fig, "dash-fleet.png")


def savings():
    fig = canvas("Savings", h=11.4)
    ax = panel(fig, [0.015, 0.70, 0.34, 0.225], "Net by component", "USD, last 7 days")
    comps = ["extract", "dedup", "toon", "searchfold", "cmdfilter", "textclean", "toolfilter", "cachesplit",
             "compact", "mask"]
    vals = [412, 368, 301, 240, 177, 122, 64, 18, -41, -96]
    bars_h(ax, comps, vals)
    ax = panel(fig, [0.385, 0.70, 0.60, 0.225], "Net by kind", "USD per hour, stacked")
    t = np.arange(168); day = np.clip(0.5 + 0.5 * np.sin((t % 24 - 9) / 24 * 2 * np.pi), 0, None)
    rew = 2.0 * day + 0.1; spe = -0.45 * day; stt = 0.5 * day
    ax.stackplot(t, rew, stt, colors=[GREEN, BLUE], labels=["rewrite", "setting"], alpha=0.8)
    ax.fill_between(t, spe, color=RED, alpha=0.7, label="spend")
    ax.axhline(0, color=EDGE, lw=1); hours(ax, 168)
    ax.legend(loc="upper left", fontsize=8, frameon=False, labelcolor=TXT, ncol=3)
    # row 2
    ax = panel(fig, [0.04, 0.375, 0.275, 0.25], "Saved tokens by tier", "millions, last 7 days")
    tiers = ["cache_read", "input", "cache_write_5m", "cache_write_1h"]
    ax.bar(range(4), [38.2, 9.7, 3.1, 1.4], color=[TEAL, BLUE, PUR, ORG], width=0.6)
    ax.set_xticks(range(4)); ax.set_xticklabels(tiers, color=TXT, fontsize=8)
    ax = panel(fig, [0.345, 0.375, 0.32, 0.25], "Cache effects", "USD per day, stacked")
    days = np.arange(7)
    ef = {"rescue": ([6, 7, 9, 8, 10, 4, 3], GREEN), "break": ([-4, -5, -3, -6, -4, -1, -1], RED),
          "split": ([-1, -1, -2, -1, -1, 0, 0], ORG), "ttl_premium": ([-2, -2, -3, -2, -2, -1, -1], PUR)}
    for k, (v, c) in ef.items():
        ax.bar(days, v, color=c, width=0.6, label=k)
    ax.axhline(0, color=EDGE, lw=1)
    ax.set_xticks(days); ax.set_xticklabels(["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"], color=DIM, fontsize=8)
    ax.set_ylim(-8, 15)
    ax.legend(fontsize=7.5, frameon=False, labelcolor=TXT, ncol=4, loc="upper center")
    ax = panel(fig, [0.695, 0.375, 0.29, 0.25], "Break cost by cause", "USD, last 7 days")
    bars_h(ax, ["ended_change", "model_changed", "none"], [-23, -9, -4], colors=[RED] * 3)
    # row 3
    stat(fig, [0.015, 0.045, 0.17, 0.27], "Estimated share", "3.2%", YEL, "figures priced with a fallback rate")
    ax = panel(fig, [0.21, 0.045, 0.355, 0.27], "Unpriced requests", "per hour, by reason")
    for name, c, lvl in [("penalty_unknown", ORG, 2.2), ("operator_not_allowed", PUR, 0.8)]:
        ax.plot(np.arange(48), (lvl * (1 + 0.4 * rng.standard_normal(48))).clip(0), color=c, lw=1.4, label=name)
    ax.legend(fontsize=8, frameon=False, labelcolor=TXT, loc="upper left")
    ax.set_xticks([])
    ax = fig.add_axes([0.595, 0.045, 0.39, 0.27], facecolor=PANEL)
    ax.set_xticks([]); ax.set_yticks([])
    for s in ax.spines.values():
        s.set_color(EDGE)
    ax.set_title("Components with a negative net", color=TXT, fontsize=10.5, loc="left", pad=7)
    rows = [("component", "saved", "added", "net"), ("compact", "$57", "$98", "-$41"), ("mask", "$31", "$127", "-$96")]
    for i, r in enumerate(rows):
        yy = 0.78 - i * 0.2
        for x, cell in zip([0.04, 0.45, 0.65, 0.84], r):
            col = DIM if i == 0 else (RED if x == 0.84 else TXT)
            ax.text(x, yy, cell, color=col, fontsize=10, transform=ax.transAxes, weight="bold" if i == 0 else None)
        ax.axhline(yy - 0.09, color=EDGE, lw=0.8) if False else None
    ax.text(0.04, 0.10, "Read this table first: a negative component costs more than it saves.",
            color=DIM, fontsize=8.5, transform=ax.transAxes)
    save(fig, "dash-savings.png")


def cache():
    fig = canvas("Cache and keep-alive", h=10.4)
    ax = panel(fig, [0.04, 0.70, 0.445, 0.225], "Cache read ratio", "cache_read / all input tokens, by model")
    t = np.arange(168)
    for name, base, c in [("claude-sonnet-5-5", 0.86, BLUE), ("claude-opus-5-5", 0.79, PUR), ("claude-haiku-5-5", 0.64, TEAL)]:
        ax.plot(t, (base + 0.05 * np.sin(t / 9) + rng.normal(0, 0.012, 168)).clip(0, 1), color=c, lw=1.4, label=name)
    ax.set_ylim(0.4, 1); ax.set_yticks([0.5, 0.75, 1.0]); ax.set_yticklabels(["50%", "75%", "100%"], color=DIM)
    hours(ax, 168); ax.legend(fontsize=8, frameon=False, labelcolor=TXT, loc="lower left", ncol=3)
    ax = panel(fig, [0.515, 0.70, 0.47, 0.225], "Misses by cause", "per hour, stacked")
    causes = [("ttl_expiry", ORG, 5.0), ("prefix_change", PUR, 2.4), ("cold_start", BLUE, 1.6),
              ("model_changed", RED, 0.9), ("other", DIM, 0.5)]
    bottom = np.zeros(48)
    for n, c, lvl in causes:
        v = (lvl * (1 + 0.45 * rng.standard_normal(48))).clip(0)
        ax.bar(range(48), v, bottom=bottom, color=c, width=0.85, label=n); bottom += v
    ax.set_xticks([]); ax.legend(fontsize=7.5, frameon=False, labelcolor=TXT, ncol=5, loc="upper center")
    ax.set_ylim(0, bottom.max() * 1.35)
    ax = panel(fig, [0.015, 0.375, 0.30, 0.25], "Miss cost by cause", "USD, last 7 days")
    bars_h(ax, ["ttl_expiry", "prefix_change", "cold_start", "model_changed", "other"], [64, 38, 21, 17, 6],
           colors=[ORG, PUR, BLUE, RED, DIM])
    ax = panel(fig, [0.345, 0.375, 0.34, 0.25], "Ping outcomes", "per hour, stacked")
    bottom = np.zeros(48)
    for n, c, lvl in [("ok", GREEN, 14), ("skipped", BLUE, 4), ("failed", RED, 0.6), ("timeout", ORG, 0.4)]:
        v = (lvl * (1 + 0.25 * rng.standard_normal(48))).clip(0)
        ax.bar(range(48), v, bottom=bottom, color=c, width=0.85, label=n); bottom += v
    ax.set_xticks([]); ax.legend(fontsize=8, frameon=False, labelcolor=TXT, ncol=4, loc="upper center")
    ax.set_ylim(0, bottom.max() * 1.3)
    stat(fig, [0.715, 0.375, 0.27, 0.25], "Pings that paid", "3.4 x", GREEN, "rescue value / ping cost", GREEN, big=True)
    ax = panel(fig, [0.04, 0.045, 0.595, 0.27], "Keep-alive: cost, rescue and net", "USD per hour")
    t = np.arange(168); day = np.clip(0.5 + 0.5 * np.sin((t % 24 - 9) / 24 * 2 * np.pi), 0, None)
    cost = 0.35 * day + 0.02; resc = 1.2 * day * (1 + 0.25 * rng.standard_normal(168)).clip(0.3); net = resc - cost
    ax.plot(t, cost, color=ORG, lw=1.4, label="ping cost"); ax.plot(t, resc, color=GREEN, lw=1.4, label="rescue")
    ax.plot(t, net, color=BLUE, lw=1.8, label="net"); hours(ax, 168)
    ax.legend(fontsize=8, frameon=False, labelcolor=TXT, loc="upper left", ncol=3)
    ax = fig.add_axes([0.665, 0.045, 0.32, 0.27], facecolor=PANEL)
    ax.set_xticks([]); ax.set_yticks([])
    for s in ax.spines.values():
        s.set_color(EDGE); s.set_linestyle("--")
    ax.set_title("Shared-warm tokens", color=TXT, fontsize=10.5, loc="left", pad=7)
    ax.text(0.5, 0.5, "No data yet\nneeds #853", color=DIM, fontsize=12, ha="center", va="center", transform=ax.transAxes)
    save(fig, "dash-cache.png")


def machines():
    fig = canvas("Machine comparison", h=9.4)
    ax = panel(fig, [0.015, 0.52, 0.40, 0.40], "Net saved by machine", "USD, last 30 days, sorted")
    names = [f"m-{h}" for h in ["1c4e", "9a02", "e7d1", "40bb", "7f3c", "b218", "2d96", "c05a", "5e71", "a9f4", "03dd", "d6b7"]]
    bars_h(ax, names, [892, 801, 655, 544, 410, 331, 247, 160, 98, 44, 11, -37])
    ax = fig.add_axes([0.445, 0.52, 0.54, 0.40], facecolor=PANEL)
    ax.set_xticks([]); ax.set_yticks([])
    for s in ax.spines.values():
        s.set_color(EDGE)
    ax.set_title("Saving rate by preset", color=TXT, fontsize=10.5, loc="left", pad=7)
    head = ("preset", "machines", "net saved", "saving rate", "estimated")
    data = [("house", "14", "$ 3,210", "19.4 %", "2.8 %"), ("lean", "6", "$ 1,090", "14.1 %", "3.0 %"),
            ("max", "3", "$ 720", "23.7 %", "6.9 %"), ("custom", "1", "$ 38", "5.2 %", "11.0 %")]
    xs = [0.04, 0.27, 0.45, 0.66, 0.84]
    for x, h in zip(xs, head):
        ax.text(x, 0.84, h, color=DIM, fontsize=10, weight="bold", transform=ax.transAxes)
    for i, row in enumerate(data):
        for x, cell in zip(xs, row):
            ax.text(x, 0.68 - i * 0.14, cell, color=YEL if (x == 0.84 and float(cell.split()[0]) > 10) else TXT,
                    fontsize=11, transform=ax.transAxes)
    ax.text(0.04, 0.06, "Saving rate = net saved / baseline cost. It needs the baseline counter (not built yet).",
            color=DIM, fontsize=8.5, transform=ax.transAxes)
    ax = panel(fig, [0.045, 0.06, 0.37, 0.35], "Net saved by proxy", "USD, last 30 days")
    ax.bar([0, 1], [3520, 1538], color=[BLUE, TEAL], width=0.5)
    ax.set_xticks([0, 1]); ax.set_xticklabels(["cortex", "context-guru"], color=TXT, fontsize=10)
    for x, v in zip([0, 1], [3520, 1538]):
        ax.text(x, v + 80, f"${v:,}", color=TXT, ha="center", fontsize=10)
    ax.set_ylim(0, 4200)
    ax = panel(fig, [0.445, 0.06, 0.54, 0.35], "Net saved per 1,000 requests", "USD, by machine")
    vals = np.sort(rng.normal(0.41, 0.13, 23).clip(0.02))[::-1]
    ax.bar(range(23), vals, color=GREEN, width=0.7)
    ax.axhline(vals.mean(), color=YEL, lw=1, ls="--"); ax.text(22.4, vals.mean() + 0.015, "fleet mean", color=YEL, fontsize=8, ha="right")
    ax.set_xticks([])
    save(fig, "dash-machines.png")


if __name__ == "__main__":
    fleet(); savings(); cache(); machines()
    print("wrote", sorted(f for f in os.listdir(OUT) if f.startswith("dash-")))
