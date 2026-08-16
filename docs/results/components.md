# Component internals & real examples — context-guru vs headroom vs rtk

How each tool actually compacts context: what every component does, when it triggers,
how it works, and **real before→after examples pulled from the 50-task run logs**.

The three compaction tools fall into two families:

- **context-guru** and **headroom** are **request-stream proxies**. Both are
  **live-zone-only** (they only touch the newest, not-yet-cached content and leave the
  provider's cached prefix byte-identical) — that shared discipline is why neither busts
  the cache. They differ in *how* they compress: context-guru is a **hybrid** (deterministic
  passes + a cheap LLM for relevance-aware skeletonization), headroom is **fully
  deterministic** (AST / ML-scorer / structural compressors, no generative model).
- **rtk** is a **shell-level hook** (fully deterministic, no model): it compresses Bash
  command output *before* it enters the transcript, so it is cache-safe by construction (the
  compressed form is what gets cached — there is no cached prefix to invalidate).

All numbers are from the matched 50-task run (see [comparison.md](comparison.md)).

---

## context-guru — pipeline `[format, dedup, failed_run, cmdfilter, extract_llm, extract, cacheinject]`

!!! note "This is the pipeline as this run used it — `codesmart` has moved on twice since"
    Two differences from the shipped preset today, both left as-is above because the heading
    records what the run actually executed:

    - it ends in `cacheinject`, where `codesmart` now ends in **`cachesplit`** — breakpoint
      placement was removed from every preset in
      [#36](https://github.com/rossoctl/context-guru/pull/36);
    - it has no `toon`, which `codesmart` now runs second.

    Current composition:
    `[format, toon, dedup, failed_run, cmdfilter, extract_llm, extract, cachesplit]` — see
    [Presets](../reference/presets.md).

Two type-enforced kinds: **Reformat** (lossless repack, no stash) and **Offload**
(lossy-but-reversible: leaves a `<<cg:HASH>>` marker + stashes the original, recoverable
via the `context_guru_expand` tool). Every component is fail-open isolated — an error or
a size-growth reverts *that component only*.

**Cache-safety invariant.** An offloader only ever *creates* a new compaction on content
in the uncached tail (`TailOnly`, message index > `MaxCachedIdx`), and once it compacts an
output it **freezes the exact replacement bytes and replays them on every later turn**
(`state.go` freeze/reapply). The agent re-sends the full original each turn, so replaying
identical compacted bytes keeps the request prefix byte-stable and the cache warm — an
output never flips compacted→full→compacted (which would churn the cache).

### 1. `format` (Reformat, lossless)
Compact-re-encodes pretty-printed JSON tool outputs. Triggers on tool-role JSON ≥ 50 tok
that shrinks. **0 acts on SWE** (terminal/file text, not JSON). Latency ~41 ms total.

### 2. `dedup` (Offload)
Replaces a tool output byte-identical to an earlier one *in the same request* with a
pointer. Triggers on tool-role text ≥ 100 tok whose content-hash was seen earlier; the
pointer+marker must be strictly smaller.
> **Real example** (git diff re-sent): `181 → 21 tok` →
> `[identical to an earlier tool output] <<cg:3da56b20…>>`
Run: 7 acts, 1,120 cumulative / **160 unique** tok.

### 3. `failed_run` (Offload — auto-off on cached agents)
Collapses *earlier failed* test/build runs superseded by a later run (keeps passed runs +
the latest run verbatim). **On a cached agent it auto-disables new collapses** (`if
c.CacheAware { continue }`): a superseded run is old/already-cached, so collapsing it would
force a full-suffix cache-write for almost no saving (this was the dominant +cost in an
earlier design — 121 such transitions). Frozen collapses are still replayed for stability.
Run: **0 acts** (cache-aware), but still scans every run-like output (~6.7 s total — the
costliest *deterministic* detection).

### 4. `cmdfilter` (Offload)
Declarative DSL filters keyed on the shape of a command output's leading lines (e.g. `pytest`,
`make`, `gradle`, `terraform-plan`, `pulumi`): strip blank/`PASSED`/progress lines, cap length,
keep failures.
> **Real example** (pytest session): `1140 → 1068 tok` — passing/blank noise stripped,
> failures + warnings kept verbatim.
Run: 3 acts.

!!! note "The filter set has since tripled"
    This run had **3** filters matched on the output's *first* line only.
    [#42](https://github.com/rossoctl/context-guru/pull/42) took it to **24** filters, rewrote every
    selector to match output shape over six leading lines, and added the per-family `/stats` ledger;
    `pip-install` and `latex` have since brought the shipped set to **26**.
    The 3-act figure is not a ceiling for the current set — but nor is 26 filters a promise of 8×
    the savings: on the Terminal-Bench dump the four filters *predicted* to matter (`pulumi`,
    `terraform-plan`, `xcodebuild`, `gradle`) fired zero times, and `apt` + `gcc` carried ~73% of
    the savings instead. Re-measure rather than extrapolate.

### 5. `extract_llm` (Offload — the relevance-aware LLM pass)
A cheap **haiku**-class model writes a sandboxed **Starlark program** that trims *one*
tool output to what the agent needs next. It may delete or regex-rewrite, must preserve
ids/paths/errors verbatim, and may emit a one-line `SUMMARY` shown inline in the marker.

**When:** request ≥ 3000 tok; per-output floor **≥ 3000 tok** (`max(min_tokens, frac·window)`);
only NEW outputs in the uncached **tail** (`TailOnly`); ≤ **4 calls/request**, cadence every
request; skips already-expanded content (`MarkKeptVerbatim`). Reapply-first: a re-sent
output is replayed from the frozen result with **no model call**.

**How:** goal + KEEP-identifiers → `buildCodePrompt` → the model's program runs sandboxed
(json module + `re_*`, no imports/IO, 2 s limit) → validated (strictly smaller +
`IsContained` subsequence proof, unless rewrite mode) before splicing. **Reward-safe
skeletonization**: for a source file, keep imports / every signature / docstrings / any
KEEP- or goal-relevant line verbatim, and **keep the full body of any def that is short
(≤~15 lines), relevant, or adjacent to relevant code** — elide only *long unrelated*
bodies into `# … N lines elided (call context_guru_expand) …`. Per-output calls run in
**parallel** (a single-call batch was measured ~3× worse on tokens).

> **Real example — skeletonization** (sympy `normalforms.py` file read): `3928 → 2863 tok`
> — imports, docstring, and every `def` signature kept byte-identical; long unrelated
> bodies replaced with elision markers.
>
> **Real example — rewrite + SUMMARY** (a huge symbolic determinant): `4825 → 264 tok`,
> with the model's digest spliced next to the marker:
> `… [f(5) raw determinant (verify simplifies to 0); full expr preserved] <<cg:f824977c…>>`

Run: **31 model calls, 42 acts, 129,966 cumulative / ~18k unique tok, $0.31** haiku cost,
~167 s cumulative own-latency (the dominant latency contributor).

### 6. `extract` (Offload — deterministic, zero-latency)
No-LLM, conservative noise removal that runs every turn (byte-stable → cache-safe). First
`stripTerminalNoise` — **strip ANSI escapes and collapse carriage-return progress
redraws** (keep the final rendered line) — then collapse blank runs, progress-bar lines,
and consecutively-repeated blocks (k≤12), keeping every unique informative line.
> **Real example** (Django test run with repeated "Cloning test database…" lines):
> `409 → 121 tok` — duplicate lines collapsed to one; the result summary + failures kept.
Run: **245 acts, 34,293 cumulative / ~2.9k unique tok**, ~0.6 s total (near-zero).

### 7. `cacheinject` (Reformat)
Stamps an ephemeral `cache_control` breakpoint on the prefix boundary so the provider KV
cache hits across turns. No content change.

!!! danger "This row measured a suppressed component"
    On the run behind this page, `cacheinject`'s breakpoints **never reached the wire** —
    46 applied at the component level, 0 in the output body across 40 captured requests
    ([#32](https://github.com/rossoctl/context-guru/issues/32)). Its only possible targets
    are assistant turns carrying `tool_use`, which bifrost cannot round-trip, so the
    writeback layer discarded every mark.

    So the **97.8% cache-hit rate is not attributable to this component.** That rate is
    claude-code's own breakpoints, which it sets on every request and which the proxy
    forwarded untouched. Fixed in [#36](https://github.com/rossoctl/context-guru/pull/36); a
    re-run must re-measure this row rather than carry the number forward. In the meantime
    placement has been removed from every preset, so a fresh `codesmart` run has no
    `cacheinject` row at all — it has a `cachesplit` one.

---

## headroom (`headroom-ai` v0.32.1) — a `ContentRouter` of deterministic compressors

Runs as an HTTP proxy; a `ContentRouter` detects each content block's type (Magika +
regex) and dispatches to a type-specific compressor. **Live-zone-only** via
`frozen_message_count` (leading cache-anchored messages are never rewritten) + protection
windows (skip user messages, protect the 4 most-recent code blocks + all Read outputs +
error outputs). Ran in **`cache` mode** (compress only the live zone; `token` mode would
also rewrite the frozen prefix for ~25–35% more savings but busts the cache).
**All compressors are deterministic — no generative LLM** (`added_llm_cost $0`); the only
models are a *local* ONNX token-scorer (Kompress) and the Magika detector.

| compressor | what it does | run: events / tokens saved |
|---|---|---|
| **text** (Kompress) | ML-scored lossy prose compression (local ModernBERT scorer keeps top-value tokens) — the fallback for plain text/markdown | 140 / **18,417** |
| **code_aware** (AST) | tree-sitter AST compression: preserves imports/signatures/types/error-handlers, compresses function bodies, "output always parses" | 63 / **10,559** |
| **log** | collapses repeated/templated log lines, keeps ERROR/FAIL | 9 / 1,267 |
| **diff** | trims unified-diff context/noise hunks, keeps change lines (never lossy-chained — would break `git apply`) | 10 / 234 |
| **smart_crusher** (JSON) | structural compression of JSON arrays-of-dicts (column dedup/fold) | 3 / 162 |
| **tabular** | CSV/TSV/markdown tables via SmartCrusher | 1 / 26 |
| **search** | clusters grep/ripgrep `file:line:` matches (lossless) | 7 / — |

> **Real per-request examples** (`stats-hd-cache.json → recent_requests`):
> `29255 → 27978` (log), `26324 → 25473` (code_aware), `26100 → 25249` (text),
> `23356 → 22509` (mixed). Best single compression: **16,042 → 13,860 (13.6%)**.

**Why code_aware/text dominate and JSON is tiny:** coding tool-output is source code,
prose, and logs — not JSON arrays-of-dicts, so `smart_crusher` barely fires. **Note on the
headline number:** headroom reports `proxy_compression_saved = 675,564`, but ~660k of that
is `anthropic:tool_schema_compaction` (~825 tok/req of tool-*schema*, fired on nearly every
request), not tool-output content. The live-zone *content* compression is **30,665 tokens
across 233 events** → `content_savings_pct = 2.64%`.

**Reversibility (CCR)** stashes originals under `<<ccr:HASH>>` + a `headroom_retrieve`
tool — but its streaming handler re-indexes SSE content blocks, which **corrupts
claude-code's stream ("Content block not found")**, so it must run with `--no-ccr`. On
this run compression was therefore **one-way lossy** (0 retrievals). Run: **63.35 ms/req**
overhead, 0 failures.

---

## rtk (`rtk-ai/rtk` v0.43.0) — a shell-level `PreToolUse` hook

rtk is a single static Rust binary installed as a Claude Code **`PreToolUse` hook**. Before
the agent runs a Bash command it rewrites it to the rtk equivalent (`cat f`→`rtk read f`,
`pytest`→`rtk pytest`, `git status`→`rtk git status`, `grep`→`rtk grep`, …); rtk runs the
real command, compresses its output, and returns the compact form to the agent. **The
compression happens at the shell, before the output enters the transcript** — so unlike the
two proxies, rtk is not on the network path, adds **0 ms to the model request**, runs **no
model** ($0 tool cost), and is **cache-safe by construction** (the compressed output is what
gets cached from turn 1 — there is no cached prefix to mutate). rtk ships no tokenizer, so
its own savings are a `bytes/4` estimate of **bash output only**.

**Structural ceiling:** the hook fires only on the **Bash** tool. Claude Code's built-in
`Read` / `Grep` / `Glob` tools bypass it, so rtk never sees output the agent reads through
those — the reason a request-stream proxy that compacts *everything* (context-guru) can go
deeper.

Across the matched 50-task run rtk rewrote **637 bash commands** and removed **~338k
bash-output tokens (65.8%)**, concentrated in file reads and grep:

| rtk command | invocations | ~bash tokens removed | share | what it does |
|---|--:|--:|--:|---|
| `rtk read` (`cat`) | 39 | ~209,000 | 62% | keeps imports + signatures, elides bodies |
| `rtk grep` | 285 | ~80,400 | 24% | groups matches by file, truncates long lines |
| `rtk git` | 134 | ~20,800 | 6% | compact status/diff; stash → confirmation line |
| `rtk ls` | 44 | ~12,600 | 4% | tree with counts instead of one line per entry |
| `rtk pytest` | 57 | ~12,000 | 4% | failures only, passing collapsed to a count |
| `diff`/`pip`/`wc`/`find`/`curl` | 81 | ~3,100 | 1% | misc structural trims |

> **Real example — `cat` a source file → `rtk read -l aggressive`** (332 B → 164 B): imports
> and every signature kept, bodies elided to `// ... implementation`.
>
> **Real example — a failing `pytest` run → `rtk test`** (1,055 B → 195 B, ~81%): the 213
> passing lines dropped, the one `FAILED …` line + the `1 failed, 213 passed` summary kept.

**Reversibility** is a **tee file**: on a *failed* command rtk writes the full unfiltered
output to `~/.local/share/rtk/tee/…` and prints the path, so the agent can read the raw
output without re-running — a different model from the proxies' inline `<<cg:HASH>>` /
`<<ccr:HASH>>` markers (no marker in the transcript, no model-callable expand tool).

---

## Head-to-head

| axis | context-guru | headroom | rtk | winner |
|---|---|---|---|---|
| layer | request-stream proxy | request-stream proxy | shell `PreToolUse` hook | — |
| approach | hybrid (deterministic + haiku LLM) | fully deterministic | fully deterministic | — |
| scope | whole request (incl. `Read` file reads) | whole request | **Bash-tool output only** | proxies |
| **billed cost** (matched) | **$27.77** | $30.30 | $29.09 | **context-guru** |
| **cache-read tokens** | **84.5M** | 96.4M | 91.7M | **context-guru** |
| cache-write tokens | 1.847M | 1.839M | **1.835M** | ≈ tie (within 0.7%) |
| reward (solved/50) | **44** | 40 | 43 | **context-guru** |
| mean steps | **31.1** | 35.1 | 33.2 | **context-guru** |
| added latency / req | 117 ms | 63 ms | **0 ms** | **rtk** |
| tool's own LLM cost | $0.31 | **$0** | **$0** | rtk / headroom |
| content removed / req | 1.09% (whole req) | 2.64% (whole req) | 65.8% (*bash only*) | — (diff. denominators) |
| reversibility on streaming | **`expand` works** (SSE aggregation) | CCR off (corrupts claude-code SSE) | tee file on failure | context-guru |
| exceptions (of 50) | 0 | 0 | 0 | tie |

**Key nuance 1 — proxies:** headroom removes more *raw* content per request (2.64% vs 1.09%),
yet context-guru is **cheaper** with **lower cache-read**, because it **freezes each
compaction and replays it byte-identically every turn** (the reduction compounds across the
session) and its LLM skeletonizer targets the biggest file reads headroom leaves larger.

**Key nuance 2 — rtk:** a *deterministic shell filter with no model and zero request
latency* lands **2nd on cost** and **beats headroom on both cost and reward**. It wins by
attacking `cat`/`grep`/test output at the source (so the cache stores it pre-shrunk every
turn) — even though it never sees the built-in `Read`/`Grep`/`Glob` traffic. Its 66% cut of
bash output nets to −9% on the bill because bash output is only a slice of a ~98%-cached
agent's context.

**Cache-write is a four-way wash** (baseline 1.855M · context-guru 1.847M · headroom 1.839M ·
rtk 1.835M — within 1.1%): **none of the arms busts the cache**.

**Verdict.** context-guru wins the dollar-and-reward metrics (cost, cache-read, steps, most
tasks solved) and stays reversible on streaming. rtk is the efficiency surprise — nearly the
cheapest, reward-neutral, at $0 tool cost and 0 ms request latency, from the simplest design.
headroom is squeezed between them: a proxy that costs more than rtk while solving fewer tasks,
its only remaining edge (deterministic hot-path latency) matched by rtk at the shell.
