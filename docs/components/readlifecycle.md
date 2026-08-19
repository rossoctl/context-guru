# readlifecycle

!!! warning "Offload — reversible. Ships OFF, in no preset."
    Offloads a file `Read` whose body no longer describes the file: the file was **edited** later
    (STALE) or **read again** later (SUPERSEDED). A **fresh** Read is never touched. Measured on
    this box's traffic it removes **0 tokens on every warm arm**, for the structural reason in
    [Why it is off](#why-it-is-off-by-default). Enable it only for the cold-sweep case below.

## What it detects

`schema.ToolCalls` pairs every `tool_result` with the `tool_use` that produced it, so the component
reads the tool **name** and its **arguments** — for a `Read`, the `file_path` and the `offset`/`limit`
window. It walks the transcript in index order and classifies each Read against **strictly later**
events only:

| class | proof in the transcript | why it can go |
|---|---|---|
| **STALE** | a later `Edit`/`Write`/`MultiEdit`/`NotebookEdit` on the same path | the body in context is **factually wrong** — content and line numbers that no longer exist. This is a correctness argument before it is a token argument. |
| **SUPERSEDED** | the same path read again later with the **same** `offset`/`limit` | the later Read is authoritative; the earlier body is redundant. |
| **FRESH** | neither | **never touched.** |

The marker states only what is true — *"was modified later"*, *"was read again later"* — never
"identical to", which a re-indent would already falsify. In `marker_mode: full` (the default) the
original is stashed and `context_guru_expand` restores it byte-for-byte.

### Deliberately conservative, in four places

- **Supersession keys on (path, offset, limit)**, not on path alone. Claude Code reads *ranges*; a
  later read of lines 500–600 does not replace an earlier read of the whole file. Containment (a
  later full read covering an earlier partial one) is **not** modelled — worth 867 tokens on the
  entire corpus here, which does not pay for the reasoning.
- **An Edit anywhere in the file staleizes every prior Read of it**, including a Read of a different
  range: an edit shifts the line numbers the Read printed.
- **Bash commands are not edits** unless `bash_edits` is on (default **off**), and then only for
  `> file`, `>> file`, `tee`, `sed -i`, `patch`, `truncate` — forms whose operands *are* the files
  written. `git apply`, `python script.py`, a Makefile and a generator are **not** counted: the file
  they write is not in the command text, and guessing there deletes **correct** context.
- **An image Read is never rewritten.** All 23 Reads in `capture-tb.jsonl` are PNGs; a text rewrite
  would destroy the image block. `schema.Rewritable` is the guard.

## The measured split — ours vs headroom's

headroom's `read_lifecycle.py` records **67% stale / 12% superseded / 20% fresh of Read bytes**.
Measured over `testdata/read_lifecycle.json` (every Read/Edit sequence in the SWE-bench,
Terminal-Bench and Claude Code captures on this box — 6 transcripts, 34 text Reads, 32,650 tokens):

| class | Reads | share of Reads | tokens | share of Read tokens | headroom |
|---|--:|--:|--:|--:|--:|
| stale | 15 | 44.1% | 6,889 | **21.1%** | 67% |
| superseded | 0 | 0.0% | 0 | **0.0%** | 12% |
| fresh | 19 | 55.9% | 25,761 | **78.9%** | 20% |

**Our numbers are the truth for our traffic, and they invert headroom's.** Two reasons, both
visible in the corpus:

- **Superseded is zero.** The only path-level repeat Reads here are Terminal-Bench's image Reads
  (excluded) and one partial re-read of a *different* range. Honouring the range key — which
  correctness requires — leaves nothing.
- **Fresh dominates by tokens.** Interactive Claude Code sessions on this box are read-heavy
  *investigation*: they read big files and edit nothing, so all 17 of their Reads are fresh and they
  carry 79% of all Read tokens. Stale Reads live almost entirely in SWE-bench, where the agent edits
  — and there each Read is small.

Calibration this sits inside: `Read` output is **6.9%** of interactive input tokens (`Bash` is
56.3%), so even a perfect result in this class is bounded at roughly 5% of input.

## Why it is off by default

Deterministic replay through `cg-research/bench/ab.sh` (same capture, same clock, config the only
difference), arm `pipeline: [readlifecycle]` against `pipeline: []`:

| capture | requests | removed | gates |
|---|--:|--:|---|
| `short.jsonl` | 5 | **0** | `fresh_read` |
| `long.jsonl` | 35 | **0** | `fresh_read:131`, `no_file_reads:2` |
| `mixed.jsonl` | 21 | **0** | `fresh_read` |
| `cold.jsonl` (`CG_IDLE=430`) | 9 | **0** | `fresh_read` |
| SWE-bench session (122 reqs, the most file-active in `capture-swebench.jsonl`) | 122 | **0** | **`cached_prefix:1125`**, `fresh_read:720`, `below_min_tokens:136` |

The four interactive arms remove nothing because **every Read in them is fresh** — the safety
property working as designed, not a broken component. The SWE-bench row is the interesting one:
**1,125 stale-Read candidates were declined by the cache-tail gate**, and that is *structural*. A
Read only becomes stale once a later `Edit` appears, and by that turn the Read is already inside the
provider's cached prefix, so the gate can essentially never let this component act on a warm turn.

### `stale_at_depth` — measured, and it does not pay

The obvious response is to lift the gate for the stale class. That costs one **re-anchor**: rewriting
the Read at index *i* forces a cache-write of everything after *i*, once, at the turn the Read goes
stale. The freeze then replays the decision forever, so the recurring saving is the removed body on
every later turn. Whether that pays depends entirely on how far the Read is from the tail when its
file is finally edited — so it was measured, not argued
(`TestReanchorCostOfStaleAtDepth`, `internal/tokens`/o200k, gateway price $2.00/MTok):

| session | turns | stale transitions | one-time re-anchor | recurring saving | distance Read→cache boundary | break-even |
|---|--:|--:|--:|--:|--:|--:|
| SWE-bench, 1 instance | 122 | 22 | 188,319 tok = **$0.471**@write | 13,541 tok/turn = **$0.0027**/turn@read | mean **30.5** msgs, max 100 | **174 turns** |
| SWE-bench, 3 instances | 355 | 45 | 475,613 tok = **$1.189**@write | 32,595 tok/turn = $0.0065/turn@read | mean **37.3** msgs, max 142 | **182 turns** |

The Read→Edit loop is **not** tight: the agent reads a file, explores for ~15 turns, then edits it.
So the re-anchor is ~8.6k already-cached tokens per transition, and at the tier this corpus is
actually billed (≈90% `cache_read`, 0.1× fresh) the saving needs **~180 turns** to repay it — on
sessions that run 122. `stale_at_depth` is therefore **net negative on real traffic** and ships off.
(The re-anchor figure is an upper bound: two transitions in one turn re-anchor the same suffix once.
Halving it still leaves break-even beyond the session length.)

### `cold_cache` — the one place it does pay

On a turn whose prompt cache has provably expired there is no prefix to disturb, so the depth
restriction is free. Replaying a **single** SWE-bench request with `cold_cache: true`
(`CG_IDLE=430`, `frozen=0`, `attempted=100%` — so `removed < attempted`, a real saving and not
cache invalidation):

| request | before | after | removed | $@cache_write | $@cache_read |
|---|--:|--:|--:|--:|--:|
| last turn of the session | 37,329 | 25,005 | **12,324 (33.0%)** | **$0.0308** | $0.0025 |
| mid-session turn | 21,883 | 13,634 | **8,249 (37.7%)** | $0.0206 | $0.0016 |

A third of a cold SWE-bench request is stale file Reads. Cache-write is the correct tier for a cold
request, so that is a genuine **+$0.031 for one request, at zero LLM cost** — this component makes
no model calls. It applies only on turns idle past the provider TTL, which is why `cold_cache` is
off by default like every other component's.

!!! danger "Do not read the summed cold replay as a saving"
    Replaying all 122 requests with `CG_IDLE=430` reports `removed=782,038 (28.5%)`, `$1.96`@write —
    against `attempted=37,329`. **`removed ≫ attempted` is cache invalidation, not saving.** That run
    fabricates a 430-second gap before *every* turn; real SWE-bench turns are seconds apart. The
    honest figure is the per-request one above.

## Cache stability

The component rewrites message **history**, which is the cached prefix of every later turn, so
byte-stability is the whole ballgame. Three properties, each with a test:

1. **The classification is monotone.** It is a function of strictly later events, and events only
   accumulate, so a Read goes fresh → stale/superseded **exactly once** and never flaps back.
   Ordinals are absolute `req.Input` indices and matching is strictly-earlier-only, so appending a
   turn can never reclassify an earlier Read — headroom's `cross_turn_dedup` invariant 1, pinned by
   `TestFrozenPrefixIsByteStable` on the real fixture with the tail gate on.
2. **The replacement is content-addressed and deterministic.** It is a pure function of
   (content, class, path, config); the marker key is `sha256(original)`. No map is iterated on the
   path that produces output — `schema.ToolCalls`'s map is only ever looked up by an ascending index
   — so Go's randomized map order cannot move a byte. `TestDeterministicAcrossProcesses` re-runs the
   transform in three **child processes** and compares a hash of the whole rewritten transcript.
3. **A decision, once made, is replayed forever.** New decisions are confined to the uncached tail
   (`Ctx.TailOnly`); once made they are frozen by content hash and re-applied on **every** later
   turn at any depth, so a message never flips offloaded → full → offloaded as the tail boundary
   moves past it (`TestFrozenDecisionReplaysForever`, 50 turns). A freeze the store has since
   dropped is re-derived at depth (`repairLostFreeze`) because the provider is already holding the
   offloaded bytes — there, leaving the body verbatim is the cache-destructive move.

Cache safety comes from **determinism**, not from "only touch the delta" — which is also why gating
this component on a per-turn condition would be a bug: applied on alternate turns it re-anchors on
every one. For any given session it is `stale_at_depth`/`cold_cache`-shaped ALWAYS or NEVER.

## Config

```yaml
pipeline: [readlifecycle]      # in no preset; enable explicitly
components:
  readlifecycle:
    min_tokens: 100            # only offload a Read above this many tokens
    stale: true                # offload a Read whose file was later edited
    superseded: true           # offload a Read re-read later at the same range
    bash_edits: false          # count narrow shell write forms as edits
    stale_at_depth: false      # offload a stale Read inside the cached prefix (net negative — above)
    cold_cache: false          # act at any depth on a turn idle past the provider cache TTL
    marker_mode: full          # full | summary | off
```

Gate reasons on `/stats`: `no_file_reads`, `fresh_read`, `non_text_blocks`,
`marker_or_kept_verbatim`, `below_min_tokens`, `cached_prefix`, `marker_no_win`.

## Recommendation

**Leave it off.** It belongs in no preset: on warm traffic it removes 0 tokens, and the only knob
that would change that (`stale_at_depth`) is measured net-negative by ~2 orders of magnitude on the
break-even. Turn it on — with `cold_cache: true` — for a deployment running long **editing** agents
with real idle gaps, where it removes a third of a cold request for no LLM spend. Do not turn it on
for read-only investigation traffic: there it is pure latency (0.2–1.9 ms/req) for zero tokens.
