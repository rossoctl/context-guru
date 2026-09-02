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
| Caching, WARM | seen once | **~56,400 tokens** |
| Caching, WARM | recurring (amortized over replays) | **~40,300 tokens** |
| Caching, COLD (TTL expired) | recurring, per content class | **1,000-3,900 tokens** |
| Non-caching | recurring | **~3,100 tokens** |

!!! warning "The value half was corrected on 2026-08-22, and one of those corrections was itself wrong"
    The **rate** fix stands: a replayed removal is a cache-READ token, not a cache-write one -- 12.5x
    cheaper -- so `tokenValue` carries both rates and the gate computes
    `removed x (perToken + reuses x repeatPerToken)`. On a WARM turn both rates ARE the read rate, so
    this leaves the two figures above untouched; it moves only the COLD break-even.

    The **count** fix was wrong and is reverted. The reuse priors were briefly changed from 6/3/4 to
    1.5/0.3/0.6 on a figure of 1.59x, derived as `saved_gross/saved_unique` over only the 13 requests
    that made calls. That is not an amortization figure: `saved_unique` is 46,380 whether summed over
    those 13 rows or all 1,770 -- every unique removal accrues on a calling turn by definition -- so
    restricting the numerator while keeping that denominator subtracts every replay by construction.
    The replays are in the other 1,757 rows (2,408,593 gross against the same 46,380 unique), and
    **per session** the realized multiplier is 4.0, 4.4, 8.0, 12.0, 79.9, 215.0 -- median 12.0,
    minimum 4.0. So 6/3/4 is conservative and inside the observed range, while 0.6 sat below every
    observation. The claim that accompanied it -- that claude-cli drops old turns so a removal is not
    carried forward -- is contradicted by the same ledger: 416 rows are pure replays.

    Pinned by `TestBreakEvenSizesMatchTheDocumentedVerdict`, `TestReusePriorMatchesTheMeasuredLedger`
    and `TestColdTurnPricesReplaysAtTheReadRate`.

The warm figures are why the component is **off by default on caching backends** and why
Running ONLY the cold sweep is the right setting on a prompt-caching agent: the agent truncates every
tool result near 30,000 characters, so **the largest output that can exist is ~7,399 tokens** —
5x below the warm break-even. The gate would only ever be declining. Independently confirmed
on production: the same saved tokens valued at the warm cache-read rate were worth **$0.017
against $0.6039 of spend, ROI 0.03x**.

**Where it does pay is the cold turn.** On a TTL-expired turn the whole transcript is re-billed
at 1.25x fresh, so a removed token is worth 12.5x more, and break-even lands *inside* the
reachable size range — but only for content that actually compresses. See
[content classes](#content-classes).

### Content classes {: #content-classes }

The gate used to price every candidate with one compression ratio learned across all kinds of
tool output at once. Measured over 9,763 captured messages, that pooling is why it could not
tell a paying call from a losing one — the classes span 23x:

| class | measured reduction | cold break-even | reachable under the ~7,399-token ceiling? |
|---|---|---|---|
| `ls -l` listing | 65.5% | ~1,000 tok | yes, comfortably |
| markdown doc | 36.2% | ~2,200 tok | yes |
| multi-file bundle | 34.6% | ~2,300 tok | yes |
| source code | 29.9% | ~3,000 tok | yes |
| `Read` w/ line numbers | 29.2% | ~3,100 tok | yes |
| YAML config | 26.1% | ~3,900 tok | yes |
| ANSI-coloured CLI output | 8.0% | ~14,600 tok | **no** |
| grep / rg output | 6.7% | ~17,500 tok | **no** |
| JSON blob | 2.8% | ~41,700 tok | **no** |
| test/eval result log | 1.5% | ~77,800 tok | **no** |

JSON blobs and ANSI CLI output are **31% of the reachable token mass in the two classes that
compress worst**, which is the direct cause of the flat size-versus-yield relation the corpus
shows (r = −0.10 between candidate size and reduction ratio). **Raising a size floor selects
for exactly the material that cannot pay.** So the class's own measured ratio is handed to the
economic gate in place of the pooled one and the existing expected-saving comparison does the
rest — no new threshold and no list of banned classes. Unrecognised content keeps
using the learned ratio. Refusals are counted as `low_yield_content_class:<class>`.

These use the **measured** compression ratio, and that measurement is the uncomfortable part: on
real captures an accepted extraction removed only **31–254 tokens per call** on outputs of
400–2,000 tokens — an actual ratio around **0.10–0.12**, not the ~0.45 one might assume. The model
declines to cut aggressively, and correctly so: its contract is recall-first.

Most tool outputs are nowhere near the warm break-even — in one measured Terminal-Bench capture
the **largest** tool output was 2,053 tokens, and across 19,805 production requests the largest
that exists at all is 7,399, because the agent truncates every tool result near 30,000
characters. That is why the same
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

## Four defects fixed, August 2026

A measurement pass over real production traffic and fresh Claude Code CLI sessions found four
independent reasons this component was losing money. All four are fixed; the numbers below are
measured, not projected. Full context in
[Measured value, Aug 2026](../results/measured-2026-08.md).

### 1. The model was writing Python, and every program was thrown away

On real traffic `claude-haiku-4-5` returns a plausible filter program on essentially every
call, and the sandbox discarded **12 of 13**. Starlark is a Python *subset*: it has no
generator expressions, and `any(k in ln for k in ids)` — the single most natural way to write
the filter — is a syntax error.

The prompt contract now states what Starlark is not, naming the constructs models actually
reach for: generator expressions, f-strings, `while`, `try/except`, set literals,
`sorted(key=…)` closing over a mutable local. `codeContract` is part of `PromptVersion`, so
results cached against the old prompt are not reused.

**Measured on the same real 33,932-character file read, three runs each:** before, 0/3
produced any output; after, 3/3, at 56%, 83% and 55% reduction.

### 2. Every failure reported the same reason

`RunExtractionDetail` collapsed a sandbox syntax rejection, a transport error and "the model
never replied" into one string, `"no usable program or reply"`. Three causes with opposite
fixes — fix the prompt, retry the call, stop calling — reported identically. That is why
defect 1 survived: the component looked like it was being ignored when it was being answered
and then thrown away.

The reason now escapes to the call record and into the dashboard, as one slug per cause:
`syntax error: <the parser's message>`, `truncated reply: …`, `empty reply`, `program produced
an empty OUTPUT`, `runtime error: …`, `model call failed: <status>`, `result not smaller`, and
`acceptance check: <which check>`. The first thing this bought was a `status 401` in a test
harness, visible immediately instead of after an afternoon; the second was the measurement
below, which found the real dominant failure hiding inside the acceptance bucket.

### 3. The economic gate under-priced its own calls by 21–31×

`callCost` modelled the prompt as `preamble(1463) + min(candidate, 5000) + 200` — at most
6,663 tokens. Under `context: full`, which every cold sweep used to force, the rendered transcript
**is** the prompt. Measured on production: five calls on one request each sent **~138,000**
prompt tokens. For a 3,433-token candidate the gate weighed an expected saving of $0.0077
against an estimated cost of $0.0046 and allowed the call; it cost $0.1422 and removed nothing.

The true figure was already computed two lines from the gate, as `promptOverhead`, and
discarded. The gate now receives it, and `analyticBaseline` scales with it so the
observed-cost blend stays comparable.

### 4. Our own calls were not being prompt-cached

Five calls on one request with `cache_read = 0` and `cache_write = 0` on all five: the same
~138k transcript sent fresh, five times over. The conversation context is computed **once per
request** and is identical for every candidate in it — but it sat in the user message, which
is not cacheable.

It is now a trailing **system** block, inside the prefix `CompleteBlocks` marks — but only when
the request will make more than one call (`Cfg.CacheContext`, set from the final candidate
count). That condition is the design, not an optimisation: a cache write costs 1.25× fresh, so
paying it for a single call is a 25% loss with nothing to read it back.

It also lifts the prefix over the model's minimum cacheable size. The invariant preamble alone
is 1,893 o200k tokens, below `claude-haiku-4-5`'s 4,096-token floor — so on haiku **nothing was
being cached at all**, breakpoint or not.

### 4b. …and the floor itself compared unlike units

`claude-haiku-4-5`'s 4,096-token minimum is in the **provider's** tokens. The gate compared it
against `internal/tokens`' `o200k_base` count. That is a unit error, and it silently declined a
cache the provider was willing to grant: measured 2026-08-19, a **3,673 o200k** prefix billed
**4,217** input tokens and cached perfectly, while `3,673 < 4,096` withheld the breakpoint.

The comparison now converts first (`cheapmodel.minCacheableO200k`, `4,096 / 1.20` = 3,413 o200k,
using the markup measured on haiku itself). Raw `usage` from the gateway, same prefix, three
consecutive calls — this is the whole of the fix's effect:

```
model=claude-haiku-4-5  prefix=3673 o200k  new floor=3413 o200k  (old gate compared it against 4096)
call 1  {"input_tokens":8,"cache_creation_input_tokens":4217,"cache_read_input_tokens":0,
         "cache_creation":{"ephemeral_5m_input_tokens":4217,"ephemeral_1h_input_tokens":0}}
call 2  {"input_tokens":8,"cache_creation_input_tokens":0,"cache_read_input_tokens":4217}
call 3  {"input_tokens":8,"cache_creation_input_tokens":0,"cache_read_input_tokens":4217}
```

Being wrong in the loose direction is nearly free — a breakpoint below the provider's real
minimum is *ignored*, not charged (measured: `input_tokens` identical with and without it), and
`claimCacheWrite`'s release records that and retries. Being wrong tight forgoes the cache
forever, which is what was happening.

Verified against the live gateway: first call `cache_creation_input_tokens: 30007`, every
repeat `cache_read_input_tokens: 30007` with `cache_creation: 0`. On a five-call request that
is $0.114 → $0.038, a **67% reduction**.

### The one thing not fixed here

**94% of the losing spend ($16.38 of $17.48 on the affected account) was made against this
gate's own written advisory.** `fire_on: size` demotes the economic gate to advisory and
leaves the per-turn and per-session caps as the only brake. The gate was right and was
overridden by configuration. The advisory is recorded per call so the counterfactual is
visible, but no code change can rescue an operator from switching the brake off — if you set
`fire_on: size`, read the `economic_gate_advisory` gate count and the net column.

## Accuracy of the code leg, measured (August 2026)

Basis: **40 real tool outputs** captured from Claude Code traffic (`/home/vpcuser/cg-research/bench`
+ SWE-bench proxy captures, 563–15,473 tokens each), replayed through the code leg alone —
`strategy: code`, no deterministic fallback, `rewrite: true`, `aggressiveness: medium`,
compactor `claude-haiku-4-5`, real gateway. Reproduce with
`CG_DIAG=1 CG_DIAG_CORPUS=<dir> go test ./internal/extract/ -run TestDiagCodeLegCorpus -v`.

| | before | after |
|---|---|---|
| accepted | **15/40 (38%)** | **27/40 (68%)** |
| tokens removed over the corpus | 27,680 (20.6%) | **61,240 (45.6%)** |
| our own spend | $0.3061 | $0.2972 |
| spend per accepted extraction | $0.0204 | **$0.0110** |

Rejections, by cause — the slugs are new, and the breakdown is the whole point:

| cause | before | after |
|---|---|---|
| `acceptance check` | 12 | **1** |
| `result not smaller` | 10 | 5 |
| `program produced an empty OUTPUT` | 2 | 4 |
| `syntax error` / `truncated reply` / `runtime error` | 1 | 0 / 1 / 1 |
| `model call failed` (transient gateway) | 0 | 1 |

What actually moved the number:

1. **The keep-set was full of English words.** `HarvestIdentifiers` matches any word of four
   or more letters, and it harvests from the agent's recent turns — which are prose. So
   `against`, `including`, `large`, `exactly`, `credits` entered both the KEEP list the model
   is told to preserve verbatim and the recall check that refuses any reduction dropping one.
   **13 of 40 calls were refused for "dropping" a common English word** — a good compaction,
   paid for in full and thrown away because it removed a noise line containing the word
   "large". A reference now has to carry a mark prose does not: a separator, a digit, or an
   inner capital (`parse_config`, `src/api/users.py`, `IndexError`, `sympy-1.11`). This was
   invisible until the acceptance bucket was split, and it is the single largest lever here.
2. **The Starlark contract said things that are not true.** Verified against the real
   interpreter: `%`-formatting, `dict.setdefault`, `sorted(key=…)`, lambda and **dict**
   comprehensions all work, and the block forbade all of them — fixed overhead on every call,
   pushing the model into contortions for nothing. The two forms that actually killed calls in
   production were not named at all: a **type annotation** (`kept: list = []` — the recorded
   `extract.star:4:7: got ':', want newline`) and a **set comprehension**. Both are now named,
   the false bans are gone, and `TestTheContractOnlyBansWhatTheSandboxActuallyRejects` checks
   every claim in both directions against the interpreter.
3. **One repair round-trip on a syntax error.** The parser's message is actionable on its own,
   so the rejected program and the error go back for exactly one correction — without the tool
   output, since fixing syntax needs the program and not the data. Measured: 2 attempts, 2
   recovered, ≈$0.004 marginal per attempt. It pays at the cold-turn valuation of a removed
   token (the cache-creation rate — $2.50/MTok on this gateway, read from the request's own rate
   card rather than a constant; see [Economics](#economics)) and is about break-even at the fresh rate; it does not pay if the removed
   tokens would only ever have been billed as cache reads. Kept on, bounded to one retry.
4. **Truncation is no longer reported as a syntax error.** A reply cut off at the output cap
   leaves an incomplete program that fails with whatever error the cut produced. Detected from
   the source (unbalanced brackets or an unterminated string — a complete program always
   balances), so "raise the reply budget" stops looking like "fix the prompt".

**Prompt size.** Example D in the aggressiveness blocks restated the source-file skeleton rule
that `codeContract` already gives in prose (327 tok in medium, a near-verbatim second copy at
299 tok in high). Both are cut to the loop alone / one sentence: the preamble goes 1,893 → 1,798
tokens at `medium` and 1,973 → 1,778 at `high` (the shared contract grew 52 tokens for the
corrected Starlark facts, so `low` goes 1,547 → 1,599). This buys **money, not latency** —
measured, prompt build is 0.089 ms against a 1,812 ms p50 gateway floor on an 8-token call.

### The result derives from the input, and that is now checked

In the shipped default (`rewrite: true`) **nothing** verified the relationship between the
result and the input: the containment proof is deletion-only, `MinKeepRatio` is 0, and a
pervasive keep-id is exempt. A paraphrase, a renumbered file read or an invented value passed,
and the only real protection was that the original stays recoverable via `context_guru_expand`
— at the cost of an agent turn.

The choice made here is **not** to default `rewrite: false` (that would refuse the column
strips and elision markers the prompt asks for, and the measured deletion-only leg is a weaker
compactor) but to require the result to **derive** from the input: the fraction of the result's
bytes matchable in order against the body, with the elision markers the model was told to add
excluded, must be ≥ 0.9. Measured over the corpus, **every accepted reduction scored 1.00**
(0.93 and 0.97 in one earlier run) — a filter deletes, it does not retype — so the floor costs
0 acceptances and 0.2 ms, while a reordered paraphrase or a fabricated value lands far below
it. It is a calibration knob, not a law; the number is in `minDerivationRatio`.

Still unmitigated: a reduction that drops a record the agent needed next but keeps every
harvested id (harm #1 — recoverable via expand, at a turn), and a cross-session cache hit
derived toward a different goal (harm #10 — recoverability is gated, correctness is not
re-verified).

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

**`context`** is `goal` | `recent` (default) | `full`, with `context_messages` (2) as the N for
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
| Call cost | **Analytic and size-aware** — `(preamble (1,893 o200k, measured) + shown content + overhead) × 1.29` to convert our `o200k_base` count into the tokens the provider bills, priced at real model rates — then reconciled with the observed mean once real calls exist. A flat per-call constant is not just imprecise, it **deadlocks**: pricing every call at the $0.012 average (≈5× the true cost on small outputs) suppressed everything, so nothing was ever observed and the estimate could never correct itself. Measured: $0.0024/call actual vs the $0.012 prior |
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
  # extract_llm_sweep, in its own block, leaves ordinary turns alone
  extract_llm_sweep:
    enabled: true
    min_tokens: 1000
```

On such a turn the sweep lifts the tail gate (nothing is cached, so depth is free), prices saved
tokens at the cache-write rate, and escalates to the agent's own model if the transcript will not
fit the extraction model's window. Every removal leaves a `<<cg:HASH>>` marker with a one-line
summary, and the result is frozen so later warm turns replay it byte-for-byte and the new prefix
stays cache-stable.

### The sweep no longer forces `context: full` — measured before/after

> **Corrected 2026-08-20.** An earlier version of this section presented the whole
> -$0.039 -> +$0.020 turnaround as the effect of dropping the forced `context: full`.
> Independent re-measurement on the fixed code shows `full` is ALSO net-positive
> (+$0.0119, accepting 3/3), so the keep-list fix in `HarvestIdentifiers` carries most of
> the change on its own and dropping the force adds roughly +$0.005. The before/after
> below spans two independent changes; do not attribute it to one.

It used to, on the argument that judging what an old message may lose requires knowing what
happened since. The argument does not survive measurement, and removing that one line is the
largest single change to this component's economics.

`full` **is the whole request**: at 127 turns the rendered context was 138,596 tokens against a
138,341-token request, sent once per candidate. Break-even removal at k=4 falls from **113,286
tokens (1.13x the transcript — structurally impossible) to 6,833**. The counter-argument was real
but is about the **keep-list**, not the context — a full-transcript context took acceptance from
3/4 to 0/6 because every unique token in the noise became a required identifier — and
`HarvestIdentifiers` now reads `ctxRecent` explicitly, so the two are separate concerns.

Replayed on `bench/cold.jsonl` (9 real Claude Code requests, 3 of them verified `ttl_expiry`
with `cache_read = 0`), `CG_IDLE=430`, compactor `claude-haiku-4-5`, agent model
`aws/claude-sonnet-5` at $2.00/MTok. Identical config on both sides; only the code differs.
**n=3 per arm, because this arm's LLM output is noisy enough that the shipped default's net sign
flips between identical runs** (`bench/BASELINE.md`):

| | calls | prompt tok/call | our spend | tokens removed | accepted | net (see below) |
|---|---|---|---|---|---|---|
| **before** (`full` forced) | 1 | 36,686 | $0.0385–0.0396 | **0** | 0/3 runs | **−$0.0385…−$0.0396** |
| **after** (`recent`) | 2 | 18,657 + 13,101 | $0.0344–0.0366 | 87,812–95,944 | 3/3 runs | **+$0.0157…+$0.0212** (n=6, median +$0.0172) |

The same candidate (15,473 tokens) went from a 36,686-token prompt rejected by the acceptance
check on all three runs, to an 18,657-token prompt accepted on all three, removing 14,438 tokens.
**Halving the prompt is the smaller half of the win; the acceptance rate going 0/3 → 3/3 is the
larger one**, and it is the keep-list effect above, now visible because the context shrank.

!!! warning "How that net is computed, and why it is not the `$@write` column"
    The arm's raw `net@write` reads **+$0.196…+$0.205**, and quoting that would be dishonest:
    `removed` (95,944) exceeds `attempted` (27,189), i.e. most of it is history the provider had
    already cached. Priced honestly instead:

    * **14,568–16,829 tokens** were removed *uniquely, on `ttl_expiry` turns* — the sweep's own
      work, correctly valued at the cache-creation rate ($2.50/MTok) = **$0.0403–0.0421**;
    * the remaining ~79,000 are that reduction **replayed** on later warm turns of the same
      session, which would have been billed as cache reads ($0.20/MTok) = **$0.0158**.

    Total value $0.0561–0.0579 against $0.0344–0.0366 of our own spend. Positive, ~20x smaller
    than the headline column, and stable in sign across all three runs — which the shipped
    default's +$0.0097 is not.

`context: full` still exists and still means full; it is simply no longer imposed. Also measured:
on real Claude Code traffic `recent` renders **2,734–5,681** tokens, not the 100–436 of a
synthetic transcript — the last seven messages are mostly large tool results, clipped at
`recentContextCap`. So the real prompt reduction is ~2x, not ~16x, and the R\* table above is an
upper bound on this corpus.

### The context cache write is now earned before it is read

`CacheContext` moves the per-request conversation context into a trailing cacheable **system**
block, and was set whenever a request had more than one candidate — but `claimCacheWrite`
deliberately withholds the breakpoint from **concurrent siblings** (an entry that is only ever
written costs 1.25x fresh and buys nothing), so with `llmConcurrency = 4` the first call took the
write slot and calls 2–4 sent no mark and paid plain fresh input for the identical context.
Measured on production: five haiku calls on **one** request each sent ~138,000 prompt tokens with
`cache_read = 0` **and** `cache_write = 0`.

The sweep therefore issues its **first call alone, then the rest concurrently** — one writer,
then readers. At T = 180k, k = 4 that moves break-even removal from 198,620 tokens to **79,088**.

**The cost is wall clock and the trade is deliberate.** Per-call latency here is gateway *queue*
time, not prompt size (measured: an 8-token call has a 1,812 ms p50 floor and is not faster than
an 8k-token one), so serializing one call adds roughly one whole queue round — ~2–4 s at p50,
12–16 s in the tail. It is paid only on a turn whose entire transcript is re-billing at 1.25x
fresh; the warm per-output path stays fully concurrent, where the extra second would buy a
fraction of a cent. `TestSweepEarnsTheContextCacheWriteBeforeReadingIt` pins both halves.

!!! note "Not exercised on the current bench corpus"
    On `bench/cold.jsonl` each request yields a single candidate, so `CacheContext` is `false`
    and this path never engaged in the numbers above. Its effect is proven by unit test and by
    the arithmetic, not by that corpus — an honest gap, and the reason the before/after table
    credits it with nothing.

!!! tip "The deterministic offloaders can do this too — and they should go first"
    `mask`, `failed_run` and `collapse` take the same `cold_cache` option (also off by default) and
    lift the same gate without spending a model call. On the measured window the cold path of *this*
    component cost **$12.71 of our own LLM spend to save 3,161 tokens** — about 4x worse per token
    than its warm path — so where a deterministic offloader can take a cold-turn token, it should:
    same saving, no call. Enable both and the deterministic pass runs first, leaving the sweep the
    residue. See [Freeze / cold_cache](../design.md#the-one-turn-where-depth-is-free-cold_cache) for
    the measured delta, which is single-digit percent of cold spend rather than all of it.

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

2. **Provider prompt cache on the extraction preamble.** The 1,893-o200k invariant
   preamble is sent as a stable `system` block with a `cache_control` breakpoint (a leading system
   message on the OpenAI backend, which has no explicit breakpoints).

    !!! warning "Measured: this buys nothing on `claude-haiku-4-5`"
        A breakpoint below the model's **minimum cacheable prefix** is silently ignored — no error,
        `cache_creation_input_tokens: 0`. That minimum is **4096 tokens on `claude-haiku-4-5`** and
        1024 on `claude-sonnet-5`, against a **1,893-o200k** preamble — and the floor is in the
        provider's tokens, so the comparison converts first (see [4b](#4b-and-the-floor-itself-compared-unlike-units)). Verified against the gateway:

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

`/stats` gains an `extract` block (purely additive — every pre-existing field keeps its name).

!!! danger "These are NOT this component's figures. Read `extract.by_component`."
    The block's top-level keys are the **sum across every extraction component** — this one and
    [`extract_llm_sweep`](extract_llm_sweep.md), which both write the same counters. The two have
    opposite economics: per-output calls on a cheap model here, one call on the request's own
    frontier model there.

    This document used to present the keys below as `extract_llm`'s own, and that reading cost a
    whole investigation. On a measured 45-run benchmark the block reported **101 calls at 59,009 ms
    mean latency and a net value of −$1.162**, which was attributed to `extract_llm` and used to
    charge it a 5,452 ms/request latency cost — while this component's own debug record showed
    **zero surviving candidates on all 374 requests**, i.e. not one call. The 101 were very nearly
    the sweep's 96 asks. Any per-component cost, latency or call claim must come from
    `extract.by_component.extract_llm`.

    Names are frozen for `/metrics` (`cg_extract_calls_total`, `cg_extract_cost_usd`,
    `cg_extract_net_value_usd`, `cg_extract_latency_ms`), which off-repo alert rules query — **not**
    because the benchmark harness parses them. It does not: nothing under `deploy/` reads any key
    in this block.

| Field | Meaning |
|---|---|
| `by_component` | Every field below, keyed by the component that recorded it. **The per-component-safe figures.** |
| `calls` | Extraction LLM calls made — *summed across extraction components* |
| `calls_avoided` | Calls avoided by the global result cache |
| `calls_suppressed` | Calls declined by the economic gate |
| `cache_hit_rate` | `calls_avoided / cache_lookups` |
| `prompt_cache_read_tokens` / `..._write_tokens` | Preamble caching behavior — **0 read means the breakpoint is inert** |
| `extraction_cost_usd` | What was spent. Read `cost_source` before quoting it |
| `cost_source` | Where that figure came from: `component` (each call priced itself — trust it), `host_total` (the host's process-global cheap-model spend, a superset that also carries `summarize` and `agentdiet`), `partial` (some calls unpriced, so the total is a **floor**), `unpriced` (this row made calls and priced none — nothing is known), `none` (no calls; `0` is true) |
| `unpriced_components` | On the aggregate: which components' calls priced nothing, i.e. what a `partial` or `host_total` total is **short of**. Omitted when everything priced itself |
| `gross_value_usd` | What its saved tokens are worth at the rate they'd have been billed |
| **`net_value_usd`** | **The honest headline. Negative = underwater.** `null` when the spend is not known |
| `avg_latency_ms` | Mean wall time per call (latency cost on the hot path) |
| `gross_saved_tokens` | Tokens removed |
| `reasons` / `top_reason` | Why extraction ran or was suppressed |

Per-component, in `components.extract_llm`: **`acted` counts free replays.** A frozen decision
re-spliced on a later turn saves tokens and costs nothing, and it landed in the same counter as the
call that derived it — `acted: 239` beside `reapplied_same_session: 2,291` was read as 239 paid
extractions. Use `acted_fresh` (paid work) and `acted_replay` (free) instead.

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
`GET /expand`. The default `rewrite: true` mode is verified only to the extent that the result
must DERIVE from the input (≥90% of it traceable, in order, to the body; see above) plus sanity
and strictly-smaller; `rewrite: false` gives the full deletion-only (character-subsequence)
guarantee. The one-line SUMMARY spliced next to the marker is clipped to 120 runes, because an
over-long one used to abandon the whole reduction rather than truncate.

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `allow_on_caching_backend` | `false` | **Off by default on prompt-caching backends** — measured net-negative there even with the gate working. `true` re-enables it and lets the gate decide per call. |
| `economic_gate` | `true` | Only call the LLM when expected saving > expected cost. `false` restores the older spend-on-size behaviour (and implies `allow_on_caching_backend`). |
| `min_tokens` | 300 | Output floor. The literal default is **300**; leaving it unset also means the *derived* context-pressure trigger governs when a request is worth a call, and setting it pins the floor to your number instead (it folds into `trigger.min_output_tokens`). Under `fire_on: size` an unset floor is raised to **2000**, because 300 is a threshold nearly every tool output clears — that combination is the 271-call failure mode. |
| `strategy` | `code` | `code` \| `single` \| `rlm` \| `auto` \| `deterministic`. `deterministic` makes **no model call at all** (the projection runs locally), which is the one value the settings form used to omit — a stored `deterministic` was then not recognised and got rewritten to `code`, silently turning an LLM-free configuration into one that spends. |
| `model.model` | *the source's own model* | The model to COMPACT with, on the source's endpoint and credential. Empty means the source's own model, which for `incoming` is the agent's frontier model — and compaction on one does not pay: a measured cold-cache sweep through the hosted service cut the provider bill by **$0.63** and spent **$1.25 of opus** doing it (net **−$0.62**). Naming a cheap model here keeps the same account and the same gateway and does the work for roughly a tenth of the call cost. On a multi-tenant deployment this is the only way to get a cheap compactor, since `source: config` has no model there. |
| `model.source` | `incoming` | `incoming` (proxied model+key) or `config` (cheap model via `CHEAP_MODEL*`). **On a multi-tenant deployment `config` resolves to NOTHING** — `staticModel()` withholds the operator's compaction model from tenant traffic on purpose — so the component silently makes no calls however else it is configured. Measured on the hosted service: one account with `source: config`, 251 requests, zero model calls. The settings page now warns in the field itself. |
| `model.provider` | `anthropic` | Wire dialect for a config-pinned endpoint: `anthropic` \| `openai`. |
| `model.base_url` | *the provider's public API* | Pin a dedicated endpoint (a gateway, a self-hosted server) as a full URL. |
| `model.api_key` | *the process env key* | **Credential** for the pinned endpoint. Empty falls back to `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` / `OPENAI_API_KEY`, and a **hosted** deployment refuses that fallback rather than bill a tenant's compaction to the operator (`offload.AllowEnvModelKey`). The settings page can set it and never displays it back. |
| `model.auth` | `x-api-key` | Anthropic only: how the key is sent. `bearer` is what a LiteLLM/gateway front end expects. |
| `model_max_input_tokens` | *derived* | The extraction model's input budget (see [Context guard](#context-guard)). Pin it for a model whose id nothing can resolve. |
| `trigger` | *derived* | Explicit gate: `min_output_tokens`, `min_request_tokens`, `min_messages`, plus the window fractions `min_request_frac`, `min_output_frac`, `huge_output_frac`. Setting any of the absolute thresholds pins the trigger; a fraction only ever *raises* the absolute floor it resolves against. |
| `llm_every_n_requests` | — | Fire the LLM path at most once per N requests per session. |
| `llm_max_per_request` | 0 | Cap LLM calls per firing request (0 = unlimited). |
| `rewrite` | `true` | `false` forces the verified deletion-only (subsequence) guarantee. |
| `skip_file_reads` | auto | Skip line-numbered source dumps when cached; `true`/`false` to force. AUTO now actually works — see the note below. |
| `fire_on` | `pressure` | `pressure` = the derived context-pressure trigger. `size` = fire whenever a candidate clears `min_tokens`, and demote the economic gate **and** the caching-backend guard to advisory. |
| `llm_max_per_session` | 0 | Cap model calls for the whole session (0 = unlimited). The per-request cap cannot bound a long session: 2 calls x 300 turns is 600 calls. |
| `aggressiveness` | `medium` | `low` \| `medium` \| `high` — the compaction target, taught with worked examples. |
| `context` | `recent` | How much conversation the prompt carries: `goal` \| `recent` \| `full`. |
| `context_messages` | 2 | N for `context: recent`. **The biggest lever on per-call cost**: production sent 3,785 prompt tokens to compress a 2,700-token candidate at 7, so the candidate was a third of the call. Cutting it also shrinks the keep-list harvested from the same window, which is what "dropped a referenced identifier" rejections are counted against (28 of 31 production rejections) — so the cheaper prompt and the higher acceptance rate are one change. It trades away our own prefix cache on requests making MANY calls. |
| `max_chars` | 4000 | Window for the model-free deterministic projection. The window is line-aligned and names what it dropped; a result that hits the cap with nothing saying so is refused whatever this is set to. |
| `marker_mode` | `full` | How the recovery marker is emitted: `full` \| `summary` \| `off`. |

### Context guard

Every call sends **one tool output** in its own prompt, so the size risk here is a single
prompt exceeding the *extraction* model's window — not a conversation that grew too long
(nothing older is ever dropped, and user messages are never touched: only `tool`-role
messages are candidates). Before each call the component checks that

```
(shown body + prompt overhead) × 1.65  +  4096 (reply) + 512  ≤  input limit
```

fits, where the *shown body* is the bounded head+tail the prompt actually carries (a 200k-token
log still travels as a ~8k-token sample), `× 1.65` covers the extraction model tokenizing the
same bytes more heavily than our own `o200k_base` counter (**measured** 2026-08-19: 6,396 o200k
billed as 8,222 on `claude-haiku-4-5` and 10,574 on `aws/claude-sonnet-5`, i.e. 1.29x and 1.65x —
the old `1.15` under-stated both, and this margin's only job is keeping a request inside a
window, where under-counting puts a prompt on the wire the upstream may reject), and `4096` is the `max_tokens` the
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

!!! warning "Set them, or the gate spends against a list price nobody checked"
    None of the four is set on this deployment, so the gate priced every call at haiku **list**
    ($1.00/$5.00 per MTok) while the dashboard priced the same request from the operator's own
    card ($0.80/$4.00) — ~25% apart, in the numerator of every allow/suppress decision, with
    nothing reporting the divergence. A component cannot reach the provider's price table, so
    it now says so instead: a request that spends on a *named* cheap model with no
    `CHEAP_MODEL_PRICE_*` configured records the gate `cheap_model_price_unconfigured` at
    `/stats`. A silent 25% is worse than a wrong 25% — the wrong one can be corrected.

**The value of a saved token is no longer a constant either.** It is read from the request's own
rate card (`Ctx.SelfRates`, resolved from the same provider price table the dashboard uses); the
`agentFreshPerMTok` / `agentCacheReadPerMTok` / `agentCacheWritePerMTok` literals are only the
fallback for when the table said nothing. This settles a 27% disagreement that sat under every
cold-turn decision: the code said a cold-turn token was worth **$3.75/MTok** and
`docs/results/measured-2026-08.md` §9 said **$4.75/MTok**. Neither is right here — $3.75 is
1.25x `claude-sonnet-4-5`'s $3.00 *list* rate and $4.75 is the opus-5-era figure, while this
gateway bills `aws/claude-sonnet-5` at **$2.00 in / $10.00 out** per MTok (derived 2026-08-19 by
solving the recorded `cost_usd` and token-tier columns of two captured corpora simultaneously),
making a cold-turn token worth **$2.50/MTok**. A literal cannot be right for every operator.

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
