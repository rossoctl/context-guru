# What actually fires, and why

Before spending anything on a scored benchmark, a cheap question: **on real captured traffic, which
components actually act, and which are silently declining?** The answer reframed a planned
experiment badly enough to cancel it, so it is worth reading before designing any arm.

Everything here is replay against captured request bodies via the proxy's `/compact` endpoint —
no agent, no task, no reward. Total spend **~$3**.

Reproduce: `deploy/harbor/replay2.py <capture> --configs ...`, with `CACHE_MODE` set per run.

## 1. The tail gate costs more than the docs imply — **measured**

`replay2.py` inherits `CACHE_MODE`, so the same configs can be replayed with cache-awareness on and
off. The delta *is* the tail gate. 200 requests of `capture-swebench`:

| component | non-tail (`CACHE_MODE=off`) | tail-gated (`on`) | effect of the gate |
|---|---|---|---|
| **`mask`** | **50.67%** (685,055 tok) | **3.33%** (45,020 tok) | **loses 93% of its effect** |
| **`failed_run`** | 1.29% | **0%** | **fully inert** |
| `extract` | 0.84% | 0.84% | unaffected — not tail-gated |
| `cmdfilter` | 0.23% | 0.23% | unaffected |
| `dedup` | 0% | 0% | never fires on this corpus either way |

Only `mask`, `failed_run` and `extract_llm` consult `TailOnly`. The others are pure functions of
content, so replaying them each turn is byte-stable and needs no gate.

This is the quantified form of the geometry in [`mask`](../components/mask.md#when-its-inert):
`TailOnly(i)` permits only `i > MaxCachedIdx = prevLen − 1`, while `mask`'s candidates are by
definition outputs that were present last turn. `failed_run` lands in the same place and reaches
exactly zero. Both are in shipped presets — `failed_run` in `codesmart`, `agent`, `general`,
`codesafe` and `balanced`.

## 2. `extract_llm` cannot fire on SWE-bench at all — **measured**

0% saved in **every** configuration tried: economic gate on and off, cache-aware and not. The
recorded gate reason is `below_output_floor`.

The cause is a quantity nobody had measured: **tool-output size**.

| corpus | p50 | max | ≥3,000 (its floor) | ≥30,500 (its break-even, as published) |
|---|---|---|---|---|
| SWE-bench, a fresh run made for this pass | 71 | **2,760** | **0** | **0** |
| `capture-swebench` | 106 | 5,674 | a handful | **0** |
| `capture-tb` | ~0 | 1,906 | **0** | **0** |
| **LOCA-bench** | 185 | **59,857** | **54** | **7** |

**`extract_llm`'s output floor is larger than the largest tool output SWE-bench produces.** Its
break-even is an order of magnitude larger. It is structurally incapable of acting there, and the
same is true of Terminal-Bench.

### So `codesmart` saves ~1% on this workload

`codesmart` is `[format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit]`. On
caching traffic `extract_llm` is off by default and `failed_run` is inert, leaving `extract` (0.84%)
+ `cmdfilter` (0.23%) ≈ **1%**. Any arm labelled "the SWE-bench winning config" should be described
that way rather than by reputation.

### And the ~27% figure is not reproducible here

No configuration tried reproduces it on this corpus with current code. That is consistent with
`config.go`'s own note that the published numbers "describe an ancestor" of the preset. Whatever
produced it was a different workload with much larger outputs, or pre-gate code with a far lower
floor. See also the [reattribution](../components/mask.md) — the figure was long credited to `mask`,
which was never in the arm that produced it.

## 3. A mispricing in the economic gate — **found, fixed**

Raised in review: *tail-only `extract_llm` has no break-even constraint since it doesn't invalidate
the cache.* Correct in mechanism, and it exposed a real bug.

`savedTokenValue` priced **every** saved token at the cache-read rate whenever the request was
cache-aware, reasoning that re-sent content is already in the cached prefix. True of a **replay**
turn; false of the turn the cut is made — and when cache-aware `extract_llm` acts *only* on the
tail, which by definition has never been cached. On that turn the content is billed as a
cache-**write** (`$3.75/MTok`, dearer than fresh input) or as plain fresh input.

Confirmed from live usage rather than argued: the fresh SWE-bench trial reported **52,561
`cache_creation` tokens** against 746,047 cache-read over 18 turns.

| case | as published | corrected | gain |
|---|---|---|---|
| caching, recurring | 30,397 | **11,550** | 2.63× |
| caching, first sight | 42,556 | **12,900** | 3.30× |
| non-caching | unchanged | unchanged | — (one rate, so `first + r·rate ≡ (1+r)·rate`) |

**The verdict survives the correction; the number did not.** SWE-bench's largest output (2,760) is
still ~4× short, so keeping the component off by default there remains right. What changes is
large-output workloads: on LOCA the eligible set goes from **7 to 31** of 1,639 outputs.

Two side effects worth knowing before tuning: the cached/non-caching break-even ratio falls from
~20× to **~6.4×**, and **recurrence becomes a much weaker lever** — ×1.12 rather than ×1.40 —
because the applied turn now dominates the sum. Three tests that had encoded the old arithmetic
were updated rather than deleted, including the drift guard that ties the code to
[`extract_llm`'s doc](../components/extract_llm.md).

## 4. Unexplained, and recorded as such

`extract_llm` burned **12.8 s across 20 requests** (~640 ms each) while reporting `acted: 0`,
`discarded_changes: 0`, and nothing above INFO in the proxy log. Something in that path spends real
wall-clock without producing *or* discarding a change. Not diagnosed; it should be a bug rather than
a guess.

## What this changed

A planned reward-safety arm on SWE-bench Verified was **cancelled**. With ~1% of content available
to remove, a reward-neutral result would have been vacuous — it would have shown that removing
almost nothing breaks almost nothing, at ~$100.

The binding constraint turns out not to be context length, which is where the argument had been
focused, but **tool-output size**. SWE-bench and Terminal-Bench both have outputs an order of
magnitude below the thresholds these components need. **LOCA-bench is the only benchmark in the set
where anything can fire** — and independently the only one whose context length is a dial. That is
now the vehicle.

Sobering even there: only 31 of 1,639 LOCA outputs clear the corrected `extract_llm` break-even.
`coref` has a better case on the same corpus — its floor is 300 tokens, giving it 580 candidate
blocks (35%) — which is the one place the two components' economics genuinely diverge.

See also: [`mask`](../components/mask.md) · [`extract_llm`](../components/extract_llm.md) ·
[density](coref-density.md) · [the eval-box measurement](coref-evalbox.md) ·
[reachability](coref-reachability.md)
