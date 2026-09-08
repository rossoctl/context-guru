#!/usr/bin/env python3
"""How MANY keep-alive pings a conversation is worth, decided per conversation.

This is the learned counterpart of `kvcache.BudgetPolicy`, and the two must agree: the Go side
is what a replay is scored by, this side is where the fit lives, and
`kv_ttl_keepalive_drift_test.go` drives the `--fixture` entry point below to pin them together.
When they disagree, Go is right. (`BudgetPolicy` is not in `kvcache.Registry()`, so it reaches a
replay the way `Custom`'s predictor does: from an in-process caller, because "a predictor is
code, not a query parameter". No dashboard page renders it.)

    kv_ttl_keepalive_policy.py --fixture f.json      # the drift test's interface, stdlib only
    kv_ttl_keepalive_policy.py --score f.json        # the MODEL's drift interface, stdlib only
    kv_ttl_keepalive_policy.py --self-test           # asserts this module's claims, stdlib only
    kv_ttl_keepalive_policy.py --db cg.db --prices prices.yaml \
        --model-out reusemodel_v1.json --go-out ../../kvcache/reusemodel_v1_gen.go

`--db` fits the SHIPPED model and writes the two checked-in artifacts. It is the wiring this
docstring used to say was missing, and it exists so the numbers below can be re-derived rather
than quoted.

Two models live here and the difference is the point:

  * `fit()` is the reference — a gradient-boosted pair, the fit AS IT WAS RUN, kept because it
    is the method the measurements above were made with. It is NOT shippable: ~37 trees cannot
    be reviewed in a diff, cannot be a dot product on the live pinger's request path, and have
    no coefficients to port.
  * `fit_portable()` is what ships — the same person-period frame, the same stake weights, the
    same conditional target, fitted as a logistic regression so it exports as ~30 coefficients
    that compile into `kvcache.ReuseModelV1`. `--db` prints both models' metrics side by side,
    so the cost of going linear is measured on every re-fit rather than assumed once.

That is what puts the arm on a dashboard page. `kvcache.BudgetPolicy` was kept out of
`kvcache.Registry()` because it "cannot be built from a name alone — it needs a `Predictor`";
with the model compiled in, it can, and the registry keeps its promise that every name in it
resolves. See docs/how-to/kv-cache-keepalive-budget.md.

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
beats this policy by 0.89 pp of the bill: it ranks conversations well (AUC 0.93) and DOLLARS much
worse, falling to 0.57 on the largest prefixes, which carry 87% of the stake (not 0.70 for the
whole population — kv-cache-keepalive-budget.md's own AUC-decomposition math rules that out;
its table flags the whole-population dollar-AUC cell unverified instead), so its mistakes land
where they cost most, and at a 11.5:1 payoff a wrong skip is expensive. Per PING it is 9x
better. So:

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
import math
import os
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

#: Where the sibling modules keep these, so a `--db` run reads the same corpus and the same
#: rates the cost model and the arm study did.
DEFAULT_DB = "/var/lib/context-guru/cg.db"
DEFAULT_PRICES = "/etc/context-guru/prices.yaml"
#: The checked-in fitted model, beside this file. `--score` reads it; nothing at runtime does.
DEFAULT_MODEL = "reusemodel_v1.json"


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


# ── the shipped model: a frozen dot product, and the CDF it composes ─────────
#
# Also stdlib-only and pure, and for the same reason: `--score` drives it from
# kv_ttl_reusemodel_drift_test.go, and a guard that needs numpy is a guard that does not run
# in CI. Everything the FIT needs stays below the divider; everything the SHIPPED model needs
# to be evaluated is here.
#
# WHY A LOGISTIC REGRESSION WHEN `fit()` BELOW IS A GRADIENT-BOOSTED ENSEMBLE
# ---------------------------------------------------------------------------
# Because the ensemble cannot be shipped. Three constraints meet here and only one shape
# satisfies all of them:
#
#   * `kvcache.Predictor` is consulted inside `BudgetPolicy.Windows`, which is on the request
#     path in the live pinger. proxy/keepalivestrategy.go's own rule for that path is
#     explicit: "a rule, or ... a portable logistic-regression dot product — never an embedded
#     model or a call out of the hot path."
#   * The model has to be REVIEWABLE. A `HistGradientBoostingClassifier` is ~37 trees of
#     thresholds; a reviewer cannot read a diff of it and say whether it got worse.
#   * It has to be FIXED. A model that is re-fitted at startup makes yesterday's dashboard
#     number unreproducible, and the kv-cache page's whole claim is that a historical window
#     replays identically every time.
#
# So the shipped model is the same person-period survival model as `fit()`'s `survival` arm —
# the same frame, the same stake weights, the same conditional target — fitted as a logistic
# regression so that it exports as coefficients. What that costs against the ensemble is
# measured, not assumed: `--fit-report` prints both, and the figure is in
# docs/how-to/kv-cache-keepalive-budget.md. It is not free, and it is not large.
#
# `user_id` is deliberately NOT among the shipped features even though `FEATURES` below has
# it. A one-hot over tenant ids is a per-tenant dummy: it does not generalise to a tenant the
# fit never saw, it grows the artifact with every account, and it puts tenant identifiers in
# the shipped binary. `stat_p`/`log_stat_n` already carry that tenant's own history in a
# non-identifying form — which is what the accumulator is for.

#: The shipped model's NUMERIC features, in coefficient order. The order is a wire contract:
#: Go reads the same list from the generated table and must apply it index for index.
#: `STATE_NUM` above carries `sweep_k`, `log_elapsed` and `log_secs_to_deadline`; none of the
#: three is here, and this is the one place the shipped model's SPECIFICATION differs from the
#: reference rather than only its estimator. All three are exact functions of the sweep index —
#: `log_elapsed` is log1p(k*interval), and `log_secs_to_deadline` is log1p(life - interval) at
#: EVERY sweep, a constant. In a tree ensemble that is merely redundant. In a linear model it is
#: perfect collinearity, and the first fit showed what that costs: coefficients of -4.77 and
#: +4.09 on two features that move together, summing to a hazard of ~14% at every sweep against
#: a measured 4.86% at sweep 1 falling to 0.24% by sweep 7. The arm then bought its maximum
#: budget on 97% of decisions and saved LESS than a flat MaxPings=2.
#:
#: The sweep index is in `SHIPPED_CAT` instead, one-hot — a free baseline hazard per sweep. That
#: is the standard discrete-time survival specification and it is strictly more general than the
#: linear term it replaces: the shape of the hazard over k is read off the corpus rather than
#: assumed monotone, and the measured shape is not monotone.
SHIPPED_NUM = ["log_prefix", "log_prev_gap", "turn", "hour_sin", "hour_cos", "dow_sin",
               "dow_cos", "stat_p", "log_stat_n"]

#: The shipped model's ONE-HOT features. Provider-level, so they generalise.
#:
#: `cache_ttl` is in `SPAN_CAT` above and deliberately NOT here, and the reason only becomes
#: visible once the model is wired in behind `kvcache.Predictor` — which is what this PR does
#: and what the reference fit never did:
#:
#:   * Offline, `cache_ttl` is the tier the corpus RECORDED, read straight off the row. Fitting
#:     on it and scoring on it is self-consistent, which is why the reference fit is fine.
#:   * At serve time the seam hands over `Observation.TTL`, and kvcache/simulate.go sets that
#:     from `st.tier` — the tier the ARM UNDER TEST chose on the previous turn. On a replay of
#:     `keepalive-1h` every row reads `ephemeral_1h` no matter what the corpus recorded.
#:
#: So the feature means one thing in training and another in serving, and worse, the second
#: meaning is the model's own prior output fed back as an input. Neither is fixable by
#: re-fitting: the tier is endogenous to the arm. Dropped, and the ~0.13 spread its
#: coefficients carried is left on the table on purpose.
#:
#: `sweep` is the one-hot baseline hazard described above `SHIPPED_NUM`. Its level is the sweep
#: index as a decimal string, so it needs no machinery of its own beyond what `model` already
#: uses. The level for sweep `max_k + 1` is never fitted — there is no label past the horizon —
#: and never read either: `windows()` looks no further than `t_max_k + life`, which sits below
#: `t_(max_k+1)`.
SHIPPED_CAT = ["model", "sweep"]

#: Hour and day-of-week are encoded as sin/cos pairs rather than as the raw integers
#: `STATE_NUM` carries. A linear model reads raw hour 23 and hour 0 as 23 units apart, which
#: is not a fact about time; the ensemble did not care because it splits. The pair costs one
#: extra coefficient each and makes the encoding honest. Measured effect on this corpus is
#: inside the seed-noise floor either way — see the negative results in the module docstring.
_HOUR_PERIOD = 24.0
_DOW_PERIOD = 7.0


def _level_of(span: dict, name: str, k: int) -> str:
    """A one-hot level. `sweep` comes from the sweep index; everything else off the span.

    Model ids are case-folded. The corpus carries the same upstream under more than one
    spelling — `Azure/gpt-4o` beside `azure/gpt-5.5` — because the id is whatever the client
    sent, and two levels for one model split its evidence in half. `PriceBook` case-folds its
    match keys for the same reason. Go's ReuseModel does `strings.ToLower` here too.
    """
    if name == "sweep":
        return str(int(k))
    return str(span.get(name) or "").lower()


def span_from_raw(*, ts_ms: int, cached_tokens: int, since_last_ms: int, turn: int,
                  stat_p: float, stat_n: int, model: str) -> dict:
    """The span-level feature dict, derived from what `Observation` actually carries.

    The ONE place the derivation lives, so `load_spans` below and the `--score` guard cannot
    disagree about it. Go's `ReuseModel.spanOf` is the counterpart and is compared against this
    by kv_ttl_reusemodel_drift_test.go — which is why the guard's fixture carries the RAW fields
    and not the logs: a fixture carrying precomputed features could not catch a log1p applied to
    milliseconds on one side and seconds on the other.
    """
    return {
        "ts_ms": int(ts_ms),
        "log_prefix": math.log1p(max(int(cached_tokens), 0)),
        "log_prev_gap": math.log1p(max(int(since_last_ms), 0) / 1000.0),
        "turn": float(turn),
        "stat_p": float(stat_p),
        "stat_n": int(stat_n),
        "model": model or "",
    }


def _sigmoid(z: float) -> float:
    """1/(1+e^-z), written so a large-magnitude z cannot overflow either way."""
    if z >= 0.0:
        return 1.0 / (1.0 + math.exp(-z))
    e = math.exp(z)
    return e / (1.0 + e)


def _interp(x: float, xs: list[float], ys: list[float]) -> float:
    """`numpy.interp` for a sorted grid: linear inside, CLAMPED to the endpoints outside.

    Clamping matters and is not a detail. A horizon past the last knot must read as the last
    cumulative value, not extrapolate past 1.0; a horizon before the first must read 0. Go's
    ReuseModel.cdf does the same, and kv_ttl_reusemodel_drift_test.go compares the two on
    horizons deliberately placed outside the grid.
    """
    if not xs:
        return 0.0
    if x <= xs[0]:
        return ys[0]
    if x >= xs[-1]:
        return ys[-1]
    lo, hi = 0, len(xs) - 1
    while hi - lo > 1:
        mid = (lo + hi) // 2
        if xs[mid] <= x:
            lo = mid
        else:
            hi = mid
    span = xs[hi] - xs[lo]
    if span <= 0.0:
        return ys[hi]
    return ys[lo] + (ys[hi] - ys[lo]) * (x - xs[lo]) / span


@dataclass
class ReuseModel:
    """A frozen discrete-time survival model over the sweep grid, as a portable dot product.

    One coefficient per shipped feature, one per one-hot level, one intercept, and the
    standardisation the fit applied — because Go has to apply the identical transform or the
    coefficients mean nothing. `kvcache.ReuseModelV1` is the generated Go twin of this, and
    `--score` exists so a test can prove the twins agree.

    The model answers the CONDITIONAL question `s_k = P(still idle at t_(k+1) | idle at t_k)`,
    which is what the person-period frame labels. The cumulative CDF `windows()` wants is
    composed forwards from those, exactly as `FitResult.cdf_for` does it — see that method for
    the two caveats about the grid, which apply here identically.
    """

    intercept: float
    num_coef: dict[str, float] = field(default_factory=dict)
    num_center: dict[str, float] = field(default_factory=dict)
    num_scale: dict[str, float] = field(default_factory=dict)
    #: feature -> level -> coefficient. The empty-string level is the catch-all an unseen
    #: level falls to, so a new model id scores as "like the others" rather than as zero.
    cat_coef: dict[str, dict[str, float]] = field(default_factory=dict)
    interval_s: float = DEFAULT_INTERVAL_S
    life_s: float = DEFAULT_LIFE_S
    max_k: int = DEFAULT_MAX_K
    #: Provenance. Not decoration: a shipped model whose training window nobody can name is a
    #: number nobody can argue with.
    trained: dict = field(default_factory=dict)

    # -- the feature vector, which must match _state_frame's state block -----

    def state_features(self, span: dict, k: int) -> dict:
        """One span's shipped features AS SEEN AT SWEEP k.

        The span block is carried; only the state block moves with k. Deliberately the same
        arithmetic as `_state_frame` below, on the same fields, so that the shipped model and
        the reference ensemble are fitted on the same frame and differ ONLY in the estimator.
        """
        t_k = k * self.interval_s
        secs = span["ts_ms"] / 1000.0 + t_k
        hour = int(secs // 3600 % 24)
        dow = int((secs // 86400 + 4) % 7)
        return {
            "log_prefix": float(span["log_prefix"]),
            "log_prev_gap": float(span["log_prev_gap"]),
            "turn": float(span["turn"]),
            "hour_sin": math.sin(2.0 * math.pi * hour / _HOUR_PERIOD),
            "hour_cos": math.cos(2.0 * math.pi * hour / _HOUR_PERIOD),
            "dow_sin": math.sin(2.0 * math.pi * dow / _DOW_PERIOD),
            "dow_cos": math.cos(2.0 * math.pi * dow / _DOW_PERIOD),
            "stat_p": float(span["stat_p"]),
            "log_stat_n": math.log1p(float(span["stat_n"])),
        }

    def survival(self, span: dict, k: int) -> float:
        """`s_k` for one span: the dot product, standardised, through the logistic."""
        f = self.state_features(span, k)
        z = self.intercept
        for name in SHIPPED_NUM:
            scale = self.num_scale.get(name) or 1.0
            z += self.num_coef.get(name, 0.0) * (f[name] - self.num_center.get(name, 0.0)) / scale
        for name in SHIPPED_CAT:
            levels = self.cat_coef.get(name) or {}
            level = _level_of(span, name, k)
            # An unseen level falls to the catch-all rather than to zero. Zero is not
            # "unknown", it is "exactly the reference level", which is a different claim.
            z += levels.get(level, levels.get("", 0.0))
        return _sigmoid(z)

    def cdf_for(self, span: dict, *, interval_s: float | None = None,
                life_s: float | None = None, max_k: int | None = None):
        """A CUMULATIVE return-time CDF closure for one span, for `budget_for`/`windows`.

        `F(t_(j+1)) = 1 - prod_(i<=j) s_i`, on the grid {t_j} u {t_j + life}, linearly
        interpolated between knots and clamped outside. F(t_1) = 0 by construction: this CDF
        is already conditioned on having reached the first sweep, which is exactly the
        population `windows()` divides by.
        """
        iv = self.interval_s if interval_s is None else interval_s
        life = self.life_s if life_s is None else life_s
        mk = self.max_k if max_k is None else max_k
        if mk <= 0:
            mk = DEFAULT_MAX_K
        surv, cum = 1.0, {0.0: 0.0}
        for j in range(1, mk + 2):
            t_j = j * iv
            cum[t_j] = 1.0 - surv
            surv *= self.survival(span, j)
            cum[t_j + life] = 1.0 - surv
        xs = sorted(cum)
        ys = [cum[x] for x in xs]
        return lambda seconds: _interp(float(seconds), xs, ys)

    # -- serialisation -------------------------------------------------------

    def to_dict(self) -> dict:
        return {"intercept": self.intercept, "num_coef": self.num_coef,
                "num_center": self.num_center, "num_scale": self.num_scale,
                "cat_coef": self.cat_coef, "interval_s": self.interval_s,
                "life_s": self.life_s, "max_k": self.max_k, "trained": self.trained,
                "shipped_num": SHIPPED_NUM, "shipped_cat": SHIPPED_CAT}

    @classmethod
    def from_dict(cls, d: dict) -> "ReuseModel":
        """Rebuild from `--model-out`'s JSON, refusing a feature list this code cannot score.

        A silently-ignored extra feature is a model scored on a subset of what it was fitted
        on, which produces plausible numbers and no error at all.
        """
        for key, want in (("shipped_num", SHIPPED_NUM), ("shipped_cat", SHIPPED_CAT)):
            got = d.get(key)
            if got is not None and list(got) != want:
                raise ValueError(f"{key} mismatch: model has {got}, this code scores {want}")
        return cls(intercept=float(d["intercept"]),
                   num_coef=dict(d.get("num_coef") or {}),
                   num_center=dict(d.get("num_center") or {}),
                   num_scale=dict(d.get("num_scale") or {}),
                   cat_coef={k: dict(v) for k, v in (d.get("cat_coef") or {}).items()},
                   interval_s=float(d.get("interval_s", DEFAULT_INTERVAL_S)),
                   life_s=float(d.get("life_s", DEFAULT_LIFE_S)),
                   max_k=int(d.get("max_k", DEFAULT_MAX_K)),
                   trained=dict(d.get("trained") or {}))


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
    AUC 0.93 by row while ranking dollars far worse — see kv-cache-keepalive-budget.md's AUC
    table (its whole-population dollar figure is flagged unverified there; 0.70 is not it,
    and the same page's arithmetic rules 0.70 out too). Weighting by stake does not close that
    gap — nothing measured here does — but fitting without it makes it worse.
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


# ── fitting the shipped model, and where its training data comes from ────────
#
# This block is what the module docstring called "the next thing to do here": the `--db`
# wiring the fit never had, so the figures can be reproduced rather than quoted.


#: How many closed gaps a stats cell needs before it is trusted over its parent. The same 6
#: as kvcache/strategy.go's `minCell`, and it has to be: `stat_p`/`stat_n` are read from
#: `Observation.Stats` at serve time, so a fit that computed them with a different floor would
#: be training on features the model is never shown.
MIN_CELL = 6

#: The four six-hour bands of kvcache.BucketOf, which `Observation.Bucket` carries.
_BUCKETS = ("night", "morning", "afternoon", "evening")


def _bucket_at(ts_ms: int) -> str:
    """kvcache.BucketAt in Python: the UTC six-hour band an instant falls in."""
    hour = int((ts_ms // 3_600_000) % 24)
    if hour >= 18:
        return "evening"
    if hour >= 12:
        return "afternoon"
    if hour >= 6:
        return "morning"
    return "night"


class _History:
    """kvcache.History in Python: the leak-free stats accumulator, gap by closing gap.

    A faithful port, not an approximation, because `stat_p`/`stat_n` are served to the model
    at request time by the Go original. Same five fallback levels, same "count the gap in
    EVERY level", same `MIN_CELL` floor, same global-cell last resort. `windows()` and the
    break-even are pinned to Go by a drift test; this is pinned to Go by
    TestReuseModelStatsMatchTheAccumulator, which replays one trajectory through both.
    """

    def __init__(self) -> None:
        self._gaps: dict[tuple, list[float]] = {}

    @staticmethod
    def _keys(user: str, model: str, bucket: str) -> list[tuple]:
        return [(user, model, bucket), (user, model, ""), (user, "", ""),
                ("", model, ""), ("", "", "")]

    def observe(self, user: str, model: str, bucket: str, gap_s: float) -> None:
        """Record one CLOSED gap. Never called for a gap that has not happened yet."""
        if gap_s < 0.0:
            gap_s = 0.0
        for k in self._keys(user, model, bucket):
            self._gaps.setdefault(k, []).append(gap_s)

    def reuse_within(self, user: str, model: str, bucket: str,
                     horizon_s: float) -> tuple[float, int]:
        """The share of this cell's closed gaps no longer than `horizon_s`, and the count."""
        ks = self._keys(user, model, bucket)
        cell = None
        for k in ks:
            g = self._gaps.get(k)
            if g is not None and len(g) >= MIN_CELL:
                cell = g
                break
        if cell is None:
            cell = self._gaps.get(ks[-1]) or None
        if not cell:
            return 0.0, 0
        hits = sum(1 for g in cell if g <= horizon_s)
        return hits / len(cell), len(cell)


#: A successor that could not have read the entry however warm it was. The same two reasons
#: kvcache's stepCost excludes from `hit`, so a "rescue" here means what it means there.
_FULL_MISS = ("prefix_change", "cold_start")


def load_spans(db_path: str, prices_path: str, *, since_ms: int | None = None,
               until_ms: int | None = None, horizon_s: float = 300.0):
    """One row per IDLE SPAN, with the features the shipped model reads, read-only from cg.db.

    An idle span is opened by every request and closed by its successor in the same
    conversation. `gap_s` is the seconds until that successor, and it is `inf` — an
    unrescuable span — in two cases that are the same case: there is no successor inside the
    window, or the successor was a full miss (`prefix_change`/`cold_start`), which could not
    have read the entry however warm a ping kept it.

    `stake` is the DOLLARS at risk on the span: what one successful rescue avoids, at the
    price list's own rates for that row's model. It is the fit's sample weight, and the
    reason is in `fit()`'s docstring.

    Trajectory reading is delegated to `kv_ttl_cost_model.load_trajectories`, which owns the
    read-only-URI quoting and the `keepalive = 0` exclusion — a ping counted as a turn invents
    traffic and breaks the very gap it sits inside.
    """
    import numpy as np
    import pandas as pd

    from kv_ttl_cost_model import PriceBook, load_trajectories

    rows = load_trajectories(db_path, since_ms=since_ms, until_ms=until_ms)
    book = PriceBook.from_operator_file(prices_path)

    # Group into trajectories so a successor is only ever this conversation's own. Sorted by
    # (ts, id) for the same reason Simulate sorts: a tied timestamp must break deterministically.
    by_conv: dict[tuple, list] = {}
    for r in rows:
        by_conv.setdefault(r.key, []).append(r)
    for g in by_conv.values():
        g.sort(key=lambda r: (r.ts_ms, r.request_id))

    hist = _History()
    prev_ts: dict[tuple, int] = {}
    prev_gap: dict[tuple, float] = {}
    turn: dict[tuple, int] = {}
    nxt: dict[int, object] = {}
    for g in by_conv.values():
        for a, b in zip(g, g[1:]):
            nxt[a.request_id] = b

    out = []
    # The SAME chronological walk Simulate does, in the same order, for the same reason: the
    # accumulator must only ever hold gaps that closed at or before the decision instant.
    for r in sorted(rows, key=lambda r: (r.ts_ms, r.request_id)):
        key = r.key
        # 1. close the previous span — the gap becomes history HERE and only here, bucketed at
        #    the PREDECESSOR's timestamp and priced at the current row's model, exactly as
        #    kvcache/simulate.go does it.
        if key in prev_ts:
            gap_s = max(0.0, (r.ts_ms - prev_ts[key]) / 1000.0)
            hist.observe(r.user, r.model, _bucket_at(prev_ts[key]), gap_s)
            prev_gap[key] = gap_s
        # 2. the decision instant: everything below was knowable now.
        turn[key] = turn.get(key, 0) + 1
        stat_p, stat_n = hist.reuse_within(r.user, r.model, _bucket_at(r.ts_ms), horizon_s)
        prefix = int(r.cached_context or 0)
        price = book.for_model(r.model) if hasattr(book, "for_model") else book.for_(r.model)
        succ = nxt.get(r.request_id)
        if succ is None or (succ.miss_reason or "") in _FULL_MISS:
            gap = float("inf")
        else:
            gap = max(0.0, (succ.ts_ms - r.ts_ms) / 1000.0)
        rescue_rate = (price.write_5m - price.cache_read) if price.known else 0.0
        row = span_from_raw(ts_ms=r.ts_ms, cached_tokens=prefix,
                            since_last_ms=int(1000.0 * prev_gap.get(key, 0.0)),
                            turn=turn[key], stat_p=stat_p, stat_n=stat_n, model=r.model)
        row.update({"gap_s": gap, "stake": prefix * rescue_rate, "user_id": r.user,
                    "cache_ttl": (r.ttl_recorded or ""), "prefix": prefix})
        out.append(row)
        prev_ts[key] = r.ts_ms
    return pd.DataFrame(out)


def _shipped_design(spans, featuriser: ReuseModel, *, interval_s: float, life_s: float,
                    max_k: int, levels: dict | None = None, min_level_rows: int = 200):
    """The person-period design matrix for the SHIPPED feature set.

    Same layout as `person_periods` — one row per (span, sweep it ACTUALLY reached) — and the
    numeric block is produced by `ReuseModel.state_features` itself rather than by a second
    copy of that arithmetic. That is deliberate: the featuriser used to FIT is the same code
    path that scores at serve time, so the two cannot drift apart in a way a test would have
    to notice.

    Returns (X, y, w, levels). `levels` maps each one-hot feature to its ordered level list,
    with "" LAST as the catch-all that rare and unseen levels share — the same role
    `OneHotEncoder(min_frequency=...)` plays in the reference fit.
    """
    import numpy as np

    if levels is None:
        levels = {}
        for name in SHIPPED_CAT:
            if name == "sweep":
                # Enumerated rather than counted out of the frame: every sweep the induction can
                # reach needs its own baseline, and a sweep that happened to be rare in this
                # window must not be folded into the catch-all, which would hand it another
                # sweep's hazard.
                levels[name] = [str(j) for j in range(1, max_k + 1)] + [""]
                continue
            counts = spans[name].fillna("").astype(str).str.lower().value_counts()
            keep = [str(v) for v, n in counts.items() if n >= min_level_rows and str(v) != ""]
            levels[name] = sorted(keep) + [""]

    rows_num, rows_cat, ys, ws = [], [], [], []
    for k in range(1, max_k + 1):
        t_k = k * interval_s
        reached = spans[spans.gap_s > t_k]
        if reached.empty:
            break
        # `t_k + interval_s` at EVERY sweep, including the last.
        #
        # `person_periods` above uses `inf` at k == max_k, which makes `y_survival` identically 0
        # there — "nobody survives past the horizon". For the reference's own purpose that is
        # only a truncation. For a model whose survival curve is COMPOSED INTO A CDF it is a
        # corruption of the tail, and it is not subtle: s_max_k fits to ~0, so F jumps to 1.0 at
        # t_max_k + life, and `windows()` then computes
        #
        #     h_max_k = (F(t_max_k + life) - F(deadline)) / (1 - F(t_max_k)) = (1-F)/(1-F) = 1.0
        #
        # — a 100% hazard at the last sweep. The backward induction propagates that certainty
        # back through every earlier sweep, so V_j > 0 everywhere and the arm buys its maximum
        # budget on 100% of decisions, byte-identical to a flat MaxPings. Measured exactly that
        # before this line changed.
        #
        # Nothing needs censoring here: `gap_s > t_max_k + interval_s` is observable for every
        # span in the frame, so the label is available and the honest thing is to use it.
        nxt_t = t_k + interval_s
        for rec in reached.to_dict("records"):
            f = featuriser.state_features(rec, k)
            rows_num.append([f[n] for n in SHIPPED_NUM])
            onehot = []
            for name in SHIPPED_CAT:
                lv = levels[name]
                val = _level_of(rec, name, k)
                idx = lv.index(val) if val in lv else len(lv) - 1
                onehot.extend(1.0 if i == idx else 0.0 for i in range(len(lv)))
            rows_cat.append(onehot)
            ys.append(1 if rec["gap_s"] > nxt_t else 0)
            ws.append(max(0.0, float(rec["stake"])))
    if not rows_num:
        raise ValueError("no span reached the first sweep; nothing to fit")
    xn = np.asarray(rows_num, dtype=float)
    xc = np.asarray(rows_cat, dtype=float)
    y = np.asarray(ys, dtype=int)
    w = np.asarray(ws, dtype=float)
    # The same stake normalisation `fit()` uses, and the same epsilon: a zero-stake row still
    # carries a little weight, because dropping it would silently change the population.
    w = 1e-9 + w / (w.mean() or 1.0)
    return np.hstack([xn, xc]), y, w, levels


def _weights(w, mode: str):
    """The sample weight for a given `--weight` mode.

    `stake` is what `fit()` uses and its docstring defends: rows are not what the bill is made
    of. That argument is about RANKING, and it is right about ranking.

    `none` exists because of an argument that turned out to be WRONG, and it is kept, and
    still selectable, because the argument is a good one and the next corpus may vindicate it.
    It runs: this model is not used for ranking. `BudgetPolicy` compares the probability against
    the rates' own break-even — 8.70% — so what the arm needs is a CALIBRATED probability for
    the span in front of it, and a stake-weighted fit is deliberately miscalibrated, reporting
    the hazard of a dollar rather than of a conversation. The two differ fourfold on this corpus
    (21.10% stake-weighted at sweep 1 against 4.86% by row) and the decision turns entirely on
    which side of 8.70% the number lands. `log_prefix` is a covariate precisely so the size
    effect survives without the weighting.

    Measured on the 18.4-day corpus, both fits replayed through the real arm against the same
    fixed-5m baseline:

        weight    pings   off the bill   net per ping   budget 0 on
        stake     9,003         2.059%        0.0907          41.4%
        none      7,250         1.836%        0.1004          56.4%

    So `stake` is worth 0.22 pp more of the bill, and `none` is 11% better per ping. `stake`
    ships because the money is the question the page is asked; the calibration argument bought
    efficiency, not savings. What it did NOT do is what the argument predicted — being
    better-calibrated against the break-even did not produce better decisions, it produced
    FEWER of them. That is worth remembering before re-deriving the same argument.
    """
    import numpy as np

    if mode == "stake":
        return w
    if mode == "none":
        return np.ones_like(w)
    raise ValueError(f"unknown weight mode {mode!r}; want stake or none")


def fit_portable(spans, *, interval_s: float = DEFAULT_INTERVAL_S,
                 life_s: float = DEFAULT_LIFE_S, max_k: int = DEFAULT_MAX_K,
                 seed: int = 0, weight: str = "stake") -> tuple:
    """Fit the SHIPPED model: the survival arm, as a logistic regression that exports.

    Same frame and same stake weights as `fit()`; the estimator is the only difference, and
    the whole reason is portability — see the shipped-model block above the fit divider.

    Returns (ReuseModel, report). The report carries the metrics the docs quote, plus the
    reference ensemble's own metrics on the identical frame, so the cost of going linear is a
    measured number rather than a claim.
    """
    import numpy as np
    from sklearn.ensemble import HistGradientBoostingClassifier
    from sklearn.linear_model import LogisticRegression
    from sklearn.metrics import log_loss, roc_auc_score

    shape = ReuseModel(intercept=0.0, interval_s=interval_s, life_s=life_s, max_k=max_k)
    x, y, stake, levels = _shipped_design(spans, shape, interval_s=interval_s, life_s=life_s,
                                          max_k=max_k)
    w = _weights(stake, weight)
    n_num = len(SHIPPED_NUM)
    center, scale, degenerate = _standardise(x[:, :n_num])
    xs = x.copy()
    xs[:, :n_num] = (xs[:, :n_num] - center) / scale
    # A feature with no variance in the frame carries no information, and standardising it
    # would divide by ~0 — which is not a rounding concern but a correctness one: the
    # coefficient then multiplies float noise by 1/eps, so Python and Go disagree on the
    # fourteenth decimal and the CDF disagrees in the second. `log_secs_to_deadline` is
    # exactly this: with interval < life, `deadline - t_k` is `life - interval` at EVERY
    # sweep, so the term is a constant the intercept already carries. Zeroed, listed, and
    # kept in the feature vector so the wire contract with Go does not move.
    xs[:, :n_num][:, degenerate] = 0.0

    lr = LogisticRegression(max_iter=2000, C=1.0, random_state=seed)
    lr.fit(xs, y, sample_weight=w)
    coef = lr.coef_[0].copy()
    coef[:n_num][degenerate] = 0.0

    model = ReuseModel(
        intercept=float(lr.intercept_[0]),
        num_coef={n: float(coef[i]) for i, n in enumerate(SHIPPED_NUM)},
        num_center={n: float(center[i]) for i, n in enumerate(SHIPPED_NUM)},
        num_scale={n: float(scale[i]) for i, n in enumerate(SHIPPED_NUM)},
        cat_coef={}, interval_s=interval_s, life_s=life_s, max_k=max_k)
    at = n_num
    for name in SHIPPED_CAT:
        lv = levels[name]
        model.cat_coef[name] = {lv[i]: float(coef[at + i]) for i in range(len(lv))}
        at += len(lv)

    def scored(p):
        # Both AUCs, because they answer different questions and the module docstring is built
        # on the difference: by ROW the model looks strong, by DOLLAR it is much weaker, and
        # reporting only the first is how that gets missed.
        # auc_dollar and the loss are always STAKE-weighted, whatever `--weight` fitted with:
        # they answer "how well does this rank/price dollars", which does not change because the
        # estimator stopped optimising for it. Reporting them under the fit's own weights would
        # make the two modes' numbers incomparable, which is the one thing they are for.
        return {"auc_row": float(roc_auc_score(y, p)),
                "auc_dollar": float(roc_auc_score(y, p, sample_weight=stake)),
                "logloss_dollar": float(log_loss(y, p, sample_weight=stake)),
                "logloss_row": float(log_loss(y, p)),
                "mean_pred": float(p.mean()), "mean_actual": float(y.mean())}

    gbm = HistGradientBoostingClassifier(max_iter=200, learning_rate=0.08, random_state=seed)
    gbm.fit(xs, y, sample_weight=w)
    report = {
        "n_rows": int(len(y)), "n_spans": int(len(spans)),
        "n_reached_first_sweep": int((spans.gap_s > interval_s).sum()),
        "positive_rate": float(y.mean()),
        "weight": weight,
        "degenerate": [SHIPPED_NUM[i] for i, d in enumerate(degenerate) if d],
        "in_sample": {"logistic": scored(lr.predict_proba(xs)[:, 1]),
                      "reference_gbm": scored(gbm.predict_proba(xs)[:, 1])},
        "holdout": _holdout(spans, interval_s=interval_s, life_s=life_s, max_k=max_k,
                            seed=seed, weight=weight),
        "levels": {k: v for k, v in levels.items()},
    }
    return model, report


def _standardise(xn):
    """Per-column mean and standard deviation, with the no-variance columns flagged.

    The floor is relative to the column's own centre rather than absolute, because a feature
    measured in log-seconds and one measured in turns do not share a scale at which "no
    variance" means the same thing.
    """
    import numpy as np

    center = xn.mean(axis=0)
    scale = xn.std(axis=0)
    degenerate = scale <= 1e-8 * (1.0 + np.abs(center))
    scale = scale.copy()
    scale[degenerate] = 1.0
    center = np.where(degenerate, 0.0, center)
    return center, scale, degenerate


def _holdout(spans, *, interval_s: float, life_s: float, max_k: int, seed: int,
             weight: str = "none", train_frac: float = 0.7) -> dict:
    """Fit on the EARLY part of the window and score the late part.

    Split by time, never at random. A random split puts the same conversation's sweeps on both
    sides — the person-period layout means one span contributes up to `max_k` rows — so a
    random holdout is scored on rows whose siblings it trained on, and reports a number the
    deployment will not see. Rolling origin is what the reference measurement used; one
    time-ordered cut is the cheap version of it, and it is the number to argue with.
    """
    import numpy as np
    from sklearn.linear_model import LogisticRegression
    from sklearn.metrics import log_loss, roc_auc_score

    cut = float(spans.ts_ms.quantile(train_frac))
    early, late = spans[spans.ts_ms <= cut], spans[spans.ts_ms > cut]
    if early.empty or late.empty:
        return {"skipped": "window too short to split"}
    shape = ReuseModel(intercept=0.0, interval_s=interval_s, life_s=life_s, max_k=max_k)
    xtr, ytr, str_, levels = _shipped_design(early, shape, interval_s=interval_s,
                                             life_s=life_s, max_k=max_k)
    xte, yte, ste, _ = _shipped_design(late, shape, interval_s=interval_s, life_s=life_s,
                                       max_k=max_k, levels=levels)
    wtr = _weights(str_, weight)
    n_num = len(SHIPPED_NUM)
    center, scale, degenerate = _standardise(xtr[:, :n_num])

    def prep(x):
        out = x.copy()
        out[:, :n_num] = (out[:, :n_num] - center) / scale
        out[:, :n_num][:, degenerate] = 0.0
        return out

    lr = LogisticRegression(max_iter=2000, C=1.0, random_state=seed)
    lr.fit(prep(xtr), ytr, sample_weight=wtr)
    p = lr.predict_proba(prep(xte))[:, 1]
    if len(np.unique(yte)) < 2:
        return {"skipped": "holdout has one class only"}
    return {"train_rows": int(len(ytr)), "test_rows": int(len(yte)),
            "cut_ts_ms": int(cut),
            "auc_row": float(roc_auc_score(yte, p)),
            "auc_dollar": float(roc_auc_score(yte, p, sample_weight=ste)),
            "logloss_dollar": float(log_loss(yte, p, sample_weight=ste)),
            "logloss_row": float(log_loss(yte, p)),
            "mean_pred": float(p.mean()), "mean_actual": float(yte.mean())}


def _go_float(v: float) -> str:
    """A Go float literal that round-trips: 17 significant digits, never an integer."""
    s = repr(float(v))
    if "e" not in s and "E" not in s and "." not in s and "inf" not in s and "nan" not in s:
        s += ".0"
    return s


def emit_go(model: ReuseModel, report: dict, *, package: str = "kvcache") -> str:
    """Render the fitted model as the Go source that ships it.

    Generated rather than hand-typed because a 30-coefficient table transcribed by hand is a
    table with a typo in it, and checked in rather than loaded at runtime because a model read
    from a file at startup makes yesterday's dashboard figure unreproducible.
    """
    tr = dict(model.trained or {})
    ins = report.get("in_sample") or {}
    lg, gb = ins.get("logistic") or {}, ins.get("reference_gbm") or {}
    ho = report.get("holdout") or {}
    degenerate = report.get("degenerate") or []
    lines = [
        "// Code generated by deploy/harbor/kv_ttl_keepalive_policy.py --go-out. DO NOT EDIT.",
        "//",
        "// Regenerate with:",
        "//",
        "//\tcd deploy/harbor && python3 kv_ttl_keepalive_policy.py \\",
        "//\t    --db /var/lib/context-guru/cg.db --prices /etc/context-guru/prices.yaml \\",
        "//\t    --model-out reusemodel_v1.json --go-out ../../kvcache/reusemodel_v1_gen.go",
        "//",
        "// Provenance. This is the argument for trusting the coefficients, so it is generated",
        "// from the fit rather than written by hand — including the parts that are unflattering.",
        "//",
        f"//\twindow (UTC)        {tr.get('window', '?')}  ({tr.get('days', '?')} days)",
        f"//\trequests            {tr.get('requests', '?')}",
        f"//\tidle spans          {report.get('n_spans', '?')}",
        f"//\t  reaching sweep 1  {report.get('n_reached_first_sweep', '?')}"
        "   <- the population the decision is actually faced on",
        f"//\tperson-periods      {report.get('n_rows', '?')}",
        f"//\tschedule            interval {model.interval_s:.0f}s / life {model.life_s:.0f}s"
        f" / max_k {model.max_k}",
        f"//\tsurvived to sweep+1 {report.get('positive_rate', 0.0) * 100:.2f}% of person-periods",
        "//",
        f"//\tfit weight          {report.get('weight', '?')}",
        "//",
        "//\t                    AUC by row   AUC by dollar   logloss   mean p / actual",
        f"//\tholdout (late 30%)  {ho.get('auc_row', float('nan')):.4f}       "
        f"{ho.get('auc_dollar', float('nan')):.4f}          "
        f"{ho.get('logloss_row', float('nan')):.4f}      "
        f"{ho.get('mean_pred', float('nan')):.4f} / {ho.get('mean_actual', float('nan')):.4f}",
        f"//\tin-sample           {lg.get('auc_row', float('nan')):.4f}       "
        f"{lg.get('auc_dollar', float('nan')):.4f}          "
        f"{lg.get('logloss_row', float('nan')):.4f}      "
        f"{lg.get('mean_pred', float('nan')):.4f} / {lg.get('mean_actual', float('nan')):.4f}",
        f"//\tin-sample, ref GBM  {gb.get('auc_row', float('nan')):.4f}       "
        f"{gb.get('auc_dollar', float('nan')):.4f}          "
        f"{gb.get('logloss_row', float('nan')):.4f}      "
        f"{gb.get('mean_pred', float('nan')):.4f} / {gb.get('mean_actual', float('nan')):.4f}",
        "//",
        "// The HOLDOUT row is the one to argue with: it is fitted on the early 70% of the",
        "// window and scored on the late 30%, split by time rather than at random because one",
        "// span contributes up to max_k person-periods and a random split scores a row against",
        "// its own siblings. The in-sample rows are here only to show the gap; the reference",
        "// GBM's in-sample figures in particular are an upper bound on nothing.",
        "//",
        "// Neither AUC is a saving. What this arm is worth in money is the KV-cache page's own",
        "// replay of a window under it, against the same baseline as every other arm — which is",
        "// what shipping it as a registry arm is for. docs/how-to/kv-cache-keepalive-budget.md",
        "// carries the measured table and says plainly where a flat MaxPings still beats it.",
        "//",
    ]
    if degenerate:
        lines += [
            f"// Zero-variance in this frame, so the coefficient is exactly 0: "
            f"{', '.join(degenerate)}.",
            "// Kept in the vector rather than removed, so the feature order Go reads does not",
            "// move when a future re-fit on a different schedule makes the term informative.",
            "//",
        ]
    # gofmt folds a comment block that abuts `package` into the package doc and strips the
    # trailing "//". Emit the blank line it wants, so the generated file is already canonical
    # and `gofmt -l` in CI stays silent without a formatting pass this script cannot run.
    while lines and lines[-1] == "//":
        lines.pop()
    lines += [
        "",
        f"package {package}",
        "",
        "// ReuseModelV1 is the fitted, frozen reuse model the `keepalive-budget` arm reads.",
        "//",
        "// See reusemodel.go for what the fields mean and for the CDF this composes. The",
        "// numbers are a logistic survival fit on the corpus named above; the Python that",
        "// produced them, the feature arithmetic they assume, and this table are pinned",
        "// together by TestReuseModelV1AgreesWithThePort.",
        "var ReuseModelV1 = &ReuseModel{",
        f"\tVersion:   {_go_str(tr.get('version', 'v1'))},",
        f"\tTrainedOn: {_go_str(tr.get('window', ''))},",
        f"\tIntervalS: {_go_float(model.interval_s)},",
        f"\tLifeS:     {_go_float(model.life_s)},",
        f"\tMaxK:      {model.max_k},",
        f"\tIntercept: {_go_float(model.intercept)},",
        "\tNumeric: []ReuseTerm{",
    ]
    for name in SHIPPED_NUM:
        lines.append(f"\t\t{{Name: {_go_str(name)}, Coef: {_go_float(model.num_coef.get(name, 0.0))}, "
                     f"Center: {_go_float(model.num_center.get(name, 0.0))}, "
                     f"Scale: {_go_float(model.num_scale.get(name, 1.0))}}},")
    lines.append("\t},")
    lines.append("\tCategorical: []ReuseCategory{")
    for name in SHIPPED_CAT:
        lines.append(f"\t\t{{Name: {_go_str(name)}, Levels: []ReuseLevel{{")
        for level, c in sorted((model.cat_coef.get(name) or {}).items()):
            lines.append(f"\t\t\t{{Level: {_go_str(level)}, Coef: {_go_float(c)}}},")
        lines.append("\t\t}},")
    lines.append("\t},")
    lines.append("}")
    lines.append("")
    return "\n".join(lines)


def _go_str(s) -> str:
    """A Go string literal. json.dumps is exactly Go's quoting for the characters here."""
    return json.dumps("" if s is None else str(s))


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

    print(f"self-test OK   break-even {100 * be:.2f}%   "
          f"{len(FEATURES)} reference / {len(SHIPPED_NUM) + len(SHIPPED_CAT)} shipped features"
          f"   interval {DEFAULT_INTERVAL_S:.0f}s / life {DEFAULT_LIFE_S:.0f}s / max_k "
          f"{DEFAULT_MAX_K}")
    return 0


def _run_score(model_path: str, fixture_path: str) -> int:
    """Score SPANS through a fitted model and print JSON. Stdlib only, so the guard runs anywhere.

    This is the model layer's counterpart to `--fixture`. `--fixture` feeds a hand-written step
    CDF and therefore pins only the break-even/windows/induction arithmetic; the fitted model
    was never on either side of it. `--score` closes that hole: it drives the SAME coefficients
    the Go table carries, so a codegen slip, a feature-order change, a standardisation that Go
    forgot to apply, or a divergent sin/cos convention shows up as a number, not as a
    plausible-looking dashboard row.

    A fixture is {"cases":[{ts_ms, cached_tokens, since_last_ms, turn, stat_p, stat_n, model,
    input_rate, min_prefix (optional), horizons:[seconds, …]}]} — RAW fields, exactly what
    `kvcache.Observation` carries, so the feature DERIVATION is pinned too and not merely the
    dot product. The output carries the per-sweep survival vector, the CDF at each requested
    horizon, the per-window h/s, and the resulting budget — all four, because a budget that
    agrees for the wrong reason is a guard that has already stopped working.
    """
    with open(model_path, encoding="utf-8") as fh:
        model = ReuseModel.from_dict(json.load(fh))
    with open(fixture_path, encoding="utf-8") as fh:
        spec = json.load(fh)
    interval = float(spec.get("interval_s", model.interval_s))
    life = float(spec.get("life_s", model.life_s))
    max_k = int(spec.get("max_k", model.max_k))
    surv, cdfs, budgets, wins = [], [], [], []
    for case in spec["cases"]:
        span = span_from_raw(
            ts_ms=case["ts_ms"], cached_tokens=case["cached_tokens"],
            since_last_ms=case.get("since_last_ms", 0), turn=case.get("turn", 1),
            stat_p=case.get("stat_p", 0.0), stat_n=case.get("stat_n", 0),
            model=case.get("model", ""))
        surv.append([model.survival(span, j) for j in range(1, max_k + 2)])
        cdf = model.cdf_for(span, interval_s=interval, life_s=life, max_k=max_k)
        cdfs.append([cdf(float(h)) for h in case.get("horizons", [])])
        h, s = windows(cdf, interval_s=interval, life_s=life, max_k=max_k)
        wins.append({"h": h, "s": s})
        rates = Rates.from_input_rate(float(case.get("input_rate", 3.0e-6)))
        budgets.append(budget_for(cdf, int(case.get("cached_tokens", 0)), rates,
                                  interval_s=interval, life_s=life, max_k=max_k,
                                  min_prefix=int(case.get("min_prefix", 0))))
    print(json.dumps({"survival": surv, "cdf": cdfs, "windows": wins, "budgets": budgets}))
    return 0


def _run_fit(args) -> int:
    """Fit the shipped model on a real corpus and write the two artifacts.

    The JSON is the fit's own output and the Go file is generated from it, so the pair can be
    checked against each other — which is what `--score` plus
    TestReuseModelV1AgreesWithThePort do. Neither is loaded at runtime: the Go table is
    compiled in.
    """
    import datetime as _dt

    spans = load_spans(args.db, args.prices, since_ms=args.since, until_ms=args.until)
    model, report = fit_portable(spans, interval_s=args.interval, life_s=args.life,
                                 max_k=args.max_k, seed=args.seed, weight=args.weight)
    lo, hi = int(spans.ts_ms.min()), int(spans.ts_ms.max())
    fmt = "%Y-%m-%d %H:%M"

    def _utc(ms):
        return _dt.datetime.fromtimestamp(ms / 1000.0, _dt.timezone.utc).strftime(fmt)

    model.trained = {
        "version": args.version,
        # The basename, not the path. The window and the row counts are the provenance that
        # matters; whose home directory the snapshot happened to sit in is noise in a checked-in
        # artifact, and one more thing to scrub before sharing it.
        "corpus": os.path.basename(args.db),
        "window": f"{_utc(lo)} -> {_utc(hi)} UTC",
        "requests": int(len(spans)),
        "days": round((hi - lo) / 86_400_000.0, 1),
        "seed": args.seed,
        "weight": args.weight,
    }
    if args.model_out:
        payload = model.to_dict()
        payload["report"] = report
        with open(args.model_out, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, indent=1, sort_keys=True)
            fh.write("\n")
        print(f"wrote {args.model_out}", file=sys.stderr)
    if args.go_out:
        with open(args.go_out, "w", encoding="utf-8") as fh:
            fh.write(emit_go(model, report))
        print(f"wrote {args.go_out}", file=sys.stderr)
    lg = report["in_sample"]["logistic"]
    gb = report["in_sample"]["reference_gbm"]
    ho = report["holdout"]
    print(f"fit OK   spans {report['n_spans']}   reaching sweep 1 "
          f"{report['n_reached_first_sweep']}   person-periods {report['n_rows']}   "
          f"window {model.trained['days']}d")
    def line(tag, d):
        return (f"  {tag:19s} AUC row {d['auc_row']:.4f}   AUC dollar {d['auc_dollar']:.4f}"
                f"   logloss {d['logloss_row']:.4f}   mean p {d['mean_pred']:.4f}"
                f" vs actual {d['mean_actual']:.4f}")

    print(f"  fit weight          {report['weight']}")
    if "skipped" in ho:
        print(f"  holdout             skipped: {ho['skipped']}")
    else:
        print(line("holdout (late 30%)", ho) + "   <- the number to use")
    print(line("in-sample", lg))
    print(line("in-sample, ref GBM", gb) + "   (not shippable)")
    if report["degenerate"]:
        print(f"  zero-variance, coefficient forced to 0: {', '.join(report['degenerate'])}")
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--fixture", help="score a JSON fixture and print it (drift test)")
    ap.add_argument("--self-test", action="store_true",
                    help="assert the module's claims on hand-written distributions")
    ap.add_argument("--score", metavar="FIXTURE",
                    help="score spans through --model and print it (model drift test)")
    ap.add_argument("--model", metavar="JSON", default=DEFAULT_MODEL,
                    help=f"the fitted model --score reads (default {DEFAULT_MODEL})")
    ap.add_argument("--db", help="fit the shipped model on this cg.db (read-only)")
    ap.add_argument("--prices", default=DEFAULT_PRICES,
                    help=f"the operator price list (default {DEFAULT_PRICES})")
    ap.add_argument("--since", type=int, help="only requests at or after this epoch ms")
    ap.add_argument("--until", type=int, help="only requests before this epoch ms")
    ap.add_argument("--interval", type=float, default=DEFAULT_INTERVAL_S,
                    help="keep-alive cadence in seconds")
    ap.add_argument("--life", type=float, default=DEFAULT_LIFE_S,
                    help="the tier's lifetime in seconds")
    ap.add_argument("--max-k", type=int, default=DEFAULT_MAX_K,
                    help="how far the induction looks ahead")
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--weight", choices=("none", "stake"), default="stake",
                    help="fit sample weight; `stake` is the measured winner, see _weights")
    ap.add_argument("--version", default="v1", help="the version label recorded in the model")
    ap.add_argument("--model-out", help="write the fitted model as JSON")
    ap.add_argument("--go-out", help="write the fitted model as Go source")
    args = ap.parse_args(argv)
    if args.fixture:
        return _run_fixture(args.fixture)
    if args.score:
        return _run_score(args.model, args.score)
    if args.db:
        return _run_fit(args)
    if args.self_test:
        return _self_test()
    ap.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
