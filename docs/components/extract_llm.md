# extract_llm

!!! warning "Offload — lossy, reversible (LLM-written filter). **Spends money to save money.**"
    A cheap model writes a small program that projects a large tool output down to what the agent
    actually needs, deletes the rest, and stashes the original. The powerful, relevance-aware
    counterpart to the deterministic [`extract`](extract.md) — and the only component whose
    savings can be **net negative**. Read [Economics](#economics) before enabling it.

## The honest verdict

On a **prompt-caching backend** (the default for Anthropic/Bedrock traffic), `extract_llm` is
**usually not worth running**, and the measurements say so plainly:

| Measured (Terminal-Bench, gate off) | Value |
|---|---|
| Extraction calls | 271 |
| Extraction cost | $3.26 |
| Cumulative added latency | 1,592,467 ms (~1,592 s, ~450 ms/call) |
| Unique tokens saved | 197,548 |
| Value of those tokens at the **cache-read** rate | **$0.0395** (197,548 × $0.20/MTok) |
| **Net** | **$0.0395 − $3.26 = −$3.22 — 82× underwater** |
| Share of realized value that came from the **replay cache**, not the LLM | **~93%** |

The improvement plan's original figure was **~8×** underwater. That implicitly priced the saved
tokens as fresh input; they were not. All 197,548 sat in the **cached prefix**, so they bill at the
cache-read rate, and the honest ratio is **82×** — an order of magnitude worse, plus the 1,592,467
ms of blocking time, which is not priced at all. A later Terminal-Bench arm that excluded
`extract_llm` entirely re-derived this independently. The verdict below was right; it was
understated.

!!! note "Which cache-read rate"
    The 82× figure prices the saved tokens at **$0.20/MTok**, the rate quoted in the original
    issue. The break-even tables below use the **$0.30/MTok** sonnet-class rate the gate itself
    applies (`agentCacheReadPerMTok`), which gives $0.0593 and **55×**. Same conclusion either
    way — neither is within two orders of magnitude of paying for $3.26 — and the gate reasons
    with the *more generous* rate, so the shipped decline is the conservative one.

The reason is arithmetic, not implementation quality. A request to a caching backend is
~99.95% cached, so a token removed from a cached region saves the **cache-read** rate
(`$0.30/MTok`), not the fresh-input rate (`$3/MTok`) — a **10× haircut**. An extraction call
costing ~$0.012 must therefore remove a *lot* of tokens to break even:

| Backend | Content | Break-even output size |
|---|---|---|
| Caching | seen once | **~42,600 tokens** |
| Caching | recurring (amortized over replays) | **~30,500 tokens** |
| Non-caching | seen once | ~3,400 tokens |
| Non-caching | recurring | **~1,800 tokens** |

The caching figures are why the component is now **off by default on caching backends**: no
realistic tool output reaches 30,500 tokens, so the gate would only ever be declining.

These use the **measured** compression ratio, and that measurement is the uncomfortable part: on
real captures an accepted extraction removed only **31–254 tokens per call** on outputs of
400–2,000 tokens — an actual ratio around **0.10–0.12**, not the ~0.45 one might assume. The model
declines to cut aggressively, and correctly so: its contract is recall-first.

Most tool outputs are nowhere near 30,500 tokens — in one measured Terminal-Bench capture the
**largest** tool output was 2,053 tokens, ~15× below the cached break-even. That is why the same
component **wins on a non-caching backend and loses on a caching one**, and why the fix is not
"compress harder" but "decide per call". The [economic gate](#economics) makes that
decision automatically, so the component is safe to leave enabled — it simply declines to spend
where it cannot win.

### Measured with the gate on (replay of real captures, `aws/claude-haiku-4-5`)

`forced` = gate off (`economic_gate: false`); `gated` = the shipped defaults. Same
capture, same floor, same model. **"Saved" is extract_llm's OWN savings**, not the pipeline's:
crediting the whole pipeline's savings to this component reports a win that does not exist.
Attribution is the difference between
"positive" and "negative" here, so it is worth stating twice.

**Terminal-Bench capture (20 requests):**

| Arm | Backend | Calls | Cost | Own tokens saved | Gross value | **NET** | Avg latency |
|---|---|---|---|---|---|---|---|
| forced | caching | 5 | $0.0095 | 2 | $0.0000 | **−$0.0095** | 11,666 ms |
| **gated** | caching | **0** | $0 | 0 | $0 | **$0** | — |
| forced | non-caching | 6 | $0.0233 | 2,018 | $0.0061 | **−$0.0172** | 11,385 ms |
| **gated** | non-caching | 3 | $0.0126 | **2,394** | $0.0072 | **−$0.0054** | 10,534 ms |

**SWE-bench capture (19 requests):**

| Arm | Backend | Calls | Cost | Own tokens saved | **NET** | Avg latency |
|---|---|---|---|---|---|---|
| forced | caching | 2 | $0.0090 | 0 | **−$0.0090** | 8,556 ms |
| **gated** | caching | **0** | $0 | 0 | **$0** | — |
| forced | non-caching | **26** | $0.0660 | 274 | **−$0.0652** | 11,302 ms |
| **gated** | non-caching | 1 | $0.0000 | 0 | **$0.0000** | 15,004 ms |

Reading these:

- **The gate is a strict improvement in every arm.** It never loses more than the ungated
  behaviour and usually far less: −$0.0172 → −$0.0054 (68% less waste) on Terminal-Bench
  non-caching *while saving more tokens*, and 26 calls → 1 on SWE-bench non-caching, taking
  a −$0.0652 loss to break-even.
- **On a caching backend the component now makes zero calls and loses nothing**, because it
  is disabled there by default (see below). That is the honest resolution: the gate could
  reduce the loss but never eliminate it, so the default stops paying for it at all.
- **Even on a non-caching backend the component does not clearly earn its place** on these
  workloads — the best result is break-even, not profit. It removes only 31–254 tokens per
  call at ~10 s of added latency. It earns its place when outputs are genuinely large
  (>~1,800 tokens on a non-caching backend); these captures mostly are not.

!!! tip "If you only remember one thing"
    On a caching backend, expect `extract_llm` to suppress most candidates and contribute
    little; its value comes from the **result cache**, not from new LLM calls. On a
    **non-caching** backend it is genuinely valuable. Check
    **`extract_llm` is disabled by default on prompt-caching backends** — in code,
    not just documentation, because every caching workload measured came out net-negative even
    with a correctly-working gate. It runs on non-caching traffic, where the gate decides per
    call. Set `allow_on_caching_backend: true` to override. Check `/stats` →
    `extract.net_value_usd` on your own workload before doing so.

## How it works

For a large tool output, `extract_llm` asks a **cheap model** to write a short **Starlark filter**
specific to that content. The program sees the full output (bounded to ~32k chars) and the recent
conversation, so it can delete the exact irrelevant lines/records — and, in `rewrite` mode, reword
or collapse spans — while keeping ids, paths, and error lines verbatim. The program runs in a
**sandbox** (no imports/IO, step + 2s limits) and the result must pass a sanity check
(non-empty, strictly smaller, required ids present); on any miss the output is left verbatim. It has
RE2 regex helpers (`re_sub` / `re_findall` / `re_split` / `re_match`). JSON bodies are decoded and
filtered structurally.

- **Deletion-only guarantee (opt-in):** set `rewrite: false` and the result is accepted only if it is
  an in-order **character subsequence** of the input — the model can trim anything but provably
  cannot fabricate, reorder, or reword. Default `rewrite: true` is the more powerful mode (reword /
  summarize allowed; ids/paths/errors still required verbatim by the sanity check).
- **Model source:** `model.source` is `incoming` (default — reuse the proxied request's own model +
  key) or `config` (a dedicated cheap model set via `CHEAP_MODEL*` env / the gateway's `CheapModel`).
  With no model available it degrades to a no-op (the deterministic `extract` still runs if present).
- **Frozen and replayed:** a reduced output is checkpointed by content hash — the same output
  re-sent on a later turn reuses the prior compaction (no new model call, byte-identical result →
  the request prefix stays KV-cache stable). This replay is where **~93%** of the component's
  realized value comes from.
- **Cache-aware `skip_file_reads`:** tri-state. Unset = AUTO — skip line-numbered source-file dumps
  when the request is prompt-cached (they already bill at the cheap cache-read rate), reduce them
  otherwise. The economic gate now generalizes this same reasoning to *every* candidate.

    !!! warning "AUTO was not implemented until recently"
        Unset used to mean "always reduce", which defeated the entire measured rationale for the
        flag. Live confirmation of the shape it produces: a ~7k-token Go file read went to the
        model, which spent 40 s on a reply that hit the output cap and saved nothing. AUTO now
        behaves as documented, and a cold-cache sweep is the deliberate exception — there nothing
        is cached and file reads are the largest mass being re-billed at the write rate.

### Compaction target and how much conversation the model sees

Two knobs the prompt itself carries.

**`aggressiveness`** is `low` | `medium` (default) | `high`. Measured live on identical bodies,
two samples per level:

| shape | low | medium | high |
|---|---|---|---|
| source file read, 3,598 tok | 64.8% removed | 90.0% | 90.4% |
| access log, 8,722 tok | *declined* | 98.8% | 98.1% |

Note the access-log row: `low` is explicitly told that returning the input unchanged is an
acceptable answer, so on an almost-all-noise shape it may decline — and a declined call still
costs its money and its seconds. `low` is for "I would rather pay for tokens than risk a
re-read", not for "spend less".

 It is *taught* rather than
thresholded: the second system block states a target and carries three or four worked examples
demonstrating it, across the shapes real traffic contains — JSON, bash and test logs, prose, and
a source-file read. It changes what the model is **asked** for and never what is **accepted**:
the verbatim-preservation rule, the strictly-smaller rule and (in `rewrite: false`) the
subsequence proof are identical at every level.

**`context`** is `goal` | `recent` (default) | `full`, with `context_messages` (7) as the N for
`recent`. The model is asked to reduce one output "toward what the agent needs next", so what it
is told about the conversation is the whole basis of that judgement. `goal` carries the task and
the latest turn; `recent` adds every user turn plus the last N non-tool messages, which is what
puts mid-session corrections ("it must default to 30s, not 10s") in front of the model that is
deciding whether the line saying so may go; `full` carries the entire transcript and is what a
cold-cache sweep always uses.

The two system blocks are ordered general-contract-first because a provider caches a *prefix*:
the half that is byte-identical for every account has to come first or it is shared with nobody.
Both the level's text and the level itself are part of the result cache key, so switching level
**misses** rather than replaying the previous level's answer.

## Economics

The component only calls the LLM when **expected saving > expected cost**.

```
expected saving = tokens expected to remove
                x (1 + expected future replays)
                x per-token value       <-- cache-read rate when cache-aware, else fresh rate

expected cost   = analytic size-aware cost of one extraction call
                  (preamble + shown content + overhead, at real rates),
                  reconciled with the observed mean once calls exist
```

Each input is measured rather than assumed:

| Input | Source |
|---|---|
| Per-token value | `Ctx.CacheAware` selects the cache-read vs fresh rate (the 10× factor) |
| Expected compression ratio | **Learned** from this workload's accepted results; a conservative **0.12** (the measured figure) until ~1.5k tokens of evidence. Repeated misses drive it toward 0, shutting the gate. Note the direction of conservatism: for a *spending* gate, conservative means under-estimating the saving |
| Call cost | **Analytic and size-aware** — `preamble (1,463 tok) + shown content + overhead`, priced at real model rates — then reconciled with the observed mean once real calls exist. A flat per-call constant is not just imprecise, it **deadlocks**: pricing every call at the $0.012 average (≈5× the true cost on small outputs) suppressed everything, so nothing was ever observed and the estimate could never correct itself. Measured: $0.0024/call actual vs the $0.012 prior |
| Model pricing | `claude-haiku-4-5` list rates by default; override with `CHEAP_MODEL_PRICE_IN` / `_OUT` / `_CACHE_WRITE` / `_CACHE_READ` (dollars per MTok) |
| Expected replays | Recurrence: content seen before in **any** session is expected to recur (measured 82/103 across sessions) |
| Remaining horizon | Fewer expected replays late in a long session |

Every decision records a **reason**, visible at `/stats` → `extract.reasons` and
`extract.top_reason`, because the first question about an expensive component is always "why did
this run?". Set `economic_gate: false` to restore the older spend-on-size behaviour — needed only to
reproduce old benchmark numbers.

## Triggering

There is **no per-workload threshold to tune**. When `min_tokens` / `trigger` is unset, the
component derives its own gating from context pressure and growth:

| Context pressure (request ÷ window) | Behavior |
|---|---|
| > 80% | Per-output floor ~0.05% of the window — window pressure dominates, compact freely |
| 60–80% | Floor ~0.15%; fires on pressure alone |
| 25–60% | Floor ~0.30%; fires only if the context is also **growing** > 10%/turn |
| < 25% | Does not fire — compaction buys nothing worth an LLM call |

A *merely growing* context does not fire on every step; that was the behavior that produced 271
calls. When the context window is unknown (0) the derived logic is skipped and the configured
absolute `trigger` applies — the same fail-open convention `Trigger` already uses.

**`min_tokens` still governs when set explicitly**, so existing configs keep their behavior.

The `/compact` endpoint resolves the context window exactly as the chat path does, so
fraction-based `trigger` thresholds and the pressure logic behave identically in offline
replay and live traffic.

## Cold-cache sweep

The one regime where this component's economics are not in doubt.

When a session resumes after the provider's prompt-cache TTL, the cached prefix is gone and
the **whole transcript is re-billed as cache creation at 1.25x the fresh rate**. Two things
are true only on that turn: a removed token is worth 12.5x what it is worth on a warm turn
(cache-write rate vs cache-read rate), and rewriting deep history is free, because there is no
live cached prefix left to invalidate.

**Measured on the hosted service, 1.4 days of real traffic:**

| cache outcome | requests | cache_write tokens | cost |
|---|---|---|---|
| `hit` | 4,787 | 26.1M | $689.29 |
| **`ttl_expiry`** | **219** | **56.7M** | **$360.09** |
| `prefix_change` | 121 | 13.3M | $72.95 |
| `cold_start` | 231 | 7.8M | $51.03 |

TTL-expired turns were **4% of requests and 31% of spend** — $1.64 each against $0.144 for a
warm turn — and the shipped pipeline saved **0.015%** of it (`baseline_cost_usd` $360.14 vs an
actual $360.09).

```yaml
extract_llm:
  per_output: false          # leave ordinary turns alone
  cold_cache:
    enabled: true
    min_tokens: 1000
```

On such a turn the sweep lifts the tail gate (nothing is cached, so depth is free), takes the
whole transcript as context, prices saved tokens at the cache-write rate, and escalates to the
agent's own model if the transcript will not fit the extraction model's window. Every removal
leaves a `<<cg:HASH>>` marker with a one-line summary, and the result is frozen so later warm
turns replay it byte-for-byte and the new prefix stays cache-stable.

!!! note "Detection errs toward 'still warm'"
    The two errors are not symmetric. Believing a warm cache is cold makes a component rewrite
    a live prefix and forces a cache-write of the whole suffix; believing a cold cache is warm
    only forgoes a saving. So: the Anthropic family reads the TTL out of the request itself
    (exact — and every one of ~5,000 captured real requests carries a bare `ephemeral` mark,
    i.e. the 5-minute tier), anything else takes the documented one-hour outer bound, a minute
    of margin covers clock skew, and **no previous turn on record reads WARM**, so a proxy
    restart cannot invalidate a live cache.

## Caching

Three distinct caches, easily confused:

1. **Global result cache.** An extraction is a *context-free derived result*, so it is keyed on
   `sha256(content + prompt version + model + config fingerprint)` with **no session prefix** —
   identical content in a different session reuses the reduction. A prompt-version bump, model
   switch or config change **misses** rather than serving a stale extraction. Bounded by the
   store's existing TTL + LRU.

2. **Provider prompt cache on the extraction preamble.** The ~1,463-token invariant
   preamble is sent as a stable `system` block with a `cache_control` breakpoint (a leading system
   message on the OpenAI backend, which has no explicit breakpoints).

    !!! warning "Measured: this buys nothing on `claude-haiku-4-5`"
        A breakpoint below the model's **minimum cacheable prefix** is silently ignored — no error,
        `cache_creation_input_tokens: 0`. That minimum is **4096 tokens on `claude-haiku-4-5`** and
        1024 on `claude-sonnet-5`, against a **1,463-token** preamble. Verified against the gateway:

        | Prefix | Model | Result |
        |---|---|---|
        | ~1.5k | `claude-haiku-4-5` | `write=0 read=0` — **inert** |
        | ~4.5k | `claude-haiku-4-5` | `write=5401` then `read=5401` — caches |
        | ~1.5k | `claude-sonnet-5` | `write=2653` then `read=2653` — caches |

        So with the default cheap model the split is **structurally inert**; it pays only when
        extraction runs on a larger-context model (`model.source: incoming`). The split ships anyway
        — it is free, correct, and wins where it can — but do **not** infer a cache win from the
        fact that a breakpoint was placed. Watch `/stats` →
        `extract.prompt_cache_read_tokens`: if it stays 0 while `extract.calls` climbs, the
        breakpoint is inert on your model.

3. **The agent's own KV cache**, which the component must not disturb — hence freeze-and-replay and
   the tail-only gate for new decisions.

### Rejected: reusing the agent's cached prefix

Appending the extraction instruction after the agent's existing cached prefix, so that
extraction reads an already-cached context. **Prototyped against the live gateway and rejected.** It
works mechanically (the extraction turn read a 103,019-token prefix from cache with no cache-write
and no prefix invalidation), but cache-read is cheap, not free, and the bill scales with the *whole*
context:

| Prefix size | Cost of one extraction | vs a dedicated cheap-model call ($0.004) |
|---|---|---|
| 103,019 tok | $0.03398 | 8.5× |
| 500,000 tok | $0.15307 | 38.3× |
| 1,700,000 tok | $0.51307 | **128.3×** |

At the ~1.7M contexts this workload reaches, and with up to 4 concurrent per-output calls per turn,
that is ~$2.04/turn against ~$0.016 — the opposite of this issue's direction. Paying 1.7M
cache-read tokens to answer a question about one tool output is structurally wrong regardless of
rate. Two further reasons, each independently sufficient: it risks a **cache-write on the agent's
own prefix** (11.5× a read — exactly the mistake this workstream exists to avoid), and it **couples
the compaction model to the agent model**. Re-open only if a provider prices in-context follow-up
questions at a flat rate.

## Metrics

`/stats` gains an `extract` block (purely additive — every pre-existing field keeps its name, so
`deploy/harbor/*.py` keeps parsing unchanged):

| Field | Meaning |
|---|---|
| `calls` | Extraction LLM calls made |
| `calls_avoided` | Calls avoided by the global result cache |
| `calls_suppressed` | Calls declined by the economic gate |
| `cache_hit_rate` | `calls_avoided / cache_lookups` |
| `prompt_cache_read_tokens` / `..._write_tokens` | Preamble caching behavior — **0 read means the breakpoint is inert** |
| `extraction_cost_usd` | What the component spent |
| `gross_value_usd` | What its saved tokens are worth at the rate they'd have been billed |
| **`net_value_usd`** | **The honest headline. Negative = the component is underwater.** |
| `avg_latency_ms` | Mean wall time per call (latency cost on the hot path) |
| `gross_saved_tokens` | Tokens removed |
| `reasons` / `top_reason` | Why extraction ran or was suppressed |

Plus, at the top level of `/stats`: **`llm_truncated`** — replies that stopped at the model's
output cap. That is the worst outcome available, full price for zero result, and it used to be
invisible because a truncated program parses as nothing, exactly like a model that declined to
compact. A non-zero count means the reply budget is too small for what the model is writing.

## Before → After

Captured **live** through the proxy (`pipeline: [extract_llm]`, `strategy: code`,
`model.source: config` → `aws/claude-haiku-4-5`, `economic_gate: false` to force the call). The query
was *"find the auth timeout error and nearby context"*; the model kept the error plus a few
surrounding requests and elided ~118 repetitive successful-request lines:

```
before:  2024 GET /users/0 200 12ms          ← 60 near-identical lines
         … 58 more …
         2024 GET /users/59 200 12ms
         ERROR auth timeout on token refresh
         2024 GET /items/0 200 8ms            ← 60 more near-identical lines
         … 59 more …

after:   2024 GET /users/58 200 12ms
         2024 GET /users/59 200 12ms
         ERROR auth timeout on token refresh
         2024 GET /items/0 200 8ms
         2024 GET /items/1 200 8ms
         [auth timeout error + context; repetitive successful requests elided]
         <<cg:923fff04ab267215>> [full output: call context_guru_expand]
```

Note the reduction is real and useful — the problem was never output quality, it was whether the
call was worth its price on a caching backend.

## Lossiness

Lossy but reversible — the original is stashed and recovered via `context_guru_expand` /
`GET /expand`. The default `rewrite: true` mode is unverified (sanity + strictly-smaller only);
`rewrite: false` gives the verified deletion-only (character-subsequence) guarantee.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `allow_on_caching_backend` | `false` | **Off by default on prompt-caching backends** — measured net-negative there even with the gate working. `true` re-enables it and lets the gate decide per call. |
| `economic_gate` | `true` | Only call the LLM when expected saving > expected cost. `false` restores the older spend-on-size behaviour (and implies `allow_on_caching_backend`). |
| `min_tokens` | *derived* | Output floor. **Unset = derived from context pressure** (no tuning). Set explicitly to pin it (folds into `trigger.min_output_tokens`). |
| `strategy` | `code` | `code` \| `single` \| `rlm` \| `auto` (`rlm` maps to `code`). |
| `model.model` | *the source's own model* | The model to COMPACT with, on the source's endpoint and credential. Empty means the source's own model, which for `incoming` is the agent's frontier model — and compaction on one does not pay: a measured cold-cache sweep through the hosted service cut the provider bill by **$0.63** and spent **$1.25 of opus** doing it (net **−$0.62**). Naming a cheap model here keeps the same account and the same gateway and does the work for roughly a tenth of the call cost. On a multi-tenant deployment this is the only way to get a cheap compactor, since `source: config` has no model there. |
| `model.source` | `incoming` | `incoming` (proxied model+key) or `config` (cheap model via `CHEAP_MODEL*`). **On a multi-tenant deployment `config` resolves to NOTHING** — `staticModel()` withholds the operator's compaction model from tenant traffic on purpose — so the component silently makes no calls however else it is configured. Measured on the hosted service: one account with `source: config`, 251 requests, zero model calls. The settings page now warns in the field itself. |
| `model_max_input_tokens` | *derived* | The extraction model's input budget (see [Context guard](#context-guard)). Pin it for a model whose id nothing can resolve. |
| `trigger` | *derived* | Explicit gate: `min_output_tokens`, `min_request_tokens`, `min_messages`. Setting any pins the trigger. |
| `llm_every_n_requests` | — | Fire the LLM path at most once per N requests per session. |
| `llm_max_per_request` | 0 | Cap LLM calls per firing request (0 = unlimited). |
| `rewrite` | `true` | `false` forces the verified deletion-only (subsequence) guarantee. |
| `skip_file_reads` | auto | Skip line-numbered source dumps when cached; `true`/`false` to force. AUTO now actually works — see the note below. |
| `per_output` | `true` | The hot-path pass: reduce individual tool outputs as they arrive. `false` leaves only the cold-cache sweep, which is the half whose economics are unambiguous. |
| `cold_cache.enabled` | `false` | Sweep the whole transcript on a turn whose prompt cache has expired. See [Cold-cache sweep](#cold-cache-sweep). |
| `cold_cache.min_tokens` | 1000 | Per-output floor for the sweep — lower than the everyday one, because on that turn every candidate is re-billed at the write rate anyway. |
| `cold_cache.min_idle_seconds` | 0 | Demand MORE idle time than the provider TTL implies. Raises the bar, never lowers it. |
| `cold_cache.max_calls` | 0 | Cap model calls in one sweep (0 = unlimited). |
| `fire_on` | `pressure` | `pressure` = the derived context-pressure trigger. `size` = fire whenever a candidate clears `min_tokens`, and demote the economic gate **and** the caching-backend guard to advisory. |
| `llm_max_per_session` | 0 | Cap model calls for the whole session (0 = unlimited). The per-request cap cannot bound a long session: 2 calls x 300 turns is 600 calls. |
| `aggressiveness` | `medium` | `low` \| `medium` \| `high` — the compaction target, taught with worked examples. |
| `context` | `recent` | How much conversation the prompt carries: `goal` \| `recent` \| `full`. |
| `context_messages` | 7 | N for `context: recent`. |
| `marker_mode` | `full` | How the recovery marker is emitted: `full` \| `summary` \| `off`. |

### Context guard

Every call sends **one tool output** in its own prompt, so the size risk here is a single
prompt exceeding the *extraction* model's window — not a conversation that grew too long
(nothing older is ever dropped, and user messages are never touched: only `tool`-role
messages are candidates). Before each call the component checks that

```
(shown body + prompt overhead) × 1.15  +  2048 (reply) + 512  ≤  input limit
```

fits, where the *shown body* is the bounded head+tail the prompt actually carries (a 200k-token
log still travels as a ~8k-token sample), `× 1.15` covers the extraction model tokenizing the
same bytes more heavily than our own `o200k_base` counter, and `2048` is the `max_tokens` the
cheap clients send — most APIs bound input+output against the same window.

The **input limit** is resolved as data, never a constant: `model_max_input_tokens` if pinned →
the window of the config-pinned model from `internal/modelinfo`'s table → the host-resolved
window of the proxied model when `model.source: incoming` → otherwise a conservative
**32768**. `model.source: config` hides the cheap model's id from the component, so it takes
the conservative default; pin `model_max_input_tokens` if its real window is smaller (or
larger, to stop the guard from declining calls it could make).

A candidate that cannot fit is **left verbatim** — no truncation, no dropped messages, no
request on the wire — and the refusal is counted as
`components.extract_llm.gates.over_model_context` at `/stats`. A non-zero count on a
workload that should be compacting means the extraction model's window (or the pin) is too
small for the outputs being seen.

Extraction-model pricing for the gate comes from `CHEAP_MODEL_PRICE_IN`, `_OUT`,
`_CACHE_WRITE`, `_CACHE_READ` (dollars per MTok; defaults are `claude-haiku-4-5` list rates).

## When it shines

**Non-caching backends** — every removed token is a direct saving at the full input rate, so the
break-even is ~10× easier. Also: very large single outputs (>~12k tokens) even under caching;
recurring content that amortizes one call across many replays; and novel prose/log shapes no
deterministic rule anticipates — this is the only component that can compress those.

## When it's inert

Output below the derived floor, low context pressure, **suppressed by the economic gate** (the
common case on a caching backend), throttled out this turn, result served from the global cache,
projection not smaller, or no model available.

See also: [`extract`](extract.md) · [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
