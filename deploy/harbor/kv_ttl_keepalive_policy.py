#!/usr/bin/env python3
"""How MANY keep-alive pings a conversation is worth, decided per conversation.

This is the learned counterpart of `kvcache.BudgetPolicy`, and the two must agree: the Go side
is what a replay is scored by, this side is where the fit lives, and
`kv_ttl_keepalive_drift_test.go` drives the `--fixture` entry point below to pin them together.
When they disagree, Go is right. (`BudgetPolicy` is not in `kvcache.Registry()`, so it reaches a
replay the way `Custom`'s predictor does: from an in-process caller, because "a predictor is
code, not a query parameter". No dashboard page renders it.)

    kv_ttl_keepalive_policy.py --fixture f.json      # the drift test's interface, stdlib only
    kv_ttl_keepalive_policy.py --self-test           # asserts this module's claims, stdlib only

There is deliberately no `--db` yet, unlike kv_ttl_cost_model.py and kv_ttl_predictor_arms.py:
`fit()` and `person_periods()` below are the fit AS IT WAS RUN, kept so the method is
reviewable, but they are not wired to an entry point — so the figures quoted in this docstring
cannot be reproduced from this file alone. Wiring `--db`/`--prices` the way the siblings do,
and reusing kv_ttl_survival_predictor's person-period expansion instead of the copy below, is
the next thing to do here.

WHAT PROBLEM THIS SOLVES THAT THE SURVIVAL PREDICTOR DOES NOT
-------------------------------------------------------------
`kv_ttl_survival_predictor.py` answers "will this conversation come back within 5 minutes?"
from the instant a request is served. That is the right question for choosing a TTL TIER.
It is the wrong question for a keep-alive, for two reasons this module exists to fix.

(1) THE POPULATION. A keep-alive fires only after the conversation has ALREADY been idle
    for one ping interval. So the decision is never faced by the 92.5% of spans that close
    inside five minutes — it is faced only by the survivors. Conditioning on that changes
    the answer enormously: on this deployment's corpus the unconditional 5-minute return
    rate is ~92%, while the stake-weighted hazard among spans that reached 270s idle is
    23.17%. A model fitted on the unconditional population and then asked at the ping
    instant is answering a question nobody asks.

    Concretely: `elapsed silence` is NOT a feature here. It is the SAMPLE DEFINITION. A row
    exists in the training frame because it reached that sweep.

(2) THE DECISION. "Ping or not" is not the decision either; "how many" is. And the pings
    are not independent — a ping keeps the entry alive so the NEXT ping is a cheap read
    rather than a 12.5x re-creation, which means the value of ping j depends on what ping
    j+1 can still buy. That is a backward induction, not a threshold.

THE ECONOMICS, WHICH IS THE PART TO ARGUE WITH FIRST
----------------------------------------------------
Per cached token, at the documented Anthropic multiples of base input:

    cache read   0.10x      a keep-alive ping is one of these
    cache write  1.25x      what the successor pays if the entry lapsed
    rescue       1.15x      = 1.25 - 0.10, what a successful ping avoids

So a ping pays for itself when the chance it rescues exceeds 0.10/1.15 = 8.70%. The model's
per-token rate cancels, and so does the PER-TOKEN part of the prefix: it multiplies cost and
benefit alike. That is why this module reads probabilities and not token counts, and why the
threshold is computed from the price list rather than written down — a deployment on different
rates has a different threshold, and one with no rates has none.

What does NOT cancel is `Rates.ping_cost`'s fixed ping_input/ping_output term, so 8.70% is the
gate only in the limit:

    gate(prefix) = 0.0870 + (ping_input*input + ping_output*output)
                            / (prefix * (write_5m - cache_read))

8.70% at 125k tokens, 8.72% at 20k, 8.96% at 2k, 9.74% at 500 — a spread of the same order as
the margins below, which is what `min_prefix` is for.

That 8.70% is the MYOPIC bar and it is too high. With the option value counted the
effective bar at the first sweep falls to ~7.4%, worth ~6% of the policy's net.

WHAT IT IS WORTH, INCLUDING WHERE IT LOSES
------------------------------------------
Measured on the hosted deployment's capture: 34,577 idle spans over 16.9 days, 5-fold
rolling-origin, pings at 280s. Given as a share of the window's bill and of what `optimal`
reaches, with per-ping efficiency indexed to flat MaxPings=6.

    arm                     pings   off the bill   of the ceiling   net per ping
    optimal (unreachable)   5,826         11.72%           100.0%          27.7x
    flat MaxPings=6        95,640          6.96%            59.4%           1.0x
    flat MaxPings=5        80,404          6.91%            58.9%           1.2x
    THIS POLICY             9,248          6.07%            51.8%           9.0x
    flat MaxPings=2 (dflt) 33,591          5.19%            44.3%           2.1x

Read the last two columns together, because they say opposite things. On MONEY a constant
beats this policy by 0.89 pp of the bill: it ranks conversations well (AUC 0.93) and DOLLARS barely
better than chance (AUC 0.70, and 0.57 on the largest prefixes, which carry 87% of the
stake), so its mistakes land where they cost most, and at a 11.5:1 payoff a wrong skip is
expensive. Per PING it is 9x better. So:

    pings effectively free   ->  raise MaxPings and do not deploy a model
    pings rate-limited      ->  this policy, by a wide margin

Two things were measured and did NOT work, recorded here so they are not re-tried blind:
threshold tuning (the best threshold chosen in HINDSIGHT is worth +0.38 pp overall and
-0.05 pp on the top 5% of prefixes, so there is no headroom), and four families of extra
feature (calendar, day-of-week, last-human-pause, trajectory gap history), every one of which
came in under the +-0.42 pp seed-noise floor of this corpus. See
docs/how-to/kv-cache-keepalive-budget.md.
"""

from __future__ import annotations

import argparse
import json
import sys
from dataclasses import dataclass, field

# The documented Anthropic multiples, as named constants rather than literals. kvcache's
# pricing.go carries the same three.
CACHE_READ_MULTIPLE = 0.10
WRITE_5M_MULTIPLE = 1.25
WRITE_1H_MULTIPLE = 2.00

DEFAULT_INTERVAL_S = 280.0
DEFAULT_LIFE_S = 300.0
DEFAULT_MAX_K = 8


# ── the arithmetic the Go side must match, byte for byte ─────────────────────
#
# Everything in this block is stdlib-only and pure, because it is what the drift test
# drives. No numpy, no pandas, no sklearn: a guard that cannot run in a plain CI container
# is an absent guard.


def break_even(read_rate: float, write_rate: float) -> float:
    """The reuse probability at which one ping pays for itself.

    read/(write-read). Returns 1.0 — "never worth it" — where the rates make a rescue worth
    nothing, rather than dividing by zero and reporting a threshold of infinity.
    """
    rescue = write_rate - read_rate
    if rescue <= 0:
        return 1.0
    return read_rate / rescue


def windows(cdf, interval_s: float = DEFAULT_INTERVAL_S, life_s: float = DEFAULT_LIFE_S,
            max_k: int = DEFAULT_MAX_K) -> tuple[list[float], list[float]]:
    """Turn a CUMULATIVE return-time distribution into the per-sweep conditional pair.

    `cdf(seconds) -> P(the conversation has returned by then)`, measured from the request
    that opened the idle span.

    Ping j (1-based) fires at j*interval and refreshes the entry to j*interval+life, so the
    window it and it alone protects is (deadline standing before it, j*interval + life],
    where that deadline is `life` for the first ping and (j-1)*interval+life after. Then

        h_j = (F(t_j + life) - F(d_j)) / (1 - F(t_j))    this ping rescues
        s_j = (1 - F(t_(j+1)))        / (1 - F(t_j))     reaches the next sweep

    Dividing by the survivor share is the whole point (see (1) in the module docstring): the
    decision at sweep j is only ever faced by a conversation that already stayed idle that
    long. Values are clamped to [0, 1] rather than trusted, because a non-monotone fitted
    CDF would otherwise produce a negative hazard and a budget with no meaning.
    """
    # max_k <= 0 means the default, because kvcache.BudgetPolicy.maxK() reads it that way and
    # a port that read it as "no windows at all" would disagree with Go on a value the drift
    # fixture never sends. Same convention as interval/life below.
    if max_k <= 0:
        max_k = DEFAULT_MAX_K
    h, s = [0.0] * max_k, [0.0] * max_k
    for j in range(1, max_k + 1):
        t_j = j * interval_s
        survive = 1.0 - cdf(t_j)
        if survive <= 0.0:
            # Certain to be back by now, so no later ping can be needed. The zeros say it.
            break
        deadline = life_s if j == 1 else (j - 1) * interval_s + life_s
        h[j - 1] = min(max((cdf(t_j + life_s) - cdf(deadline)) / survive, 0.0), 1.0)
        s[j - 1] = min(max((1.0 - cdf(t_j + interval_s)) / survive, 0.0), 1.0)
    return h, s


def ping_budget(h: list[float], s: list[float], rescue: float, ping: float) -> int:
    """How many pings to buy, by backward induction over the windows they protect.

        V_j = max(0, rescue*h_j - ping + s_j*V_(j+1)),   V_maxk = 0
        budget = the FIRST j at which V_j is not positive

    The FIRST, not the last. With a non-monotone hazard the last positive V buys a run of
    pings across a stretch that pays nothing in order to reach one that does — which the
    induction already accounts for when it IS worth it, by propagating that value backwards.
    Taking the last positive one double-counts it.
    """
    n = min(len(h), len(s))
    v = [0.0] * (n + 1)
    for j in range(n - 1, -1, -1):
        v[j] = max(0.0, rescue * h[j] - ping + s[j] * v[j + 1])
    budget = 0
    for j in range(n):
        if v[j] <= 0.0:
            break
        budget = j + 1
    return budget


@dataclass
class Rates:
    """The three per-token rates a keep-alive decision needs, plus a ping's fixed overhead.

    `ping_input`/`ping_output` are the tokens a ping cannot avoid — the fresh input past the
    cached prefix, and the smallest generation the provider accepts. They do NOT scale with
    the prefix, which is why a small enough entry is not worth pinging at any probability.
    kvcache.Pricing.KeepAliveCost is the same expression.
    """

    input: float
    output: float
    cache_read: float
    write_5m: float
    ping_input_tokens: int = 1
    ping_output_tokens: int = 1
    known: bool = True

    @classmethod
    def from_input_rate(cls, input_rate: float) -> "Rates":
        """The documented multiples off a base input rate. The common case."""
        return cls(input=input_rate, output=input_rate * 5.0,
                   cache_read=input_rate * CACHE_READ_MULTIPLE,
                   write_5m=input_rate * WRITE_5M_MULTIPLE)

    def rescue_value(self, cached_tokens: int) -> float:
        """What one successful rescue avoids: the successor's write becomes a read."""
        return cached_tokens * (self.write_5m - self.cache_read)

    def ping_cost(self, cached_tokens: int) -> float:
        return (cached_tokens * self.cache_read
                + self.ping_input_tokens * self.input
                + self.ping_output_tokens * self.output)


def budget_for(cdf, cached_tokens: int, rates: Rates, *,
               interval_s: float = DEFAULT_INTERVAL_S, life_s: float = DEFAULT_LIFE_S,
               max_k: int = DEFAULT_MAX_K, min_prefix: int = 0) -> int:
    """The whole decision for one conversation. `kvcache.BudgetPolicy.PingBudget` in Python.

    Returns 0 both for "not worth a ping" and for the cases where no decision can be made
    (no rates, no prefix). The Go side distinguishes those with an `ok` flag so it can fall
    back to the configured cap; here they coincide because there is no cap to fall back to.
    """
    if not rates.known or cached_tokens <= 0:
        return 0
    if min_prefix > 0 and cached_tokens < min_prefix:
        return 0
    h, s = windows(cdf, interval_s=interval_s, life_s=life_s, max_k=max_k)
    return ping_budget(h, s, rates.rescue_value(cached_tokens), rates.ping_cost(cached_tokens))


# ── the fit ──────────────────────────────────────────────────────────────────
#
# numpy/pandas/sklearn are imported INSIDE these functions, so that everything above stays
# importable in a container that has none of them.


SPAN_CAT = ["user_id", "model", "cache_ttl"]
SPAN_NUM = ["log_prefix", "log_prev_gap", "turn"]
STATE_NUM = ["sweep_k", "log_elapsed", "log_secs_to_deadline", "hour_utc", "dow_utc"]
STATS_NUM = ["stat_p", "stat_n"]

#: The 13 features, and the reason the list is exactly this long.
#:
#: Every one is carried by `kvcache.Observation` — User, Model, TTL, CachedTokens,
#: SinceLastMs, Turn, Now, ExpiresAt — or derived from `Observation.Stats`, the leak-free
#: accumulator the simulator already advances as gaps close. That is a hard constraint, not a
#: preference: a model fitted on anything else cannot be wired in behind
#: `kvcache.Predictor`, which is the whole point of the seam.
#:
#: It costs almost nothing. Measured 3-seed means on this corpus: the unrestricted 21-feature
#: model takes 6.11% off the window's bill, these 13 take 6.07% (-0.04 pp, inside the noise
#: floor), and dropping the two Stats features to leave only what Observation carries directly
#: costs 0.40 pp. So
#: the historical accumulator is doing most of the work the excluded request-shape features
#: would have done.
FEATURES = SPAN_CAT + SPAN_NUM + STATE_NUM + STATS_NUM


@dataclass
class FitResult:
    """Two fitted models and the frame they were fitted on."""

    # `cdf_for` below composes the SURVIVAL model only, so `hazard` is currently fitted and
    # never read: the h_j the policy uses is derived from the survival curve by windows(), not
    # from this. Kept because y_hazard is what person_periods() labels and a direct-hazard
    # variant is the obvious next thing to measure — but it is not what runs.
    hazard: object          # P(returns in the window this ping protects | idle at t_k)
    survival: object        # P(still idle at t_(k+1)            | idle at t_k)
    n_rows: int
    n_spans: int
    notes: dict = field(default_factory=dict)

    def cdf_for(self, span_row, *, interval_s: float, life_s: float, max_k: int):
        """A CDF closure for one span, so `budget_for` can be used unchanged.

        Reconstructed from the SURVIVAL model rather than fitted directly, because it answers
        a conditional question and `windows()` wants a cumulative one. Composing forwards is
        exact: F(t_(j+1)) = 1 - prod_(i<=j) s_i.

        Two caveats, because the drift guard covers NEITHER — it drives `--fixture`, which
        feeds a step CDF and never reaches this function:

        - F(t_1) is 0 by construction, i.e. this CDF is already conditioned on reaching the
          first sweep, and `windows()` divides by 1 - F(t_1) = 1 accordingly.
        - the grid is {t_j} u {t_j + life}, so F(deadline) for the FIRST sweep — `life`, which
          falls between t_1 and t_2 — is a linear interpolation across a whole interval rather
          than a fitted value. On the shipped 280 s / 300 s that reads F(300) at
          (300-280)/(560-280) = 7.1% of the way into the first survival step.
        """
        import numpy as np

        surv, cum = 1.0, {0.0: 0.0}
        for j in range(1, max_k + 2):
            t_j = j * interval_s
            frame = _state_frame(span_row, j, interval_s, life_s)
            s_j = float(self.survival.predict_proba(frame)[:, 1][0])
            cum[t_j] = 1.0 - surv
            surv *= s_j
            cum[t_j + life_s] = 1.0 - surv
        keys = np.array(sorted(cum))
        vals = np.array([cum[k] for k in keys])

        def cdf(seconds: float) -> float:
            return float(np.interp(seconds, keys, vals))

        return cdf


def _state_frame(span_row, k: int, interval_s: float, life_s: float):
    """One span's features AS SEEN AT SWEEP k. Only the state block moves."""
    import numpy as np
    import pandas as pd

    f = pd.DataFrame([{c: span_row[c] for c in SPAN_CAT + SPAN_NUM + STATS_NUM}])
    t_k = k * interval_s
    deadline = life_s if k == 1 else (k - 1) * interval_s + life_s
    f["sweep_k"] = k
    f["log_elapsed"] = np.log1p(t_k)
    f["log_secs_to_deadline"] = np.log1p(max(deadline - t_k, 0.0))
    secs = span_row["ts_ms"] / 1000.0 + t_k
    f["hour_utc"] = int(secs // 3600 % 24)
    f["dow_utc"] = int((secs // 86400 + 4) % 7)
    return f[FEATURES]


def person_periods(spans, interval_s: float, life_s: float, max_k: int):
    """One row per (span, sweep it ACTUALLY reached), with both targets attached.

    This layout is the module's central claim. Every row in it is a decision the policy will
    really face, which is exactly what a frame built from all requests is not.

    `spans` needs: gap_s (seconds to the next cache-compatible request, inf if none), ts_ms,
    stake, and the feature columns.
    """
    import numpy as np
    import pandas as pd

    out = []
    for k in range(1, max_k + 1):
        t_k = k * interval_s
        reached = spans[spans.gap_s > t_k]
        if reached.empty:
            break
        rows = pd.concat([_state_frame(r, k, interval_s, life_s)
                          for _, r in reached.iterrows()], ignore_index=True)
        deadline = life_s if k == 1 else (k - 1) * interval_s + life_s
        g = reached.gap_s.to_numpy()
        rows["y_hazard"] = ((g > deadline) & (g <= t_k + life_s)).astype(int)
        nxt = t_k + interval_s if k < max_k else np.inf
        rows["y_survival"] = (g > nxt).astype(int)
        rows["stake"] = reached.stake.to_numpy()
        out.append(rows)
    return pd.concat(out, ignore_index=True)


def fit(spans, *, interval_s: float = DEFAULT_INTERVAL_S, life_s: float = DEFAULT_LIFE_S,
        max_k: int = DEFAULT_MAX_K, seed: int = 0) -> FitResult:
    """Fit the two models on the person-period frame, weighted by DOLLARS AT RISK.

    The weight is the point. An unweighted fit optimises row accuracy, and rows are not what
    the bill is made of: this corpus's stake is concentrated so hard that the model can reach
    AUC 0.93 by row while ranking dollars at 0.70. Weighting by stake does not close that gap
    — nothing measured here does — but fitting without it makes it worse.
    """
    from sklearn.compose import ColumnTransformer
    from sklearn.ensemble import HistGradientBoostingClassifier
    from sklearn.pipeline import Pipeline
    from sklearn.preprocessing import OneHotEncoder

    pp = person_periods(spans, interval_s, life_s, max_k)
    w = pp.stake.clip(lower=0)
    weights = (1e-9 + w / w.mean()).to_numpy()

    def build():
        return Pipeline([
            ("prep", ColumnTransformer([
                ("cat", OneHotEncoder(handle_unknown="ignore", min_frequency=20,
                                      sparse_output=False), SPAN_CAT),
                ("num", "passthrough", SPAN_NUM + STATE_NUM + STATS_NUM)])),
            # max_iter is a ceiling, not a target: early stopping settles at ~37 trees on this
            # corpus, which is the model telling us how much signal it found.
            ("gbm", HistGradientBoostingClassifier(max_iter=200, learning_rate=0.08,
                                                   random_state=seed))])

    hz, sv = build(), build()
    hz.fit(pp[FEATURES], pp.y_hazard, gbm__sample_weight=weights)
    sv.fit(pp[FEATURES], pp.y_survival, gbm__sample_weight=weights)
    return FitResult(hazard=hz, survival=sv, n_rows=len(pp), n_spans=len(spans),
                     notes={"interval_s": interval_s, "life_s": life_s, "max_k": max_k,
                            "seed": seed})


# ── the drift test's interface ───────────────────────────────────────────────


def _run_fixture(path: str) -> int:
    """Score a fixture and print JSON. Stdlib only, so the guard runs anywhere.

    A fixture is {"interval_s":…, "life_s":…, "max_k":…, "cases":[{
        "cached_tokens":…, "input_rate":…, "cdf":[[seconds, p], …],
        "min_prefix":… (optional)}]}

    and the output is {"break_even":[…], "budgets":[…], "windows":[{"h":[…],"s":[…]}, …]} —
    one break_even PER CASE, not one for the fixture, because each case carries its own
    input_rate and a fixture that mixed rate shapes would otherwise report only the last
    case's — so the Go test can compare the decision AND the two intermediate vectors, per
    case, and a budget that agrees for the wrong reason is a guard that will stop catching
    things.
    """
    with open(path, encoding="utf-8") as fh:
        spec = json.load(fh)
    interval = float(spec.get("interval_s", DEFAULT_INTERVAL_S))
    life = float(spec.get("life_s", DEFAULT_LIFE_S))
    max_k = int(spec.get("max_k", DEFAULT_MAX_K))
    break_evens, budgets, wins = [], [], []
    for case in spec["cases"]:
        rates = Rates.from_input_rate(float(case["input_rate"]))
        break_evens.append(break_even(rates.cache_read, rates.write_5m))
        points = sorted((float(a), float(b)) for a, b in case["cdf"])

        def cdf(seconds: float, _pts=points) -> float:
            p = 0.0
            for at, val in _pts:
                if seconds >= at:
                    p = val
            return p

        h, s = windows(cdf, interval_s=interval, life_s=life, max_k=max_k)
        wins.append({"h": h, "s": s})
        budgets.append(budget_for(cdf, int(case["cached_tokens"]), rates, interval_s=interval,
                                  life_s=life, max_k=max_k,
                                  min_prefix=int(case.get("min_prefix", 0))))
    print(json.dumps({"break_even": break_evens, "budgets": budgets, "windows": wins}))
    return 0


def _self_test() -> int:
    """Assert the properties the module claims, on hand-written distributions.

    Deliberately the same four the Go tests assert, because the two implementations agreeing
    on a fixture is worth less if neither is checked against the arithmetic it claims.
    """
    rates = Rates.from_input_rate(3.0e-6)
    be = break_even(rates.cache_read, rates.write_5m)
    assert abs(be - 0.0869565) < 1e-6, f"break-even moved: {be}"

    def cdf_of(points):
        pts = sorted(points)

        def f(x: float) -> float:
            p = 0.0
            for at, val in pts:
                if x >= at:
                    p = val
            return p
        return f

    # the gate is prefix-independent
    below, above = cdf_of([(300, 0), (580, be * 0.5)]), cdf_of([(300, 0), (580, be * 3)])
    for prefix in (2_000, 124_845, 2_000_000):
        assert budget_for(below, prefix, rates, max_k=1) == 0, prefix
        assert budget_for(above, prefix, rates, max_k=1) >= 1, prefix

    # the option value pings through a lean first window
    lean = cdf_of([(580, 0.02), (860, 0.60), (3600, 1)])
    assert budget_for(lean, 124_845, rates, max_k=1) == 0
    assert budget_for(lean, 124_845, rates, max_k=4) >= 2

    # a worthless tail ends the schedule; a reachable rich window does not
    assert budget_for(cdf_of([(300, 0), (580, 0.30), (86400, 0.30)]), 124_845, rates) == 1
    far = cdf_of([(300, 0), (580, .3), (860, .3), (1140, .3), (1420, .3), (1700, .95), (86400, 1)])
    assert budget_for(far, 124_845, rates) >= 5

    # the hazard is conditional on still being idle
    gone = cdf_of([(280, 0.90), (580, 0.90), (860, 0.95), (3600, 1)])
    stay = cdf_of([(280, 0.00), (580, 0.00), (860, 0.05), (3600, 1)])
    hg, _ = windows(gone, max_k=2)
    hs, _ = windows(stay, max_k=2)
    assert hg[1] > 5 * hs[1], f"not conditional: {hg[1]} vs {hs[1]}"

    print(f"self-test OK   break-even {100 * be:.2f}%   13 features   "
          f"interval {DEFAULT_INTERVAL_S:.0f}s / life {DEFAULT_LIFE_S:.0f}s / max_k "
          f"{DEFAULT_MAX_K}")
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--fixture", help="score a JSON fixture and print it (drift test)")
    ap.add_argument("--self-test", action="store_true",
                    help="assert the module's claims on hand-written distributions")
    args = ap.parse_args(argv)
    if args.fixture:
        return _run_fixture(args.fixture)
    if args.self_test:
        return _self_test()
    ap.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
