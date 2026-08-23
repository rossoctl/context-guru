#!/usr/bin/env python3
"""What a KV-cache TTL policy COSTS on a real trajectory, at the operator's real rates.

Give it a trajectory and one action per turn — let the cache expire, write at 5 minutes,
write at an hour, or hold it with keep-alive pings — and it returns the bill, decomposed.
Nothing here decides anything. It is the scorer, so that a predictor, a threshold rule and
a hand-written policy are all judged by the same arithmetic.

It exists to answer one question: does the survival predictor in
kv_ttl_survival_predictor.py actually save money on real traffic? A predictor emits
probabilities; only a cost model can turn those into dollars, and only a cost model that
replays REAL trajectories at REAL per-model rates can turn them into dollars anyone should
act on.

Usage: kv_ttl_cost_model.py [--db PATH] [--prices PATH] [--since ISO] [--until ISO]
                            [--baseline NAME] [--split FRACTION] [--no-predictor]
                            [--json OUT.json]

       from kv_ttl_cost_model import PriceBook, load_trajectories, evaluate, POLICIES


WHY IT IS A PORT AND NOT THE ORIGINAL
-------------------------------------
The shipped implementation of this arithmetic is Go: `kvcache.Simulate` in this repo,
which is what the dashboard's KV-cache page reports from. Two implementations of one money
question is exactly the drift this project has been bitten by, so this file is a FAITHFUL
PORT rather than a second opinion, and `kv_ttl_cost_drift_test.go` beside it replays the
same trajectory and the same action list through both and fails if the totals differ by
more than a hundredth of a cent. It is in Python only because the predictor is: an ML
evaluation loop that had to shell into Go for every candidate threshold would not get run.

If the two ever disagree, Go is right and this file is the bug.


WHICH DATA IT NEEDS
-------------------
One row per request. `load_trajectories` reads them from context-guru's own dashboard
store (`DASHBOARD_DB`, `/var/lib/context-guru/cg.db` on the hosted deployment), READ-ONLY:

    user             <- requests.tenant_id      the owning account
    conversation     <- requests.session_id     the trajectory; a cache entry lives here
    request_id       <- requests.id
    ts_ms            <- requests.ts             epoch MILLISECONDS, UTC
    model            <- requests.model          picks the rates; see PriceBook
    input_tokens     <- requests.fresh_input    uncached input, billed every turn
    output_tokens    <- requests.output_tokens
    cached_context   <- requests.cache_read + requests.cache_write
                        the BILLED PREFIX: the size of the entry that exists after this
                        request, and the number every write, read and ping is priced on.
                        NOT tokens_before, which is message text only and runs a median
                        3.4x low.
    miss_reason      <- requests.cache_miss_reason
    upstream_ms      <- requests.upstream_ms    optional, for the latency figure
    billed_usd       <- requests.cost_usd       what the deployment ACTUALLY paid, priced
                        at write time. Carried for comparison only; never used as an input
                        to a simulated cost.

A conversation is the PAIR (user, conversation). The session id is client-supplied, so two
accounts can present the same one, and keying on it alone would splice their traffic into
one trajectory and derive idle gaps across the join.


WHICH PRICING IT USES
---------------------
The operator's own price list — `/etc/context-guru/prices.yaml` on this deployment, the
file `MODEL_PRICES` points at — parsed with the same rules as internal/modelinfo/table.go:
rates are USD per MILLION tokens, a trailing `*` matches a family, the LONGEST match wins,
an exact id beats everything, a non-`*` entry is matched by containment only when it looks
like a qualified id (it contains `/` or `.`), and `cache_read_frac` / `cache_write_frac`
fill in the tiers an entry does not state.

That file matters rather than being a detail: this gateway bills aws/claude-sonnet-5 at
$1.52/MTok where anthropic.com bills $3.00, so the public list price would overstate every
figure below by about 2x.

The one rate NO price list publishes is the 1-hour cache write. It is derived from the
documented multiplier against base input (2.0x, against 1.25x for the 5-minute tier), and
the multiplier is a parameter — see PriceBook(write_1h_multiple=...). A model the file has
no entry for comes back `known=False`, and then it contributes to NO dollar figure at all
and is counted in `unpriced`: an unpriced model is not a free one.


THE COST FORMULAS
-----------------
Per request, under the tier the action chose:

    uncached_input = input_tokens              x input_rate
    cache_read     = read_tokens               x cache_read_rate
    cache_write    = written_tokens            x write_rate(tier)   # 5m: 1.25x in, 1h: 2.0x in
    output         = output_tokens             x output_rate
    request_cost   = uncached_input + cache_read + cache_write + output

`read_tokens` is min(entry held, this request's prefix) when the entry is still alive, else
0. `written_tokens` is the rest of the prefix. With no cache_control at all the whole
prefix is billed as fresh input instead, which is what makes "expire" a real arm and not an
absence.

Per keep-alive:

    keep_alive  = cached x cache_read_rate + ping_input x input_rate + ping_output x output_rate
    recreate    = cached x write_rate(tier) + ping_input x input_rate + ping_output x output_rate

A keep-alive is a cache READ and costs the read rate — the same whether the entry is held
at five minutes or at an hour. The difference between a 5m and a 1h keep-alive is not the
price of one ping; it is the creation tier that put the entry there and how OFTEN a ping is
needed (12x more often at five minutes). A keep-alive that arrives after the entry lapsed
is not a refresh at all: it RE-CREATES the prefix at the write rate, 12.5x a read at the
5-minute tier and 20x at the one-hour tier, and `pings_that_rewrote` counts those.

`ping_output` is 1 token because Anthropic's Messages API requires max_tokens >= 1. A
provider that accepts a zero-generation request sets Semantics(zero_generation=True) and
the assumption disappears from the bill instead of being rounded away.

Totals, and the two that are easy to conflate:

    total_usd         = sum(request_cost) + sum(ping_cost)
    uncached_usd      = the same traffic with no prompt cache at all
    cache_premium_usd = total_usd - uncached_usd
                        what the caching machinery itself cost. NEGATIVE means the cache
                        paid for itself. This is not the same number as total_usd and the
                        two must never be reported as if they were.

    absolute_savings   = baseline_total - policy_total
    percentage_savings = absolute_savings / baseline_total x 100

Savings are NOT clamped. A policy that costs more than its baseline reports a negative
saving, because that is the only way a comparison stays one.


CACHE SEMANTICS, MADE EXPLICIT
------------------------------
Anthropic's documented behaviour is the default, and each part is a field so a provider
that differs can say so rather than being silently mispriced:

  * a cache HIT refreshes the entry for its tier's full lifetime, free
    (Semantics.hit_refreshes_ttl)
  * a keep-alive read does the same (Semantics.ping_refreshes_ttl)
  * a refresh does not UPGRADE a tier: a five-minute entry refreshed is good for another
    five minutes, not an hour
  * `cache_miss_reason` of 'prefix_change' or 'cold_start' FORCES a miss whatever the
    policy chose. No TTL rescues content that moved, and none conjures an entry that never
    existed. Those rows are the ceiling on every arm and `forced_misses` reports them.


WHAT AN ACTION IS
-----------------
Five, and they are the whole set of things a prompt-caching provider sells:

    'expire'    write no cache_control; this prompt is billed as fresh input
    'write_5m'  write/retain the prefix at the 5-minute tier
    'write_1h'  write/retain the prefix at the 1-hour tier
    'ping_5m'   hold at 5 minutes AND refresh with keep-alives while idle
    'ping_1h'   hold at an hour AND refresh with keep-alives while idle

`evaluate(rows, actions)` takes `actions` as either a list parallel to the chronological
row order, or a dict keyed by `request_id`. TTL SECONDS are accepted as a convenience — 0,
300 and 3600 mean 'expire', 'write_5m' and 'write_1h' — so a policy that thinks in
lifetimes needs no translation layer. A row with no action defaults to 'expire', which is
the arm that cannot flatter anything.


INPUT AND OUTPUT
----------------
load_trajectories(db_path, since=None, until=None, include_keepalive=False) -> list[Request]
    Read-only. Chronological, by (ts, id). Drops rows with no session and rows whose token
    accounting is 'missing' (the provider never reported usage, so there is nothing to
    price). Drops keep-alive PING rows where the column exists: a ping is a request
    context-guru sent while nobody was at the keyboard, and counting it as a turn would
    both invent traffic and break the idle gap it sits inside.

evaluate(rows, actions, prices, semantics=..., schedule=..., window_end_ms=None) -> Cost
    Cost carries: total_usd and its five components (fresh_input_usd, cache_read_usd,
    cache_write_usd, output_usd, ping_usd); uncached_usd and cache_premium_usd; requests,
    conversations, unpriced; hits, misses, hit_rate_pct, miss_rate_pct, forced_misses;
    pings, pings_that_rewrote, pings_on_open_spans; writes_5m, writes_1h, expires;
    avoided_recomputations, avoided_tokens; retained_ms; and per-user / per-model
    breakdowns in by_user / by_model.

compare(baseline: Cost, policy: Cost) -> Savings
    absolute_usd (signed), percent_usd, percent_known, hit_delta.

MEASURED ON REAL TRAFFIC
------------------------
The hosted deployment's own capture (14,407 requests, 12 accounts, 1,772 trajectories,
2026-08-17 11:48 -> 2026-08-19 20:38 UTC) at `/etc/context-guru/prices.yaml`. The predictor
is fitted on the first 70% of the window and its thresholds tuned there; every arm is then
scored on the held-out 5,722 requests, whose `observed` bill is $1,857.78:

    policy          total        vs observed     hit %   what it does
    no-cache        $3,975.58    -114.00%          0.0%  never writes cache_control
    fixed-5m        $1,857.78      +0.00%         76.4%
    fixed-1h        $2,560.08     -37.80%         78.1%  the 2.0x premium never repays
    keepalive-5m    $1,805.28      +2.83%         77.5%  5m tier + up to 2 refreshes
    keepalive-1h    $2,551.24     -37.33%         78.2%
    predictor       $1,857.91      -0.01%         76.2%  threshold ladder on the ML model
    expected-cost   $1,857.78      +0.00%         76.4%  expected-cost rule on the same model
    optimal         $1,718.46      +7.50%         76.6%  UNREACHABLE: exact, sees the future

Three things that table settles, and the first is the one worth knowing:

  1. PROMPT CACHING ITSELF IS THE WIN, AND IT IS ALREADY BANKED. no-cache costs 114% more
     than the shipped 5-minute policy: the cache is saving 53% of this traffic's bill
     already. Everything else on this page is a fight over the remainder.
  2. THE WHOLE TTL HEADROOM IS 7.5%, AND THAT FIGURE REQUIRES PERFECT FORESIGHT. A blind
     keep-alive with no model at all takes 2.83% of it — 38% of the ceiling, for free.
  3. THE MACHINE-LEARNED ARMS TAKE NOTHING (-0.01% and +0.00%), and not because the model
     is bad: it halves the Brier score of the base rate (see kv_ttl_survival_predictor.py).
     They take nothing because 92.5% of production gaps close inside five minutes, so the
     5-minute tier is right for almost every request and there is no decision left for a
     probability to improve.

Where the 7.5% actually is, by replaying the optimum's own choices one kind at a time:

    only its 70 one-hour writes   +5.04%   ($93.61)
    only its 22 keep-alive pings  +1.72%   ($31.89)
    only its 1,246 expiries       +0.69%   ($12.85)

So 93% of the reachable headroom is 92 decisions out of 5,722, on the rare conversation
that goes quiet for longer than five minutes and comes back to a very large prefix. That
makes this a RARE-EVENT problem, and it says what a useful predictor would have to be: not
well calibrated on average, but precise on the tail, and weighted by prefix size. A model
scored on average calibration will look good and buy nothing — which is exactly what the
two ML arms above did.


POLICIES maps a name to a callable (rows, ctx) -> list[Action], so a new arm is one
function. 'oracle' is included and is deliberately labelled unreachable: it reads the true
next-request time, so it is the CEILING no predictor can pass, not a result.
"""

from __future__ import annotations

import argparse
import json
import math
import sqlite3
from collections import defaultdict
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, Sequence

# ── actions and tiers ──────────────────────────────────────────────────────

EXPIRE = "expire"
WRITE_5M = "write_5m"
WRITE_1H = "write_1h"
PING_5M = "ping_5m"
PING_1H = "ping_1h"
# Write cheap, extend only if the conversation actually goes quiet: the request creates a
# 5-minute entry at 1.25x input, and a keep-alive that comes due before it lapses extends the
# context by an hour with a 1-hour WRITE at 2.0x. Distinct from PING_1H, which pays that 2.0x
# on every request whether or not the hold is ever needed.
WRITE_5M_PING_1H = "write_5m_ping_1h"
ACTIONS = (EXPIRE, WRITE_5M, WRITE_1H, PING_5M, PING_1H, WRITE_5M_PING_1H)

TTL_NONE, TTL_5M, TTL_1H = "", "ephemeral_5m", "ephemeral_1h"
LIFETIME_MS = {TTL_NONE: 0, TTL_5M: 5 * 60 * 1000, TTL_1H: 60 * 60 * 1000}

# The tier the REQUEST ITSELF writes at — not necessarily where the entry ends up.
_TIER_OF = {EXPIRE: TTL_NONE, WRITE_5M: TTL_5M, PING_5M: TTL_5M,
            WRITE_1H: TTL_1H, PING_1H: TTL_1H, WRITE_5M_PING_1H: TTL_5M}
# The tier this action's keep-alives hold the entry at, which is what a ping must pay to reach.
_PING_TIER_OF = {PING_5M: TTL_5M, PING_1H: TTL_1H, WRITE_5M_PING_1H: TTL_1H}
# TTL seconds a caller may pass instead of an action name.
_ACTION_OF_SECONDS = {0: EXPIRE, 300: WRITE_5M, 3600: WRITE_1H}

HORIZON_5M_MS = 5 * 60 * 1000
HORIZON_1H_MS = 60 * 60 * 1000

# A miss with one of these recorded reasons could not have been prevented by any TTL: the
# content moved, or there was no entry to begin with.
UNRESCUABLE = frozenset({"prefix_change", "cold_start"})


def tier_of(action: str) -> str:
    """The TTL tier an action holds the entry at."""
    if action not in _TIER_OF:
        raise ValueError(f"unknown action {action!r}; expected one of {ACTIONS}")
    return _TIER_OF[action]


def pings(action: str) -> bool:
    """Whether an action sends keep-alives during the idle span."""
    return action in (PING_5M, PING_1H, WRITE_5M_PING_1H)


def ping_tier(action: str) -> str:
    """The tier this action's keep-alives aim to hold the entry at."""
    return _PING_TIER_OF.get(action, TTL_NONE)


def normalize_action(value: Any) -> str:
    """Accept an action name or a TTL in seconds, and reject anything else loudly.

    A silent fallback here would turn a typo into an arm that quietly never caches, which
    is the most flattering possible failure for a savings figure.
    """
    if isinstance(value, str):
        if value in _TIER_OF:
            return value
        raise ValueError(f"unknown action {value!r}; expected one of {ACTIONS}")
    if isinstance(value, bool):
        raise ValueError("an action is not a boolean")
    if isinstance(value, (int, float)):
        seconds = int(value)
        if seconds in _ACTION_OF_SECONDS:
            return _ACTION_OF_SECONDS[seconds]
        raise ValueError(
            f"{seconds}s is not a tier this provider sells; use 0, 300 or 3600, or an "
            f"action name for the keep-alive arms")
    raise ValueError(f"cannot read {value!r} as an action")


# ── pricing ────────────────────────────────────────────────────────────────

PER_MTOK = 1e6
DEFAULT_CACHE_READ_MULTIPLE = 0.1
DEFAULT_WRITE_5M_MULTIPLE = 1.25
DEFAULT_WRITE_1H_MULTIPLE = 2.0
DEFAULT_PING_INPUT_TOKENS = 1
DEFAULT_PING_OUTPUT_TOKENS = 1

SOURCE_PRICE_LIST = "price_list"


@dataclass(frozen=True)
class ModelPricing:
    """One model's per-TOKEN rates, and the two assumptions a ping forces into the open."""

    model: str
    input: float = 0.0
    output: float = 0.0
    cache_read: float = 0.0
    write_5m: float = 0.0
    write_1h: float = 0.0
    ping_input_tokens: int = DEFAULT_PING_INPUT_TOKENS
    ping_output_tokens: int = DEFAULT_PING_OUTPUT_TOKENS
    source: str = ""
    known: bool = False

    def write_rate(self, tier: str) -> float:
        """The creation rate for one tier. TTL_NONE creates nothing, so it is 0 — the
        caller must have billed those tokens as fresh input instead."""
        if tier == TTL_5M:
            return self.write_5m
        if tier == TTL_1H:
            return self.write_1h
        return 0.0

    def request_cost(self, input_tokens: int, read: int, write: int, output: int,
                     tier: str) -> float:
        return (input_tokens * self.input + read * self.cache_read
                + write * self.write_rate(tier) + output * self.output)

    def keep_alive_cost(self, cached: int, sem: "Semantics") -> float:
        return (cached * self.cache_read + self.ping_input_tokens * self.input
                + sem.ping_output(self) * self.output)

    def recreate_cost(self, cached: int, tier: str, sem: "Semantics") -> float:
        return (cached * self.write_rate(tier) + self.ping_input_tokens * self.input
                + sem.ping_output(self) * self.output)

    def uncached_cost(self, input_tokens: int, prefix: int, output: int) -> float:
        return (input_tokens + prefix) * self.input + output * self.output

    def hold_cost(self, cached: int, tier: str, n_pings: int, sem: "Semantics") -> float:
        """What it costs to put `cached` tokens at `tier` and hold them with n_pings
        refreshes. The figure to quote beside a rate: a dollar amount on a real prefix."""
        return cached * self.write_rate(tier) + n_pings * self.keep_alive_cost(cached, sem)


@dataclass(frozen=True)
class Semantics:
    """The provider's cache behaviour. Defaults are Anthropic's documented behaviour."""

    hit_refreshes_ttl: bool = True
    ping_refreshes_ttl: bool = True
    zero_generation: bool = False

    def ping_output(self, price: ModelPricing) -> int:
        if self.zero_generation:
            return 0
        return max(0, price.ping_output_tokens)


@dataclass(frozen=True)
class PingSchedule:
    """When a keep-alive fires, per tier, and how many fire per idle span.

    Both intervals sit a little INSIDE the lifetime they protect, so a refresh lands before
    the deadline rather than on it. A one-hour entry therefore needs one twelfth as many
    refreshes to be held for the same wall-clock span, which is the ping-count half of the
    5m-versus-1h trade.
    """

    idle_5m_ms: int = 280 * 1000
    idle_1h_ms: int = 3360 * 1000
    max_pings: int = 2

    def interval_ms(self, tier: str) -> int:
        return self.idle_1h_ms if tier == TTL_1H else self.idle_5m_ms


def pings_per_span(gap_ms: int, interval_ms: int, max_pings: int) -> int:
    """How many keep-alives one idle span attracts.

    The first fires one interval after the last activity, each subsequent one an interval
    after that, and max_pings caps the count. A span no longer than one interval attracts
    none. Mirrors kvcache.PingsPerSpan; the drift test asserts they agree.
    """
    if interval_ms <= 0 or max_pings <= 0 or gap_ms <= interval_ms:
        return 0
    n = math.floor((gap_ms - interval_ms) / interval_ms) + 1
    return min(n, max_pings)


class PriceBook:
    """Per-model rates, resolved from the operator's price list by the model on the row.

    The lookup is a port of internal/modelinfo.Table.lookup and its subtleties are load
    bearing, not incidental:

      * an EXACT match on the full id or on its last path segment wins outright;
      * otherwise entries compete in ONE pass ordered LONGEST MATCH FIRST, so
        `aws/claude-opus-4-8` beats `aws/claude-opus-4*` and the file's own order cannot
        make a lookup ambiguous;
      * a `*` entry matches only as a prefix;
      * a non-`*` entry is matched by containment ONLY when it looks like a qualified model
        id — it contains a `/` or a `.`. Without that, tier names that are ordinary English
        (`fast`, `premium`) claim every unrelated id containing them, and a confidently
        wrong price is worse than a missing one.
    """

    def __init__(self, entries: Sequence[tuple[str, bool, bool, ModelPricing]] | None = None,
                 *, write_1h_multiple: float = DEFAULT_WRITE_1H_MULTIPLE,
                 ping_input_tokens: int = DEFAULT_PING_INPUT_TOKENS,
                 ping_output_tokens: int = DEFAULT_PING_OUTPUT_TOKENS):
        self._entries = list(entries or ())
        self.write_1h_multiple = float(write_1h_multiple)
        self.ping_input_tokens = int(ping_input_tokens)
        self.ping_output_tokens = int(ping_output_tokens)
        self._overrides: dict[str, ModelPricing] = {}
        self._cache: dict[str, ModelPricing] = {}

    # -- construction -------------------------------------------------------

    @classmethod
    def from_operator_file(cls, path: str | Path, **kwargs: Any) -> "PriceBook":
        """Load `/etc/context-guru/prices.yaml` (or whatever MODEL_PRICES names).

        A missing or malformed file is an ERROR, never a silent fallback: a price list that
        failed to load looks exactly like "every model is free".
        """
        import yaml  # only this constructor needs it

        raw = yaml.safe_load(Path(path).read_text(encoding="utf-8"))
        if not isinstance(raw, Mapping):
            raise ValueError(f"{path}: not a mapping")
        read_frac = float(raw.get("cache_read_frac") or DEFAULT_CACHE_READ_MULTIPLE)
        write_frac = float(raw.get("cache_write_frac") or DEFAULT_WRITE_5M_MULTIPLE)
        book = cls(**kwargs)
        entries: list[tuple[str, bool, bool, ModelPricing]] = []
        for i, m in enumerate(raw.get("models") or ()):
            match = str(m.get("match", "")).strip().lower()
            if not match:
                raise ValueError(f"{path}: entry {i} has no match")
            prefix = match.endswith("*")
            match = match.rstrip("*")
            in_r, out_r = float(m.get("in") or 0.0), float(m.get("out") or 0.0)
            cr, cw = float(m.get("cache_read") or 0.0), float(m.get("cache_write") or 0.0)
            if min(in_r, out_r, cr, cw) < 0:
                raise ValueError(f"{path}: {match!r} has a negative rate")
            if in_r == 0 and out_r == 0:
                raise ValueError(
                    f"{path}: {match!r} has no rates; omit the entry rather than pricing "
                    f"it free")
            price = ModelPricing(
                model=match, input=in_r / PER_MTOK, output=out_r / PER_MTOK,
                cache_read=(cr / PER_MTOK) or (in_r / PER_MTOK) * read_frac,
                write_5m=(cw / PER_MTOK) or (in_r / PER_MTOK) * write_frac,
                ping_input_tokens=book.ping_input_tokens,
                ping_output_tokens=book.ping_output_tokens,
                source=SOURCE_PRICE_LIST, known=True)
            price = replace(price, write_1h=book._derive_write_1h(price))
            qualified = ("/" in match) or ("." in match)
            entries.append((match, prefix, qualified, price))
        # Longest match first: specificity is a property of the match, not of the file's
        # ordering or of the KIND of match.
        entries.sort(key=lambda e: len(e[0]), reverse=True)
        book._entries = entries
        return book

    def _derive_write_1h(self, p: ModelPricing) -> float:
        """The 1-hour creation rate, the one rate no price list publishes.

        Derived from the multiplier against base INPUT, because that is how the provider
        documents it, and never allowed below the 5-minute rate: a list that implied
        otherwise would be a typo, and honouring it would make every 1h arm look free.
        """
        if p.input > 0:
            w = p.input * self.write_1h_multiple
        elif p.write_5m > 0:
            w = p.write_5m * (self.write_1h_multiple / DEFAULT_WRITE_5M_MULTIPLE)
        else:
            w = 0.0
        return max(w, p.write_5m)

    def override(self, model: str, **rates: float) -> None:
        """Price a model by hand — a preview id, an internal route name, a server-resolved
        tier the file has never heard of. An override makes the model KNOWN."""
        base = self.for_model(model)
        p = replace(base, **rates)
        if "write_1h" not in rates:
            p = replace(p, write_1h=self._derive_write_1h(p))
        p = replace(p, source="override",
                    known=any((p.input, p.output, p.cache_read, p.write_5m, p.write_1h)))
        self._overrides[model] = p
        self._cache.pop(model, None)

    # -- lookup -------------------------------------------------------------

    def for_model(self, model: str) -> ModelPricing:
        """The rates for one model, read off the trajectory's own `model` field.

        A model with no entry comes back known=False, NAMING ITSELF, so a caller can report
        which model it could not price rather than reporting a total that quietly excluded
        it.
        """
        if model in self._overrides:
            return self._overrides[model]
        hit = self._cache.get(model)
        if hit is not None:
            return hit
        p = self._lookup(model)
        self._cache[model] = p
        return p

    def _lookup(self, model: str) -> ModelPricing:
        full = (model or "").strip().lower()
        tail = full.rsplit("/", 1)[-1]
        for match, _prefix, _qual, price in self._entries:
            if match == full or match == tail:
                return replace(price, model=model)
        for match, prefix, qualified, price in self._entries:  # longest first
            if prefix:
                if full.startswith(match) or tail.startswith(match):
                    return replace(price, model=model)
                continue
            if qualified and match in full:
                return replace(price, model=model)
        return ModelPricing(model=model, ping_input_tokens=self.ping_input_tokens,
                            ping_output_tokens=self.ping_output_tokens)


# ── the trajectory ─────────────────────────────────────────────────────────

@dataclass
class Request:
    """One request of a trajectory, as the cost model needs it."""

    request_id: int
    user: str
    conversation: str
    ts_ms: int
    model: str
    input_tokens: int = 0
    output_tokens: int = 0
    cached_context: int = 0
    miss_reason: str = ""
    hit: bool = False
    upstream_ms: float = 0.0
    billed_usd: float = 0.0
    # The tier this request ACTUALLY asked for, where the store recorded it (`cache_ttl`).
    # None means the column did not exist when the row was written — a THIRD state next to
    # "5m" and "no cache_control", and the reason the `observed` arm reports its coverage
    # instead of quietly assuming a default.
    ttl_recorded: str | None = None
    # Derived by derive(): the next request in the SAME conversation.
    next_ts_ms: int | None = None
    idle_ms: int | None = None

    @property
    def key(self) -> tuple[str, str]:
        return (self.user, self.conversation)

    @property
    def hour_utc(self) -> int:
        return int((self.ts_ms // 3_600_000) % 24)


def derive(rows: list[Request]) -> list[Request]:
    """Fill next_ts_ms / idle_ms per conversation and return the rows in wall-clock order.

    Three properties, each a way this has been got wrong: grouped by (user, conversation)
    so a later request from another trajectory is never treated as this one's return;
    ordered by (ts, id) so tied timestamps break deterministically and a zero-length gap
    stays zero instead of going negative; and the LAST request of a conversation keeps
    idle_ms None — not 0, which would read as "it came back instantly".
    """
    by_conv: dict[tuple[str, str], list[Request]] = defaultdict(list)
    for r in rows:
        r.next_ts_ms, r.idle_ms = None, None
        by_conv[r.key].append(r)
    for group in by_conv.values():
        group.sort(key=lambda r: (r.ts_ms, r.request_id))
        for a, b in zip(group, group[1:]):
            a.next_ts_ms = b.ts_ms
            a.idle_ms = max(0, b.ts_ms - a.ts_ms)
    return sorted(rows, key=lambda r: (r.ts_ms, r.request_id))


DEFAULT_DB = "/var/lib/context-guru/cg.db"
DEFAULT_PRICES = "/etc/context-guru/prices.yaml"


def load_trajectories(db_path: str | Path = DEFAULT_DB, *, since_ms: int | None = None,
                      until_ms: int | None = None,
                      include_keepalive: bool = False) -> list[Request]:
    """Read the trajectories from the dashboard store, READ-ONLY, chronologically."""
    uri = f"file:{Path(db_path).as_posix()}?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        have = {r[1] for r in con.execute("PRAGMA table_info(requests)")}
        conds = ["session_id <> ''", "token_accounting <> 'missing'"]
        args: list[Any] = []
        # A ping is a request context-guru sent while nobody was at the keyboard. Counted as
        # a turn it invents traffic and breaks the idle gap it sits inside. Older snapshots
        # predate the column and contain no pings.
        if "keepalive" in have and not include_keepalive:
            conds.append("keepalive = 0")
        if since_ms is not None:
            conds.append("ts >= ?")
            args.append(since_ms)
        if until_ms is not None:
            conds.append("ts < ?")
            args.append(until_ms)
        cols = ["id", "tenant_id", "session_id", "ts", "model", "fresh_input",
                "output_tokens", "cache_read", "cache_write", "cache_miss_reason",
                "upstream_ms", "cost_usd"]
        # cache_ttl arrived as an additive column, so an older snapshot has no opinion about
        # the tier a request asked for. Selected only where it exists; absent it stays None.
        ttl_col = "cache_ttl" in have
        if ttl_col:
            cols.append("cache_ttl")
        sql = (f"SELECT {', '.join(cols)} FROM requests WHERE {' AND '.join(conds)} "
               f"ORDER BY ts, id")
        rows = [
            Request(request_id=r[0], user=r[1], conversation=r[2], ts_ms=r[3], model=r[4],
                    input_tokens=r[5] or 0, output_tokens=r[6] or 0,
                    cached_context=(r[7] or 0) + (r[8] or 0), miss_reason=r[9] or "",
                    hit=(r[9] or "") == "hit", upstream_ms=r[10] or 0.0,
                    billed_usd=r[11] or 0.0,
                    ttl_recorded=(r[12] if ttl_col else None))
            for r in con.execute(sql, args)
        ]
    finally:
        con.close()
    return derive(rows)


# ── the result ─────────────────────────────────────────────────────────────

@dataclass
class Group:
    """One user's or one model's slice of a result."""

    key: str
    requests: int = 0
    total_usd: float = 0.0
    hits: int = 0
    misses: int = 0
    pings: int = 0
    ping_usd: float = 0.0
    writes_5m: int = 0
    writes_1h: int = 0
    unpriced: int = 0

    @property
    def hit_rate_pct(self) -> float:
        d = self.hits + self.misses
        return 100.0 * self.hits / d if d else 0.0

    @property
    def valued(self) -> bool:
        """As Cost.valued, one level down. Here so a consumer never has to spell the predicate
        a second time as `unpriced < requests` on every per-user and per-model row."""
        return self.requests > 0 and self.unpriced < self.requests


@dataclass
class Cost:
    """One policy's whole bill on one set of trajectories."""

    policy: str = ""
    requests: int = 0
    conversations: int = 0

    fresh_input_usd: float = 0.0
    cache_read_usd: float = 0.0
    cache_write_usd: float = 0.0
    output_usd: float = 0.0
    ping_usd: float = 0.0
    uncached_usd: float = 0.0

    hits: int = 0
    misses: int = 0
    forced_misses: int = 0

    pings: int = 0
    pings_that_rewrote: int = 0
    # Keep-alives that DELIBERATELY paid a longer tier's creation rate to extend a LIVE entry
    # — WRITE_5M_PING_1H's whole mechanism. Counted apart from pings_that_rewrote because the
    # two mean opposite things: one is a policy buying a hold it chose, the other is a
    # schedule repairing damage it caused.
    pings_that_upgraded: int = 0
    pings_on_open_spans: int = 0

    writes_5m: int = 0
    writes_1h: int = 0
    expires: int = 0

    avoided_recomputations: int = 0
    avoided_tokens: int = 0
    retained_ms: int = 0
    unpriced: int = 0

    decisions: dict[str, int] = field(default_factory=dict)
    by_user: dict[str, Group] = field(default_factory=dict)
    by_model: dict[str, Group] = field(default_factory=dict)

    @property
    def valued(self) -> bool:
        """Whether ANY of these requests could be priced at all.

        False means every dollar figure here is zero because nothing had RATES — not because
        nothing was spent. Stated rather than left to be derived from unpriced == requests,
        because a consumer that has to derive it will forget to: a cold start with no price map
        makes every arm cost 0.00 and turns the exact ceiling into "never cache anything",
        rendered in the style reserved for the cheapest plan that exists.
        """
        return self.requests > 0 and self.unpriced < self.requests

    @property
    def total_usd(self) -> float:
        return (self.fresh_input_usd + self.cache_read_usd + self.cache_write_usd
                + self.output_usd + self.ping_usd)

    @property
    def cache_premium_usd(self) -> float:
        """total - uncached: what the caching machinery itself cost. Negative means the
        cache paid for itself. NOT the same number as total_usd."""
        return self.total_usd - self.uncached_usd

    @property
    def hit_rate_pct(self) -> float:
        d = self.hits + self.misses
        return 100.0 * self.hits / d if d else 0.0

    @property
    def miss_rate_pct(self) -> float:
        d = self.hits + self.misses
        return 100.0 * self.misses / d if d else 0.0

    def to_dict(self) -> dict[str, Any]:
        out = {k: v for k, v in self.__dict__.items() if k not in ("by_user", "by_model")}
        out["total_usd"] = self.total_usd
        out["valued"] = self.valued
        out["cache_premium_usd"] = self.cache_premium_usd
        out["hit_rate_pct"] = self.hit_rate_pct
        out["miss_rate_pct"] = self.miss_rate_pct
        out["by_user"] = {k: g.__dict__ | {"hit_rate_pct": g.hit_rate_pct,
                                           "valued": g.valued}
                          for k, g in self.by_user.items()}
        out["by_model"] = {k: g.__dict__ | {"hit_rate_pct": g.hit_rate_pct,
                                           "valued": g.valued}
                           for k, g in self.by_model.items()}
        return out


@dataclass
class Savings:
    """One policy against one baseline. Nothing here is clamped."""

    policy: str
    baseline: str
    baseline_usd: float
    policy_usd: float
    absolute_usd: float
    percent_usd: float
    percent_known: bool
    hit_delta: int


def compare(baseline: Cost, policy: Cost) -> Savings:
    """absolute = baseline - policy ; percent = absolute / baseline * 100.

    A percentage of a zero baseline is UNDEFINED, not 0%, and percent_known says so.
    """
    absolute = baseline.total_usd - policy.total_usd
    known = baseline.total_usd != 0
    return Savings(
        policy=policy.policy, baseline=baseline.policy,
        baseline_usd=baseline.total_usd, policy_usd=policy.total_usd,
        absolute_usd=absolute,
        percent_usd=(absolute / baseline.total_usd * 100) if known else 0.0,
        percent_known=known, hit_delta=policy.hits - baseline.hits)


# ── the evaluator ──────────────────────────────────────────────────────────

@dataclass
class _Entry:
    """One conversation's simulated cache entry and the action governing its open span."""

    tokens: int = 0
    tier: str = TTL_NONE
    expires_ms: int = 0
    last_ts_ms: int = 0
    turn: int = 0
    pending: str = EXPIRE
    covered_until_ms: int = 0
    user: str = ""
    model: str = ""


def evaluate(rows: Sequence[Request], actions: Mapping[int, Any] | Sequence[Any],
             prices: PriceBook, *, semantics: Semantics | None = None,
             schedule: PingSchedule | None = None, window_end_ms: int | None = None,
             policy: str = "") -> Cost:
    """Replay a trajectory set under one action per turn and return the bill.

    `actions` is either a dict keyed by request_id or a sequence parallel to the
    chronological row order. Values are action names or TTL seconds. A row with no action
    defaults to 'expire' — the arm that cannot flatter anything.

    An OPEN idle span (a conversation whose last request is inside the window) has a length
    nobody knows, so its keep-alives are billed only up to window_end_ms and counted apart
    in pings_on_open_spans.
    """
    sem = semantics or Semantics()
    sched = schedule or PingSchedule()
    order = sorted(rows, key=lambda r: (r.ts_ms, r.request_id))
    if window_end_ms is None:
        window_end_ms = max((r.ts_ms for r in order), default=0)

    if isinstance(actions, Mapping):
        chosen = {int(k): normalize_action(v) for k, v in actions.items()}
    else:
        if len(actions) != len(order):
            raise ValueError(
                f"{len(actions)} actions for {len(order)} requests; pass a dict keyed by "
                f"request_id when the two cannot be lined up positionally")
        chosen = {r.request_id: normalize_action(a) for r, a in zip(order, actions)}

    out = Cost(policy=policy)
    states: dict[tuple[str, str], _Entry] = {}

    def group(d: dict[str, Group], k: str) -> Group:
        g = d.get(k)
        if g is None:
            g = Group(key=k)
            d[k] = g
        return g

    for r in order:
        st = states.get(r.key)
        if st is None:
            st = _Entry()
            states[r.key] = st
            out.conversations += 1
        price = prices.for_model(r.model)
        ug, mg = group(out.by_user, r.user), group(out.by_model, r.model)

        # 1. close the previous span: the keep-alives it attracted, and their effect on the
        #    entry's deadline, both of which have to land before hit/miss is decided.
        if st.turn > 0:
            _simulate_pings(out, ug, mg, st, price, sem, sched, r.ts_ms, window_end_ms,
                            open_span=False)

        # 2. hit or miss under THIS policy's own history.
        alive = st.tokens > 0 and r.ts_ms < st.expires_ms
        forced = r.miss_reason in UNRESCUABLE
        if forced:
            out.forced_misses += 1
        hit = alive and not forced
        reusable = min(st.tokens, r.cached_context) if hit else 0

        # 3. the action for the span that starts now.
        action = chosen.get(r.request_id, EXPIRE)
        out.decisions[action] = out.decisions.get(action, 0) + 1
        tier = tier_of(action)

        # 4. bill this request under the chosen tier.
        fresh, read, write = r.input_tokens, reusable, 0
        if tier == TTL_NONE:
            fresh += r.cached_context
            read = 0
        else:
            write = max(0, r.cached_context - reusable)
        out.requests += 1
        ug.requests += 1
        mg.requests += 1
        if not price.known:
            out.unpriced += 1
            ug.unpriced += 1
            mg.unpriced += 1
        else:
            out.fresh_input_usd += fresh * price.input
            out.cache_read_usd += read * price.cache_read
            out.cache_write_usd += write * price.write_rate(tier)
            out.output_usd += r.output_tokens * price.output
            out.uncached_usd += price.uncached_cost(
                r.input_tokens, r.cached_context, r.output_tokens)
            cost = price.request_cost(fresh, read, write, r.output_tokens, tier)
            ug.total_usd += cost
            mg.total_usd += cost
        if hit:
            out.hits += 1
            ug.hits += 1
            mg.hits += 1
            out.avoided_recomputations += 1
            out.avoided_tokens += read
        else:
            out.misses += 1
            ug.misses += 1
            mg.misses += 1
        # A WRITE is a cache-creation event, not a decision: a request that hit and
        # re-marked the same prefix wrote nothing. Decision counts live in `decisions`.
        if tier == TTL_NONE:
            out.expires += 1
        elif write > 0 and tier == TTL_5M:
            out.writes_5m += 1
            ug.writes_5m += 1
            mg.writes_5m += 1
        elif write > 0 and tier == TTL_1H:
            out.writes_1h += 1
            ug.writes_1h += 1
            mg.writes_1h += 1

        # 5. the entry this request leaves behind.
        if tier == TTL_NONE:
            st.tokens, st.tier, st.expires_ms = 0, TTL_NONE, 0
        else:
            st.tokens, st.tier = r.cached_context, tier
            life = LIFETIME_MS[tier]
            if hit and not sem.hit_refreshes_ttl:
                # A provider where a hit does NOT refresh keeps the entry's original
                # deadline; only a lapsed entry gets a new one.
                if st.expires_ms < r.ts_ms:
                    st.expires_ms = r.ts_ms + life
            else:
                st.expires_ms = r.ts_ms + life
            _retain(out, st, r.ts_ms, st.expires_ms, window_end_ms)
        st.last_ts_ms, st.pending, st.turn = r.ts_ms, action, st.turn + 1
        st.user, st.model = r.user, r.model

    # The OPEN spans, priced at the last request's own model and counted apart.
    for key in sorted(states):
        st = states[key]
        if st.turn == 0 or not pings(st.pending):
            continue
        price = prices.for_model(st.model)
        _simulate_pings(out, group(out.by_user, st.user), group(out.by_model, st.model),
                        st, price, sem, sched, window_end_ms, window_end_ms,
                        open_span=True)
    return out


def _retain(out: Cost, st: _Entry, from_ms: int, until_ms: int, window_end_ms: int) -> None:
    """Add one alive interval to the UNION already recorded, clipped to the window.

    A union rather than a sum of lifetimes: overlapping refreshes would otherwise count the
    same second twice.
    """
    if window_end_ms > 0 and until_ms > window_end_ms:
        until_ms = window_end_ms
    if st.covered_until_ms > from_ms:
        from_ms = st.covered_until_ms
    if until_ms > from_ms:
        out.retained_ms += until_ms - from_ms
    if until_ms > st.covered_until_ms:
        st.covered_until_ms = until_ms


@dataclass
class _PingOutcome:
    """What one idle span's keep-alives cost, and the entry they leave behind.

    Pure: it mutates nothing. Both the accounting path (`_simulate_pings`) and the exact
    optimum's dynamic program read this ONE implementation, so a second copy of the ping
    arithmetic cannot drift away from the first.
    """

    cost: float = 0.0
    fired: int = 0
    rewrote: int = 0
    upgraded: int = 0
    tier: str = TTL_NONE
    expires_ms: int = 0
    # (fired_at, new_expires) per ping, for the retention union.
    refreshes: list[tuple[int, int]] = field(default_factory=list)


def _ping_span(tokens: int, tier: str, expires_ms: int, last_ts_ms: int, pending: str,
               span_end_ms: int, price: ModelPricing, sem: Semantics,
               sched: PingSchedule) -> _PingOutcome:
    """The keep-alives that fire in (last_ts_ms, span_end_ms) under `pending`.

    The interval follows the entry's CURRENT tier, recomputed each time round, rather than the
    action's target tier fixed once. That matters only for WRITE_5M_PING_1H, whose entry starts
    at five minutes and becomes hourly partway through — so its first keep-alive has to land
    inside five minutes and the rest need only land inside an hour. For every other action the
    tier never moves and this is the same fixed cadence as before.
    """
    out = _PingOutcome(tier=tier, expires_ms=expires_ms)
    if not pings(pending) or span_end_ms <= last_ts_ms or tokens <= 0:
        return out
    want = ping_tier(pending)
    at = last_ts_ms
    for _ in range(sched.max_pings):
        step = sched.interval_ms(out.tier)
        if step <= 0:
            break
        at += step
        if at >= span_end_ms:
            break
        alive = at < out.expires_ms
        if not alive:
            # The entry lapsed before this keep-alive fired, so the "refresh" RE-CREATES it at
            # the tier the action was aiming for. Priced as a write, and counted, because a
            # schedule that does this is paying 12.5x to fix a problem it caused.
            out.tier = want
            cost = price.recreate_cost(tokens, out.tier, sem)
            out.rewrote += 1
        elif want == TTL_1H and out.tier != TTL_1H:
            # A deliberate UPGRADE of a live entry: pay the 1-hour creation rate now to hold it
            # for an hour instead of five minutes. The one case where a keep-alive is a write
            # on purpose rather than by accident.
            out.tier = TTL_1H
            cost = price.recreate_cost(tokens, TTL_1H, sem)
            out.upgraded += 1
        else:
            cost = price.keep_alive_cost(tokens, sem)
        out.fired += 1
        if price.known:
            out.cost += cost
        if sem.ping_refreshes_ttl or not alive:
            life = LIFETIME_MS[out.tier]
            if life > 0:
                out.expires_ms = at + life
                out.refreshes.append((at, out.expires_ms))
    return out


def _simulate_pings(out: Cost, ug: Group, mg: Group, st: _Entry, price: ModelPricing,
                    sem: Semantics, sched: PingSchedule, span_end_ms: int,
                    window_end_ms: int, *, open_span: bool) -> None:
    """Bill the keep-alives that fire between st.last_ts_ms and span_end_ms."""
    # Whatever happens below, this span is now settled: clearing the pending action is what
    # stops a conversation being charged twice when the open-span pass visits it as well.
    pending, st.pending = st.pending, EXPIRE
    r = _ping_span(st.tokens, st.tier, st.expires_ms, st.last_ts_ms, pending, span_end_ms,
                   price, sem, sched)
    if r.fired == 0:
        return
    st.tier, st.expires_ms = r.tier, r.expires_ms
    out.pings += r.fired
    out.pings_that_rewrote += r.rewrote
    out.pings_that_upgraded += r.upgraded
    if open_span:
        out.pings_on_open_spans += r.fired
    ug.pings += r.fired
    mg.pings += r.fired
    if price.known:
        out.ping_usd += r.cost
        ug.ping_usd += r.cost
        ug.total_usd += r.cost
        mg.ping_usd += r.cost
        mg.total_usd += r.cost
    for at, until in r.refreshes:
        _retain(out, st, at, until, window_end_ms)


# ── policies ───────────────────────────────────────────────────────────────
#
# A policy turns a trajectory into one action per turn. It is deliberately NOT part of the
# evaluator: the evaluator scores an action list, so a predictor, a threshold rule and a
# hand-written arm are all judged by identical arithmetic and a new idea is one function.
#
# Every policy here except `oracle` sees only what was knowable at the decision instant.
# `oracle` reads the true next-request time and is labelled unreachable everywhere it
# appears: it is the CEILING a predictor is measured against, not a result.


@dataclass
class PolicyContext:
    """What a policy is allowed to consult."""

    prices: PriceBook
    semantics: Semantics = field(default_factory=Semantics)
    schedule: PingSchedule = field(default_factory=PingSchedule)
    # p5m / p1h are a predictor's answers, keyed by request_id. Absent for the arms that do
    # not use one.
    p5m: Mapping[int, float] = field(default_factory=dict)
    p1h: Mapping[int, float] = field(default_factory=dict)
    threshold_5m: float = 0.5
    threshold_1h: float = 0.5
    # min_prefix is the prefix below which nothing is cached: a small prefix cannot repay a
    # write. On the production corpus the 10th-percentile prefix is 0 tokens.
    min_prefix: int = 20_000
    # Counters a policy may fill in for the report (e.g. the observed arm's coverage).
    notes: dict[str, Any] = field(default_factory=dict)


Policy = Callable[[Sequence[Request], PolicyContext], list[str]]


def policy_no_cache(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    return [EXPIRE] * len(rows)


def policy_fixed_5m(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    return [WRITE_5M] * len(rows)


def policy_fixed_1h(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    return [WRITE_1H] * len(rows)


def policy_keepalive_5m(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    """The shipped keep-alive: hold at five minutes and refresh while idle."""
    return [PING_5M] * len(rows)


def policy_keepalive_1h(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    """Hold at the one-hour tier and refresh while idle. Costs 2.0x input to create and
    needs one twelfth as many refreshes as the five-minute arm to hold the same span."""
    return [PING_1H] * len(rows)


def policy_extend_1h(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    """Write cheap, extend to an hour only when the conversation actually goes quiet.

    The arm the other two keep-alive arms bracket, and on traffic like this deployment's it is
    the one worth having: 92.5% of gaps close inside five minutes, so keepalive-1h pays the
    2.0x creation premium on nearly every request for a hold nearly none of them needs, while
    this pays 1.25x on every request and 2.0x only on the rare span that outlives five minutes.
    """
    return [WRITE_5M_PING_1H] * len(rows)


def policy_observed(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    """Replay the tier each request ACTUALLY asked for, where the store recorded it.

    A row whose tier was never recorded falls back to the provider's own default — a
    request that reached a prompt-caching provider with breakpoints in it got five minutes
    whether or not we wrote the header down — and the fallback is COUNTED, so the arm's
    coverage is reportable rather than assumed. On a snapshot predating the `cache_ttl`
    column that coverage is zero and this arm is `fixed-5m` under another name, which the
    report says out loud instead of presenting it as an independent result.
    """
    recorded = assumed = 0
    out: list[str] = []
    for r in rows:
        if r.ttl_recorded == TTL_1H:
            recorded += 1
            out.append(WRITE_1H)
        elif r.ttl_recorded == TTL_5M:
            recorded += 1
            out.append(WRITE_5M)
        elif r.ttl_recorded == TTL_NONE and r.ttl_recorded is not None:
            recorded += 1
            out.append(EXPIRE)
        else:
            assumed += 1
            out.append(WRITE_5M)
    ctx.notes["observed_recorded"] = recorded
    ctx.notes["observed_assumed"] = assumed
    return out


def _coverage_ms(action: str, sched: PingSchedule) -> int:
    """How long an action keeps an entry alive, at most.

        coverage = max_pings x interval + lifetime

    The trailing lifetime is LOAD BEARING: the last keep-alive is itself a cache read, and a
    read refreshes the entry for its tier's full lifetime. Leaving it out understates the
    reach of a ping schedule — the error that made an earlier analysis of this mechanism
    wrong by a factor of four.
    """
    if not pings(action):
        return LIFETIME_MS[tier_of(action)]
    # A pinging action's reach is set by the tier its keep-alives HOLD, not the tier the
    # request writes: WRITE_5M_PING_1H writes five minutes and its first keep-alive extends
    # that to an hour, so its coverage is the hourly one.
    held = ping_tier(action)
    return sched.max_pings * sched.interval_ms(held) + LIFETIME_MS[held]


def _long_hold(r: Request, ctx: PolicyContext, need_ms: int = HORIZON_1H_MS) -> str:
    """The cheapest action that actually holds this prefix for `need_ms`.

        write_1h = prefix x write_1h_rate                          (2.0x input, once)
        ping_5m  = prefix x write_5m_rate + K x keep_alive_cost     (1.25x plus K reads)

    The comparison is only ever made between candidates that REACH the horizon. A
    5-minute-plus-keep-alives hold with the shipped schedule (2 pings, 280 s apart) covers
    860 s — fourteen minutes, not an hour — so treating it as an alternative to a 1-hour
    write for an hour-long gap buys two pings AND misses anyway. That was the bug in the
    first version of this function, and it made the arm that used it score below a plain
    keep-alive.

    Unpriced: choose the 1-hour write, because a known one-off multiple beats an unbounded
    ping schedule at an unknown rate.
    """
    p = ctx.prices.for_model(r.model)
    candidates = [a for a in (WRITE_1H, PING_5M, PING_1H)
                  if _coverage_ms(a, ctx.schedule) >= need_ms]
    if not candidates:
        # Nothing on offer reaches that far; the longest reach is the honest choice.
        candidates = [max((WRITE_1H, PING_5M, PING_1H),
                          key=lambda a: _coverage_ms(a, ctx.schedule))]
    if not p.known:
        return WRITE_1H if WRITE_1H in candidates else candidates[0]

    def cost(action: str) -> float:
        n = ctx.schedule.max_pings if pings(action) else 0
        return p.hold_cost(r.cached_context, tier_of(action), n, ctx.semantics)

    return min(candidates, key=cost)


def policy_predictor(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    """Act on the survival predictor's two probabilities.

    P(return within 5 minutes) >= threshold_5m  -> a plain 5-minute write is enough; the
        entry survives on its own and a keep-alive would be pure waste.
    else P(return within an hour) >= threshold_1h -> hold it for an hour, by whichever of a
        1h write and a 5m write plus refreshes is cheaper on this prefix.
    else -> let it expire.

    A row the predictor has no answer for gets the provider's default rather than being
    dropped, and `predictor_unanswered` counts those.
    """
    unanswered = 0
    out: list[str] = []
    for r in rows:
        if r.cached_context < ctx.min_prefix:
            out.append(EXPIRE)
            continue
        p5 = ctx.p5m.get(r.request_id)
        p1 = ctx.p1h.get(r.request_id)
        if p5 is None or p1 is None:
            unanswered += 1
            out.append(WRITE_5M)
            continue
        if p5 >= ctx.threshold_5m:
            out.append(WRITE_5M)
        elif p1 >= ctx.threshold_1h:
            out.append(_long_hold(r, ctx))
        else:
            out.append(EXPIRE)
    ctx.notes["predictor_unanswered"] = unanswered
    return out


def policy_expected_cost(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    """Pick the action with the lowest EXPECTED cost, using the predictor's probability
    and this model's own rates. The rule the survival predictor is actually for.

    Why not a threshold ladder. `policy_predictor` asks "is P(return within 5m) above
    0.5?", and on real traffic that is the wrong question twice over. First, 92% of
    production gaps close inside five minutes, so the answer is almost always yes and the
    arm degenerates into fixed-5m. Second, the money is not in the typical request at all:
    it is in the rare one whose entry NOBODY WILL READ, where writing the prefix costs
    1.25x input for nothing when simply not caching it costs 1.0x. A ladder cannot express
    that, because the quantity it needs to compare is a cost, not a probability.

    So compare costs directly. Relative to letting the prefix expire — which bills it as
    fresh input at 1.0x — caching at tier T costs an extra

        premium(T) = prefix x (write_rate(T) - input_rate)

    now, and buys, IF the conversation returns while the entry is alive, a read instead of
    a re-creation on that next request:

        rescue = prefix x (write_5m_rate - cache_read_rate)

    `rescue` deliberately does NOT depend on T, and that is the correction that makes this
    rule work rather than invert. Pricing the avoided re-creation at write_rate(T) is
    CIRCULAR: it lets a 1-hour write inflate its own payoff, on the premise that the next
    miss would have re-created at the 1-hour rate — but the next request's tier is the next
    request's decision, not this one's. Priced at the FIVE-MINUTE rate it is the cheapest
    way to re-establish a prefix, so the figure is both non-circular and the conservative
    one. With the circular version in place this arm chose a 1-hour write 3,426 times out of
    3,699 and cost 36% MORE than a plain 5-minute policy.

    So caching at T is worth it exactly when

        P(return within coverage(T)) x rescue > premium(T) + ping_cost(T)

    and among the actions that clear that bar the lowest expected total wins. `ctx.threshold_5m`
    and `ctx.threshold_1h` are IGNORED here: the rates set the thresholds, which is the point.

    A row the predictor has no answer for falls back to the provider's default and is
    counted in `expected_cost_unanswered`. An unpriced model cannot be reasoned about at
    all, so it also takes the default rather than being decided by an arithmetic on zeroes.
    """
    unanswered = unpriced = 0
    out: list[str] = []
    for r in rows:
        price = ctx.prices.for_model(r.model)
        p5 = ctx.p5m.get(r.request_id)
        p1 = ctx.p1h.get(r.request_id)
        if p5 is None or p1 is None:
            unanswered += 1
            out.append(WRITE_5M)
            continue
        if not price.known:
            unpriced += 1
            out.append(WRITE_5M)
            continue
        prefix = r.cached_context
        best, best_cost = EXPIRE, 0.0  # expiring is the reference point: zero extra cost
        for action in (WRITE_5M, WRITE_1H, PING_5M, PING_1H):
            tier = tier_of(action)
            premium = prefix * (price.write_rate(tier) - price.input)
            # Not write_rate(tier): see the docstring. The avoided re-creation is priced at
            # the cheapest tier that could re-establish the prefix, which is what keeps this
            # from being an argument for its own most expensive option.
            rescue = prefix * (price.write_5m - price.cache_read)
            n = ctx.schedule.max_pings if pings(action) else 0
            ping_cost = n * price.keep_alive_cost(prefix, ctx.semantics)
            # The probability that the entry is still alive when the conversation returns is
            # the probability of returning inside this action's COVERAGE, and the predictor
            # answers at the two horizons it was bucketed for. A coverage BETWEEN them (a
            # keep-alive schedule reaches 14 minutes) is bounded below by the 5-minute
            # figure rather than interpolated: interpolating would invent a number the model
            # never produced, and the bound errs toward spending less.
            cov = _coverage_ms(action, ctx.schedule)
            p = p1 if cov >= HORIZON_1H_MS else (p5 if cov <= HORIZON_5M_MS else p5)
            expected = premium + ping_cost - p * rescue
            if expected < best_cost:
                best, best_cost = action, expected
        out.append(best)
    ctx.notes["expected_cost_unanswered"] = unanswered
    ctx.notes["expected_cost_unpriced"] = unpriced
    return out


def policy_optimal(rows: Sequence[Request], ctx: PolicyContext) -> list[str]:
    """UNREACHABLE CEILING: the cheapest action sequence that exists, computed exactly.

    Why a dynamic program and not a greedy rule. The action chosen at turn t decides two
    things at once — whether turn t itself may READ from cache (an action of 'expire' writes
    no cache_control, so the whole prefix is billed as fresh input however warm the entry
    was) and whether turn t+1 hits. A rule that looks only at the gap ahead therefore gets
    the current turn wrong, and the first version of this function did: it scored BELOW a
    plain keep-alive, which is impossible for a ceiling and is what exposed the error.

    The exact optimum is cheap because the state is small. Everything about the entry
    entering turn t — its size, its tier, its deadline, the keep-alives that fired during
    the span — is a function of the PREVIOUS action alone, given the two rows' timestamps.
    So the cost decomposes as a sum of f(action[t-1], action[t]) along the chain and one
    Viterbi pass per trajectory settles it: 5 states, 5 transitions, O(25n).

    It reads the true next-request time, so it is the bound a predictor is measured against
    and never a result. Quoting it as a saving would be claiming perfect foresight.
    """
    sem, sched = ctx.semantics, ctx.schedule
    by_conv: dict[tuple[str, str], list[Request]] = defaultdict(list)
    for r in rows:
        by_conv[r.key].append(r)

    chosen: dict[int, str] = {}
    for key in sorted(by_conv):
        group = sorted(by_conv[key], key=lambda r: (r.ts_ms, r.request_id))
        # best[a] = (cost so far, action list) for a trajectory whose LAST action is a.
        best: dict[str, tuple[float, list[str]]] = {}
        for i, r in enumerate(group):
            prev = group[i - 1] if i else None
            nxt: dict[str, tuple[float, list[str]]] = {}
            for action in ACTIONS:
                if prev is None:
                    c = _step_cost(None, EXPIRE, r, action, ctx, sem, sched)
                    nxt[action] = (c, [action])
                    continue
                pick: tuple[float, list[str]] | None = None
                for prev_action, (sofar, path) in best.items():
                    c = sofar + _step_cost(prev, prev_action, r, action, ctx, sem, sched)
                    if pick is None or c < pick[0]:
                        pick = (c, path + [action])
                nxt[action] = pick  # type: ignore[assignment]
            best = nxt
        # The OPEN span after the last turn: an action that pings keeps spending, bounded by
        # the window's end, and the optimum has to pay for that too or it would prefer a
        # keep-alive it never settles up on.
        last = group[-1]
        end = ctx.notes.get("window_end_ms") or last.ts_ms
        final: tuple[float, list[str]] | None = None
        for action, (sofar, path) in best.items():
            c = sofar + _open_span_cost(last, action, ctx, sem, sched, int(end))
            if final is None or c < final[0]:
                final = (c, path)
        for r, a in zip(group, final[1]):  # type: ignore[index]
            chosen[r.request_id] = a
    return [chosen[r.request_id] for r in sorted(rows, key=lambda r: (r.ts_ms, r.request_id))]


def _entry_after(row: Request, action: str) -> tuple[int, str, int]:
    """The (tokens, tier, expires) a turn leaves behind. A hit refreshes, a write starts —
    both land on the same expression, which is why this needs no hit flag."""
    tier = tier_of(action)
    if tier == TTL_NONE:
        return 0, TTL_NONE, 0
    return row.cached_context, tier, row.ts_ms + LIFETIME_MS[tier]


def _step_cost(prev: Request | None, prev_action: str, row: Request, action: str,
               ctx: PolicyContext, sem: Semantics, sched: PingSchedule) -> float:
    """The cost the pair (prev_action, action) adds: the span's keep-alives plus this
    request's own bill. The DP's transition weight, and it reads the SAME cost functions
    and the same ping core the accounting evaluator does."""
    tokens, tier, expires = 0, TTL_NONE, 0
    ping_cost = 0.0
    if prev is not None:
        tokens, tier, expires = _entry_after(prev, prev_action)
        span = _ping_span(tokens, tier, expires, prev.ts_ms, prev_action, row.ts_ms,
                          ctx.prices.for_model(prev.model), sem, sched)
        ping_cost, tier, expires = span.cost, span.tier, span.expires_ms
    alive = tokens > 0 and row.ts_ms < expires
    hit = alive and row.miss_reason not in UNRESCUABLE
    reusable = min(tokens, row.cached_context) if hit else 0
    t = tier_of(action)
    fresh, read, write = row.input_tokens, reusable, 0
    if t == TTL_NONE:
        fresh += row.cached_context
        read = 0
    else:
        write = max(0, row.cached_context - reusable)
    price = ctx.prices.for_model(row.model)
    own = price.request_cost(fresh, read, write, row.output_tokens, t) if price.known else 0.0
    return ping_cost + own


def _open_span_cost(last: Request, action: str, ctx: PolicyContext, sem: Semantics,
                    sched: PingSchedule, window_end_ms: int) -> float:
    """What a pinging action keeps spending after the trajectory's last observed request."""
    tokens, tier, expires = _entry_after(last, action)
    return _ping_span(tokens, tier, expires, last.ts_ms, action, window_end_ms,
                      ctx.prices.for_model(last.model), sem, sched).cost


POLICIES: dict[str, Policy] = {
    "no-cache": policy_no_cache,
    "fixed-5m": policy_fixed_5m,
    "fixed-1h": policy_fixed_1h,
    "keepalive-5m": policy_keepalive_5m,
    "observed-policy": policy_observed,
    "keepalive-1h": policy_keepalive_1h,
    "keepalive-5m-to-1h": policy_extend_1h,
    "predictor": policy_predictor,
    "expected-cost": policy_expected_cost,
    "optimal": policy_optimal,
}

# The arms whose result is not a reachable saving, and must be labelled wherever shown.
UNREACHABLE = frozenset({"optimal"})


def run_policy(name: str, rows: Sequence[Request], ctx: PolicyContext,
               **kwargs: Any) -> Cost:
    """Build the action list for one policy and score it."""
    actions = POLICIES[name](rows, ctx)
    return evaluate(rows, actions, ctx.prices, semantics=ctx.semantics,
                    schedule=ctx.schedule, policy=name, **kwargs)


# ── the predictor bridge ───────────────────────────────────────────────────

def predictor_probabilities(train: Sequence[Request], score: Sequence[Request], *,
                            bucket_seconds: int = 300, max_ttl_seconds: int = 3600,
                            window_end_ms: int | None = None,
                            ) -> tuple[dict[int, float], dict[int, float], dict[str, Any]]:
    """Fit kv_ttl_survival_predictor on `train` and score `score`.

    The split is the whole point: coefficients fitted over the rows they are then scored on
    are in-sample and will flatter the model. `train` must end before `score` begins, and
    the caller is responsible for that — see the CLI, which cuts by time.

    Returns P(return within 5m), P(return within 1h), both keyed by request_id, plus the
    fit's own summary. With the default bucketing those two are column 0 and one minus the
    tail of the predictor's distribution, which is why that bucketing is the default:
    they are the provider's two tiers, read straight off with no interpolation.
    """
    import pandas as pd  # only this bridge needs the scientific stack

    from kv_ttl_survival_predictor import (KVTTLTimePredictor,
                                          build_next_request_targets)

    def frame(rows: Sequence[Request]) -> "pd.DataFrame":
        return pd.DataFrame([{
            "request_id": r.request_id, "user_id": r.user,
            "conversation_id": r.conversation, "model": r.model,
            "request_time": pd.Timestamp(r.ts_ms, unit="ms", tz="UTC"),
        } for r in rows])

    end = window_end_ms if window_end_ms is not None else max(r.ts_ms for r in train)
    labelled = build_next_request_targets(
        frame(train),
        compatibility_columns=("user_id", "conversation_id", "model"),
        observation_end=pd.Timestamp(end, unit="ms", tz="UTC"))
    model = KVTTLTimePredictor(bucket_seconds=bucket_seconds,
                               max_ttl_seconds=max_ttl_seconds,
                               categorical_features=("user_id", "model"))
    model.fit(labelled)

    scored = frame(score)
    proba = model.predict_proba(scored)
    ids = scored["request_id"].tolist()
    p5m = {int(i): float(v) for i, v in zip(ids, proba[:, 0])}
    p1h = {int(i): float(1.0 - v) for i, v in zip(ids, proba[:, -1])}
    return p5m, p1h, dict(model.training_summary_)


def sweep_thresholds(rows: Sequence[Request], ctx: PolicyContext, *,
                     grid: Sequence[float] = (0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9),
                     window_end_ms: int | None = None) -> tuple[float, float, float]:
    """Pick the thresholds that minimise cost ON THESE ROWS. Returns (t5m, t1h, best_usd).

    Call it on the TRAINING half only. Tuning a threshold on the rows it is then reported
    on is the same in-sample error as fitting the model on them, one layer up, and it is
    the layer people forget.
    """
    best = (ctx.threshold_5m, ctx.threshold_1h, math.inf)
    for t5 in grid:
        for t1 in grid:
            ctx.threshold_5m, ctx.threshold_1h = t5, t1
            cost = run_policy("predictor", rows, ctx, window_end_ms=window_end_ms)
            if cost.total_usd < best[2]:
                best = (t5, t1, cost.total_usd)
    ctx.threshold_5m, ctx.threshold_1h = best[0], best[1]
    return best


# ── CLI ────────────────────────────────────────────────────────────────────

def _fmt_usd(v: float) -> str:
    return f"${v:,.2f}" if abs(v) >= 0.005 else f"${v:.4f}"


def _report(baseline_name: str, costs: dict[str, Cost], notes: dict[str, Any]) -> None:
    base = costs[baseline_name]
    print(f"\nBaseline: {baseline_name}  ({_fmt_usd(base.total_usd)} over "
          f"{base.requests:,} requests, {base.conversations:,} trajectories)")
    if base.unpriced:
        print(f"  {base.unpriced:,} requests are on a model the price list cannot price; "
              f"they are counted and contribute to NO dollar figure.")
    # The columns say CHEAPER/MORE in words, not just a sign. A column headed "saving" that
    # shows -$2,117.80 for the worst arm on the page reads, to a person scanning it, as
    # "saves $2,117.80" — which inverts the whole table. It happened on the first reading.
    head = (f"\n  {'policy':<16}{'total bill':>12}{'vs baseline':>13}{'':>9}{'%':>9}"
            f"{'hit %':>8}{'pings':>8}{'w5m':>7}{'w1h':>6}")
    print(head)
    print("  " + "-" * (len(head) - 3))
    for name, c in costs.items():
        s = compare(base, c)
        delta = -s.absolute_usd  # what the policy adds to the bill: + is worse
        word = "cheaper" if delta < -0.005 else ("MORE" if delta > 0.005 else "same")
        pct = f"{-s.percent_usd:+.2f}%" if s.percent_known else "n/a"
        mark = " *" if name in UNREACHABLE else ""
        print(f"  {name + mark:<16}{_fmt_usd(c.total_usd):>12}{_fmt_usd(delta):>13}"
              f"{word:>9}{pct:>9}"
              f"{c.hit_rate_pct:>7.1f}%{c.pings:>8,}{c.writes_5m:>7,}{c.writes_1h:>6,}")
    if any(n in UNREACHABLE for n in costs):
        print("\n  * optimal reads the true next-request time and is computed exactly. It is the "
              "CEILING\n    no predictor can reach, not a result. Never quote it as a saving.")
    print("\n  'vs baseline' and '%' are what the policy ADDS to the bill: negative is "
          "cheaper, positive is\n  worse. Nothing is clamped. HIT RATE IS NOT THE "
          "OBJECTIVE — see fixed-1h, which buys the\n  best hit rate on the page and is "
          "the second most expensive arm on it.")
    for k, v in notes.items():
        print(f"  note: {k} = {v}")


def _run_fixture(path: str) -> int:
    """Score a fixture and print the result as JSON. The drift test's interface.

    A fixture is {"window_end_ms":…, "rates":{model:{…}}, "semantics":{…},
    "schedule":{…}, "requests":[…], "actions":{id: action}} — everything the evaluator
    needs and nothing it has to look up, so kv_ttl_cost_drift_test.go can hand the SAME
    trajectory and the SAME action list to this and to kvcache.Simulate and compare.

    `"policy": "<name>"` runs a POLICY instead of a supplied list, which is how the guard
    also locks the two exact ceilings (`optimal`) together rather than only the evaluator.

    Actions are keyed by REQUEST ID, never positional: rows with equal timestamps are
    ordered by (ts, id), so a positional list is applied in a different order than it was
    written in and the two sides silently score different plans.
    """
    spec = json.loads(Path(path).read_text(encoding="utf-8"))
    book = PriceBook()
    for model, r in (spec.get("rates") or {}).items():
        book._overrides[model] = ModelPricing(  # noqa: SLF001 - the fixture IS the source
            model=model, input=r["input"], output=r["output"], cache_read=r["cache_read"],
            write_5m=r["write_5m"], write_1h=r["write_1h"],
            ping_input_tokens=r.get("ping_input_tokens", DEFAULT_PING_INPUT_TOKENS),
            ping_output_tokens=r.get("ping_output_tokens", DEFAULT_PING_OUTPUT_TOKENS),
            source="fixture", known=bool(r.get("known", True)))
    rows = derive([Request(**r) for r in spec["requests"]])
    sem = Semantics(**(spec.get("semantics") or {}))
    sched = PingSchedule(**(spec.get("schedule") or {}))
    if spec.get("policy"):
        # Run a POLICY rather than a supplied list. The point is `optimal`: both sides solve
        # the same dynamic program, and a ceiling the two disagree about is a ceiling neither
        # of them can be quoted against.
        ctx = PolicyContext(prices=book, semantics=sem, schedule=sched)
        ctx.notes["window_end_ms"] = spec.get("window_end_ms")
        actions = POLICIES[spec["policy"]](rows, ctx)
    else:
        actions = spec["actions"]
    cost = evaluate(rows, actions, book,
                    semantics=sem, schedule=sched,
                    window_end_ms=spec.get("window_end_ms"),
                    policy=spec.get("policy") or "fixture")
    print(json.dumps(cost.to_dict(), default=str))
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        description="Cost a KV-cache TTL policy on real trajectories at real rates.")
    ap.add_argument("--db", default=DEFAULT_DB, help=f"dashboard store (default {DEFAULT_DB})")
    ap.add_argument("--prices", default=DEFAULT_PRICES,
                    help=f"operator price list (default {DEFAULT_PRICES})")
    ap.add_argument("--baseline", default="observed-policy",
                    help="policy to measure savings against (default: observed-policy). The "
                         "arm names are kvcache's registry (kvcache.Strategy* in Go), because "
                         "a name that means one thing on the dashboard and another here is a "
                         "comparison nobody can trust.")
    ap.add_argument("--split", type=float, default=0.7,
                    help="fraction of the WINDOW used to fit the predictor and tune its "
                         "thresholds; every policy is then scored on the remainder "
                         "(default 0.7)")
    ap.add_argument("--min-prefix", type=int, default=20_000,
                    help="prefix below which nothing is cached (default 20000)")
    ap.add_argument("--max-pings", type=int, default=2)
    ap.add_argument("--no-predictor", action="store_true",
                    help="skip the ML arms (no pandas/scikit-learn needed)")
    ap.add_argument("--no-sweep", action="store_true",
                    help="use --threshold-5m/--threshold-1h rather than tuning on the "
                         "training half")
    ap.add_argument("--threshold-5m", type=float, default=0.5)
    ap.add_argument("--threshold-1h", type=float, default=0.5)
    ap.add_argument("--json", help="write the full result set here")
    ap.add_argument("--fixture", help="score a JSON fixture and print it (drift test)")
    args = ap.parse_args(argv)
    if args.fixture:
        return _run_fixture(args.fixture)

    prices = PriceBook.from_operator_file(args.prices)
    rows = load_trajectories(args.db)
    if not rows:
        print("no requests in that store")
        return 1
    window_end = max(r.ts_ms for r in rows)
    lo, hi = rows[0].ts_ms, window_end
    cut = lo + int((hi - lo) * args.split)
    train = [r for r in rows if r.ts_ms <= cut]
    test = [r for r in rows if r.ts_ms > cut]
    # The test half is re-derived so that "the next request" is the next one IN THE HALF
    # BEING SCORED. Leaving the full-window derivation in place would let a policy be
    # credited for a return that lies outside the rows it is measured on.
    test = derive([replace(r) for r in test])
    print(f"store   {args.db}")
    print(f"prices  {args.prices}")
    print(f"window  {lo} .. {hi} ms UTC  ({len(rows):,} requests)")
    print(f"split   fit/tune on {len(train):,} requests, score on {len(test):,}")

    ctx = PolicyContext(prices=prices, min_prefix=args.min_prefix,
                        schedule=PingSchedule(max_pings=args.max_pings),
                        threshold_5m=args.threshold_5m, threshold_1h=args.threshold_1h)
    # The exact optimum has to pay for the open span after each trajectory's last request,
    # and that is bounded by the window rather than by the data. Passed through notes so the
    # policy signature stays (rows, ctx).
    ctx.notes["window_end_ms"] = window_end
    names = ["no-cache", "fixed-5m", "fixed-1h", "keepalive-5m", "keepalive-1h",
             "keepalive-5m-to-1h", "observed-policy"]
    if not args.no_predictor:
        p5m, p1h, summary = predictor_probabilities(train, test, window_end_ms=cut)
        ctx.p5m, ctx.p1h = p5m, p1h
        ctx.notes["predictor_fit"] = summary
        if not args.no_sweep:
            # Tuned on the TRAINING rows, scored on the test rows. The training rows need
            # their own probabilities to be tuned against, and those come from the same
            # fit — in-sample for the tuning step only, which is why the reported figure
            # is the test one.
            tp5, tp1, _ = predictor_probabilities(train, train, window_end_ms=cut)
            tune_ctx = PolicyContext(prices=prices, min_prefix=args.min_prefix,
                                     schedule=PingSchedule(max_pings=args.max_pings),
                                     p5m=tp5, p1h=tp1)
            t5, t1, _ = sweep_thresholds(derive([replace(r) for r in train]), tune_ctx,
                                         window_end_ms=cut)
            ctx.threshold_5m, ctx.threshold_1h = t5, t1
        ctx.notes["thresholds"] = {"p5m": ctx.threshold_5m, "p1h": ctx.threshold_1h}
        names += ["predictor", "expected-cost", "optimal"]

    costs: dict[str, Cost] = {}
    for name in names:
        costs[name] = run_policy(name, test, ctx, window_end_ms=window_end)
    if args.baseline not in costs:
        print(f"baseline {args.baseline!r} is not one of {list(costs)}")
        return 2
    _report(args.baseline, costs, ctx.notes)

    # What the deployment ACTUALLY paid over the same rows, for scale. Not a baseline: it
    # was billed by a different pipeline under a policy this model does not reconstruct.
    billed = sum(r.billed_usd for r in test)
    print(f"\n  for scale: the deployment was actually billed {_fmt_usd(billed)} over these "
          f"same {len(test):,} requests.")

    if args.json:
        Path(args.json).write_text(json.dumps({
            "window": {"since_ms": lo, "until_ms": hi, "cut_ms": cut},
            "baseline": args.baseline,
            "notes": ctx.notes,
            "billed_usd": billed,
            "policies": {n: c.to_dict() for n, c in costs.items()},
            "savings": {n: compare(costs[args.baseline], c).__dict__
                        for n, c in costs.items()},
        }, indent=2, default=str), encoding="utf-8")
        print(f"  wrote {args.json}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
