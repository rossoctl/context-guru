# context-guru improvement plan — deep analysis across SWE-bench + Terminal-Bench

Synthesis of a 6-way deep analysis (context-guru components, headroom, rtk, per-task empirics,
the change-log dumps, and cache_control mechanics measured against real captured Claude Code
request bodies). Goal: understand what helped / hurt on both benchmarks and lay out, with
examples, how to make context-guru **much** cheaper and higher-reward across *all* workloads —
not just these two — and where our durable edge is.

---

## 0. The one thing to internalise: what the bill is actually made of

On a modern Claude Code agent the request is **~99.95% cached** (measured: fresh/uncached input
is 0.05% of the TB bill). So "remove tokens from the request" is almost the wrong objective —
**context-guru's unique token removal is a rounding error: 0.024% of billed input on SWE, 0.127%
on TB.** What actually moves the dollar figure, in order:

1. **Agent steps.** `corr(Δsteps, Δcost) = +0.95` on *every* arm and *both* benchmarks. Each extra
   turn re-reads the whole accumulated prefix at $0.20/M and emits more output at $10/M. On SWE,
   context-guru's win was almost entirely a **−13.7% step reduction** that multiplied 165k removed
   tokens into **−18.3M cache-read tokens (110× leverage)**. Tokens are the lever *only* insofar as
   they change steps.
2. **Cache-write.** One cache-write costs **11.5 cache-reads** (`($2.50−$0.20)/$0.20`). Any mutation
   of already-cached content re-writes the whole suffix. This is the entire "TB regression" story
   for the proxies (see §1).
3. **Output tokens.** On TB, output is **47% of the bill** ($47 of $100) — *larger than cache-read*.
   The only way to cut it is shorter trajectories (fewer steps). On SWE, cache-read is 64% and
   output is minor. **The two benchmarks are different cost regimes and must be tuned separately.**
4. **Breakpoint placement.** `cache_control` is **metadata, not hashed content** — adding/moving a
   breakpoint is **free and lossless**, yet it decides whether a turn is a cheap read or a full
   re-write. This is the single largest *unused* lever (§3A).

Content compaction (our current focus) matters, but mostly as a *means* to (1) and (2), and it is
currently our **most expensive and least reliable** lever.

---

## 1. What happened, corrected — the results are not what the headline said

### 1a. The Terminal-Bench "regression" was largely a measurement artifact
Six TB tasks have **degenerate baseline trials**: the baseline agent aborted in 2–6 steps /
16–800 s (almost certainly n=24 CPU-contention early-aborts that were not flagged as timeouts),
while every compaction arm ran the task properly for 50–160 steps. Comparing a 16-second no-op to
a 2-hour genuine attempt is not a cost comparison. `extract-moves-from-video` alone is **$24.10 of
headroom's $24.65 nominal regression**, and it is a task where the baseline did 2 steps.

**Recomputed on the 83 clean tasks the story matches SWE:**

| arm | all-in $ | Δ vs baseline | steps |
|---|--:|--:|--:|
| baseline | 100.17 | — | 38.1 |
| context-guru | 90.34 | **−9.8%** | 34.9 (−3.1) |
| headroom | 84.19 | **−16.0%** | 34.1 |
| rtk | 106.59 | +6.4% | 39.7 |

Reproduced independently from the row files: baseline **$100.17**, context-guru **$87.47** model cost
(**−12.7%**) or **$90.34** including its own haiku cost (**−9.8%**), solving **56 vs 54** with
**2,899 vs 3,160** total steps (**−8.3%**). Note the published baseline is a *two-stage* merge
(`rows-off-final.json` + the 11-task 4x rerun); the intermediate file alone sums to $71.44 and does
not reproduce the published $100.81 — see REPRODUCE.md §7.

So: **both proxies save ~10–16% on TB too; only rtk genuinely regresses; and context-guru
*reduces* steps on clean TB, it does not raise them.** (Action item F0: re-run those 6 baselines and
regenerate the published TB docs — the current numbers are wrong.)

### 1b. Where compaction earns, by task size (the cleanest signal in the data)
Every arm **loses money on small tasks and makes it on large ones**:

| bench | tercile | base prompt-tok | context-guru | headroom | rtk |
|---|---|--:|--:|--:|--:|
| SWE | small | 0.72M | **+8.1%** | +19.8% | +19.1% |
| SWE | large | 4.21M | **−25.7%** | −18.9% | −27.2% |
| TB | small | 0.41M | **+52.1%** | +49.7% | +24.0% |
| TB | large | 6.30M | **−19.7%** | −20.9% | +0.5% |

The marker/overhead and (for us) the haiku call are fixed costs; below ~1M prompt tokens they
exceed the savings. **Gating compaction on conversation size is the cheapest large win and carries
zero reward risk** (§3B1).

### 1c. What helped reward, and the honest read on headroom
Reward is **neutral on every arm except headroom-on-TB (+8, p≈0.096, n=1)** — suggestive, not
established. Examined per task, headroom's 5 unique hard-task solves decompose mostly into
**persistence** (it kept trying longer) and **baseline artifacts**; only `path-tracing-reverse` is a
clean "compaction freed context so the agent finished in time" case. The transferable lesson is a
**posture, not a technique**: headroom compressed only 2.64% of content, touched only the newest
turn, never dropped a whole message, excluded Read/Grep/Glob/Write/Edit, and freed ~825 tok/req of
tool-schema overhead. That is *"baseline plus free headroom,"* not *"better compression."*

### 1d. Where context-guru specifically loses today
- **Small tasks and short/exact-output categories.** cg's TB losses concentrate in `security`
  (+121%, 0 reward gain) and `debugging` (+51%) — short, exact-output-sensitive families.
- **`extract_llm` is 8× underwater.** It saved 197,548 unique tokens (~$0.04 at cache-read) for
  **$3.26 + 26 minutes of blocking wall time**. Its per-call economics are *identical* on both
  benchmarks (~$0.0166 / 1k unique tokens); TB just fires it 3.7× more often because 7% of TB
  requests clear the 3000-token floor vs 0% on SWE. **93% of its realised savings come from the
  replay result-cache, not from the model** — only 0.24% of tool outputs ever reached haiku.
- **The cache-write tax.** cg's write/read ratio is 2.82% on TB vs baseline 1.86% (+52%), from
  mutating content inside the cached prefix (§2, §3A).
- ~~**`context_guru_expand` is referenced 1,496× and callable 0×.**~~ **REFUTED — see §B2.** The
  tool *is* registered, the SSE loop *is* armed, and a live agent restored 3,372 tokens through the
  streaming path. The 4.8M was a cumulative re-count; unique is 234,119 tokens (21× smaller). The
  genuine defect in this area was a latency tautology that buffered every SSE response.

---

## 2. How headroom and rtk handle the cache (you asked explicitly)

**headroom** — *rewrites the newest turn, and pays for it.* On the path that actually ran (Python,
not the in-progress Rust rewrite) it **ignores incoming `cache_control` as a boundary** and derives
its frozen floor from observed provider usage, then in `cache` mode freezes "everything but the last
user turn." Because it compresses the live zone, it forwards bytes the client never sent and must
**replay** them next turn; every replay-state failure (600 s tracker TTL, 32-lineage cap under
subagent fan-out, any thinking/tool-id divergence) is a **full prefix re-write**. It also
**collapses Claude Code's multi-breakpoint ladder to a single tail marker**, removing the durable
older anchor — so a miss is total, not partial. Net: **its tripled cache-write is a tripled miss
rate.** The lesson is a warning: *do not rewrite the live zone; do not collapse the breakpoint
ladder.*

**rtk** — *cache-safe by construction, but a weak one-sided lever.* It compresses Bash output at the
shell **before** it enters the transcript, so there is exactly one immutable version of every tool
result from turn 1 — no replay machinery, no idempotence requirement, $0, 0 ms. But it only ever
shrinks the *new tool-result write*; everything already in the transcript is re-read forever. Its
guard is **per-command (byte-level)** and structurally blind to the global bill, so on long horizons
it happily takes lossy compressions that trigger an extra step, and one extra step (a full-prefix
re-read) wipes out a dozen compressed commands. Its regression is **step-driven, not cache-driven.**
The lesson: *operate "at the source" and freeze on first sight (cache-safe), but never let a
byte-local guard authorize a step-costly loss.*

**context-guru today** — *the right idea, three bugs.* It reads the breakpoint as a **boolean** and
throws away the position; it infers the cache boundary from **message count** (agreed with the true
last-breakpoint index on **0/73** TB turns); it **fails open** on a store miss (mutates the whole
prefix); and its freeze-store **expires frozen decisions mid-task**. Fixing these (§3A) is worth more
than any new compressor.

---

## 3. The plan — prioritized, with mechanism, economic condition, and example

### A. Cache-control exploitation — biggest, free, risk-free (your core question)

**A1. Sticky-anchor breakpoint. ★ highest single lever, measured −23.2% on TB, ±0% on SWE.**
*Mechanism:* Claude Code leaves its 4th breakpoint slot empty and pins BP3 to the **last block every
turn**. When a turn adds ≥20 blocks (parallel tool calls — **28% of TB turns**, one message had 36
`tool_use` blocks), BP3's 20-block lookback can't reach the previous entry and the whole history is
re-written. Fix: record the previous turn's breakpoint block (by **content hash**, not index) and
stamp the free 4th breakpoint there, giving the lookback a reachable anchor. Because `cache_control`
is metadata, this touches no cached bytes.
*Condition:* free; pays whenever `Pr(turn adds ≥20 blocks) > 0`.
*Example (real TB traffic, simulator):* write tokens **1.195M → 0.669M**, cost **$5.22 → $4.01
(−23.2%)**; on the 35-turn chain alone −42%. SWE unaffected (median growth 2 blocks).

**A2. Fix the freeze-store lifetime — this *is* the TB cache-write regression.**
*Mechanism:* `store/store.go` `Get` refreshes LRU recency but **not** `e.expires`, and the default
TTL is 1800 s. So a frozen compaction dies ~69 turns after it was written regardless of how often
it's replayed; on expiry it reverts full→compacted and re-writes the suffix. Three fixes: (a) sliding
TTL — refresh `expires` on `Get`; (b) raise the default well past a task's wall time (TB averaged
1975 s > 1800 s); (c) **never revert on a store miss** — once compacted, reverting is the expensive
move; keep replaying or fail closed.
*Condition:* a revert costs `W·$2.30/M` and buys nothing — always fix.
*Example:* eliminates ~15 mid-depth reverts ≈ **2.5M cache-write tokens ≈ $6.3** on TB — essentially
the entire 4.0M→6.5M gap — with no loss of compaction.

**A3. Key the tail gate on the *real* breakpoint index, and fail *closed*.**
*Mechanism:* extend `hasCacheBreakpoint` (`apply/apply.go:254`) to return the last breakpoint's
message index; set `MaxCachedIdx = min(markerFloor, growthFloor)`. Invert `TailOnly`
(`components/component.go:129`): on an unknown boundary with `CacheAware`, return **false** (mutate
nothing) instead of true.
*Condition:* the current fail-open branch risks up to **$0.63/turn** (250k-token suffix rewrite) to
save `S·$0.20/M`. Sign is unambiguous.

**A4. Promote the two static breakpoints to `ttl:"1h"`.**
*Mechanism:* rewrite `system[1]/system[2]` `cache_control` to `{"type":"ephemeral","ttl":"1h"}`
(longer-TTL must precede shorter — automatically satisfied). Protects the ~32k-token tools+system
prefix from re-creation whenever the agent stalls >5 min (test suites, training runs).
*Condition:* worth it when `Pr(idle gap > 5 min) > ~1%` — near-certain on agentic workloads.

**A5. Retire `cacheinject` in its current form.** It stamps the 4th breakpoint one message behind
BP3, *inside the same 20-block window* → **provably inert** (simulated $5.22 → $5.22) while consuming
the slot A1 needs. Replace it with the sticky-anchor component (they are mutually exclusive at the
4-breakpoint ceiling).

**A6. Do we ever want to harm the cache on purpose? Essentially no.** Break-even for deliberately
mutating cached content: `N > 11.5·(W/S)` (turns-remaining vs suffix/saving ratio). Compacting a
typical mid-depth output needs **N > 434 turns**; only removing a *large fraction of a near-tail
region* (`S ≈ 0.55·W`) clears it at `N > 21`. And tail-only + freeze-replay already propagates a
compaction into the prefix on later turns, so it captures the same savings **without** a bust:
simulated tail-only −44.2% equals compact-everything −44.2%. **Rule: never mutate at depth; there is
no realistic case for deliberate cache-busting on this traffic.** Stacking A1+A2+tail-only reached
**−57%** in simulation on the exact traffic where cg currently measures +1.7%.

### B. Step & output reduction — the real cost/reward driver (corr 0.95)

**B1. Gate compaction on context size / turn count.** Below ~1M prompt tokens compaction is pure
loss (+8% SWE-small, +52% TB-small). A `min_conversation_tokens` / `min_turns` gate before any
offloader recovers most of the small-task regression at zero reward risk. Above the gate, escalate
aggressiveness with size.

**B2. ~~Register `context_guru_expand` on the streaming path~~ — REFUTED; the real bug was a
latency tautology.** ([#26](https://github.com/rossoctl/context-guru/issues/26) /
[PR #33](https://github.com/rossoctl/context-guru/pull/33))

Both halves of the original claim were wrong:

- **The tool IS registered and the SSE loop IS armed.** `expand.Inject` fires on real streaming
  requests (24 → 25 tools, idempotent on the next turn), and `proxy.serve` buffers + aggregates SSE
  whenever markers are present. There is no streaming short-circuit.
- **A real agent DID invoke restoration through the streaming path.** Live SWE run: `bounces=1`,
  **3,372 tokens restored**. `RecordExpand` has exactly one reachable call site (`proxy.go:488`),
  inside the continuation loop, only after `expand.ResponseCalls` finds a model-issued expand call
  *and* `expand.Resolve` succeeds. All traffic was SSE.
- **The 4.8M figure was cumulative**, re-counting each compaction on every turn history is re-sent.
  Unique: **234,119 tokens behind 103 distinct markers** (TB) and **15,457 behind 29** (SWE) —
  **21× and 8× smaller**. Restoration demand is genuinely low (~1 recoverable compaction per 3
  sessions), not blocked.

**The actual bug, and it is a real one.** `hasMarkers` tested the raw body for `\u003ccg:`, and the
*injected tool description itself* contains that sequence (`toolDesc` mentions `<<cg:HASH>>`; Go's
`encoding/json` HTML-escapes `<`). So from the moment `Inject` ran, the check was a **tautology** and
**every SSE response was fully buffered** — defeating the documented zero-added-latency fast path.

Fixed by scoping the check to `messages` + `system` (`expand.HasMarkersInMessages`). Measured:
marker-free TTFB **1007 ms → 43 ms (23×)**, marker-bearing correctly unchanged. On live traffic
buffering fell from an implied **100% → 27.3%**, and the transitions confirm the intended semantics —
17 requests streamed before the first offload, buffering began exactly as `saved` went non-zero, and
a fresh session resumed the fast path.

**Lesson for this document:** a "component never fires" finding must be checked against the raw wire
bytes before it becomes a plan item. Two of this plan's entries (B2 here, C1 in §C) were premise
errors of exactly this kind, both caused by trusting a derived artifact — the change-log dumps — over
the request captures.

**B3. Make `extract_llm` cost-aware and get it off the hot path.**
(a) **Prompt-cache its 852-token fixed preamble** (`cheapmodel/anthropic.go:45` — put the invariant
rules in a cached `system` block): ~90% of its input tokens become cache-reads.
(b) **Global content-hash-keyed result cache** (drop the session prefix in `resultKey`): 82/103
unique contents recurred across sessions and are re-derived for nothing.
(c) **Economic gate:** only call when `expected_saving_$ > haiku_call_$ (~$0.012)` — on a cached
backend this correctly suppresses ~all TB calls; on a non-caching backend it lets them through.
(d) **Async:** compact in the background and let the next turn's replay cache pick it up — 93% of
value is already replay, so this removes 450 ms/req and $3.26 with negligible savings loss.

**B4. Cut expand round-trips with a head-peek in the marker.** `mask` already exposes
`keep_head_chars`; add the same ~15-token peek to `extract`/`dedup`/`extract_llm` markers so the
model can decide whether an expand is worth a turn instead of bouncing blindly.

### C. Cross-turn dedup — ~~the biggest untapped token lever~~ **REFUTED by measurement**

**C1 was wrong. The re-send factor is real; the interpretation was not.**
**`xdedup` is unbuildable, and would be harmful if built.** Measured on the raw request
captures (1,325 requests / 51 sessions across `capture-tb`, `capture-swe`, `capture-swebench`
— *not* the change-log dumps, which only record messages a component already acted on and so
cannot answer this question):

- **232 of 232** re-sent large outputs live at exactly **one stable message index** for their
  entire session. Independently re-checked: of **77** distinct >4 KB tool outputs tracked per
  session, **0 ever appeared at a second index**.
- **1124/1124 = 100%** of consecutive turn pairs have the entire previous turn as a
  byte-identical prefix (comparing *content*, ignoring `cache_control` annotations).
- **Zero** compaction/rewind events on swebench — the message count never shrank.

So a "re-read" is not a re-read. **The agent appends.** Turn N's copy of a file sits at the
index it has always occupied, inside the byte-identical cached prefix, billing at the
**cache-read** rate. The load-bearing claim — "content genuinely re-sent as new bytes" — is
false.

`xdedup` could only act where the first copy is absent *and* the repeat is in the mutable tail.
Across 1,325 requests that intersection is **empty**: 5,314 re-send events (5.46M tokens) sit in
the cached prefix, where `TailOnly` correctly refuses — and rewriting them is precisely the
full → referenced flip that converts cache-reads into cache-writes at **11.5×**. The genuinely
duplicated cases are already caught by `dedup` (23 acts on the TB run, **0 tokens missed** above
the size gate). Implemented with its guards intact, the component is a permanent no-op.

**Where the token mass actually is:** those 5.46M tokens are real but **already cheap**. The
lever is not removing them, it is keeping the prefix stable so they *stay* cache-reads. The
measured cross-session lever there is the volatile-tail split (`cachesplit`); breakpoint
*placement* tuning measured ≈0%, and a proposed cross-session "prefix repair" for Claude Code's
billing header was later refuted — cross-session reuse already worked, so there was nothing to
repair.

**One caveat left open:** `capture-tb` showed 19 turns where the message count shrank.
Compaction is the one regime that could make cross-turn dedup viable, since it removes the first
copy while later re-reads land in the tail. Even there 0 actionable cases were measured, but the
premise is worth re-testing if aggressive compaction is enabled.

Tracked as [#27](https://github.com/rossoctl/context-guru/issues/27), closed as
measurement-refuted. C2 and C3 below are unaffected and still stand.

**C2. Recurrence-aware floor.** Track `seenCount[hash]`; effective floor `= floor / max(1,seenCount)`.
A 298-token output re-sent 40× is worth 12k tokens but is invisible to the 3000-token floor today —
this unlocks the **64% of tool-token mass currently below the floor** via the free deterministic path.

**C3. Wire `freeze`/`reapplyFrozen` into *every* offloader.** Today only `mask` and `failed_run` use
it (both 0 acts on TB); `extract`, `dedup`, `cmdfilter`, `extract_llm` do not — which is why the dump
shows **101 compacted→full→compacted flip turns** and 15 non-byte-stable replays. This is the
documented cache-safety invariant, simply not implemented for 5 of 7 components.

### D. Steal from headroom — the lossless layers we lack

**D1. Tool-schema compaction. ★ highest-ROI steal (~825 tok/req, ~30 lines, lossless, cache-safe).**
Recursively drop annotation keys (`$schema,$id,title,examples,readOnly,…`), whitespace-collapse
descriptions, memoize on `sha256(tools)`, never-worse guard. That is ~3× headroom's entire *content*
savings on SWE. Splice point already exists (`expand/inject.go:57`); compact before injecting the
expand tool and apply the same transform to it. Instrument it as a first-class metric.
**Do not** adopt headroom's system-prompt compaction (lossy, unmemoized, byte-unstable).

**D2. Structural log / diff / search compressors** (TB tool output is dominated by these). Lift two
invariants verbatim: **verbatim under 50 lines** (a hard floor that kills the "compressed something
tiny and lost the one line that mattered" class) and **stack-frame collapse** (keep 3 head + 5 app
frames, collapse runtime runs to a marker pinned above the drop threshold). For diffs: **never drop a
`+`/`-` line** — only whole hunks.

**D3. `TextCrusher`-style extractive prose selection** (not the ONNX scorer first). Sentence
segmentation + `recency + 2·BM25 + 1.5·salience`, near-dup rejection, **emit kept segments verbatim in
original order** — pure selection, no model, no 209 ms, no newline destruction. Add the ModernBERT
scorer later only behind a size gate with their defence-in-depth (canary, wall-clock deadline,
verbatim-tail fallback).

**D4. Kneedle adaptive-K + lossless-first in `smartcrush`** (replaces the fixed keep-first-3/last-2
that our own code flags as unimplemented): re-render CSV/markdown-KV losslessly, adopt only if ≥15%
shrink, *then* consider lossy.

**D5. Symbol-importance + query-boost in `skeleton`.** Rank symbols by `ref_count + is_public +
0.5·fan_out (+3.0 if the goal names the symbol)`, proportional per-symbol line budget, re-parse to
verify — far better reward preservation than uniform body elision.

**D6. In-process cache-miss telemetry.** We have none — accounting is offline. Classify every turn as
`hit / ttl_expiry / prefix_change / cold_start` by comparing idle gap to the 300 s TTL and checking
prefix stability. We are flying blind on our single largest cost line; this is how headroom *knew*
"prefix_change = 100% of its misses."

### E. Steal from rtk — cache-safe structural per-command compressors

**E1. Pair each tool output with its originating command.** Walk back from a tool message via
`ToolCallID` to the assistant `ToolCall` and read `arguments.command` — the literal shell string,
rtk's dispatch key. This beats `cmdfilter`'s "match the output's first line" (which misses because
pytest starts with a platform banner) and, via `match_tool: Read|Grep|Glob`, lets us **cover
Claude Code's built-in tools — exactly rtk's structural blind spot.**

**E2. Port the near-lossless per-command compressors, refuse the lossy ones.** rtk's SWE win came
from near-lossless structure (grep grouping, `git status` porcelain, all-green pytest→one line, the
63 pre-tested `*.toml` filters — directly translatable to our DSL with their inline tests); its TB
loss came from high-loss compressors (aggressive body elision, pytest failure-gutting to 3×100
chars, silent `ls`/`git log` semantic rewrites). Port the first class; **never** the second. Keep
the real openable path (not rtk's `compact_path`), and for pytest keep the **full first traceback**,
compacting only the 2nd..Nth failure.

**E3. Two invariants from rtk worth adopting verbatim.** (i) **"Never emit an unrecoverable
elision"** — if a filter is lossy and no resolvable marker can be written, **skip the filter** (today
`cmdfilter` degrades to dropping content with no way back). (ii) **Re-runnable hints** where the
source still exists (`re-run with: git diff --no-compact`) — no store entry, no TTL, never dangles;
composes with E1 since we know the command. Plus **partial/tail restore** (`expand(id, from_line=N)`)
so recovering is cheaper than re-running.

**E4. One global "cap class" knob + a step-aware/cache-aware savings guard.** Centralise all filter
limits into `CAP_ERRORS/WARNINGS/LIST/INVENTORY` so a single per-preset dial moves every deterministic
compressor from aggressive→conservative. And make our own metric **cache-aware**: a component that
shrinks bytes but adds a turn (or a cache-write) must score **negative** — that single accounting
change would have caught rtk's TB regression before it shipped, and stops us optimising the raw byte
ratio (which overcounts 22–42×).

### F. Hygiene / methodology

**F-1. An aggregate moving in the predicted direction is not evidence that the predicted
mechanism operated.** Verify the mechanism fired *before* believing the outcome.

**Eight** premises in this effort were wrong, and nearly all failed the same way — a derived
artifact was trusted over the raw request stream, or an outcome was credited to a mechanism
nobody checked had run:

| premise | what was believed | what was true |
|---|---|---|
| §C1 `xdedup` | a 39.8× re-send factor meant tokens re-sent as new bytes | 232/232 large outputs sit at **one stable index**; the agent *appends*. Those tokens are cached-prefix reads, already cheap. Component would have had **zero** legal opportunity to act |
| §B2 expand tool | "never registered on the streaming path", 4.8M tokens stranded | tool IS registered, loop IS armed, a live agent restored 3,372 tokens. 4.8M was a **cumulative re-count**; unique is 234k (21× smaller). Real bug was a latency tautology |
| `prefixpin` | early messages mutate in place, worth ~31% of input cost | **0 mutations in ~6,500 comparisons** on claude-code. The 52% churn first measured was concurrent sessions sharing a first message, diffed against each other |
| §31 async cache-write | −45%/−39% cache-write proved the tail-protection worked | the protection **only stripped context-guru's own breakpoints**, never the agent's — so it did nothing on the primary workload. Lower cache-write came from writing *fewer breakpoints*, not from protecting the tail |
| §A5 `cacheinject` "provably inert" | placement has no headroom; the component does nothing | it applied **46 breakpoints and forwarded 0** — the writeback layer discarded every one. Inert for a reason nobody had checked. Two benchmark studies concluded things about placement while measuring a suppressed component |
| §A5 follow-on: placement is *harmful* | +61.9% cache-write/step once the marks reached the wire | **0 of 106 marks land above** the agent's own breakpoint, so the proposed mechanism (shortening the readable prefix) is ruled out. `acted=0` in that arm is a *tautology* of the arm's design, not proof the delta was placement |
| `cachesplit` on Terminal-Bench | the volatile-tail split carries a −34.1% win | TB runs the Agent **SDK**, which never appends the git/env snapshot the CLI does. All 73 captured TB requests: 3 system blocks, **zero** volatile-tail markers. **Zero legal opportunity** — the same shape as `xdedup` |
| the split on Bedrock Converse | `Changed: true`, `cachesplit` active in `/stats` | `cachePoint` is its own array entry *after* the block, so the split inserts the volatile half **before** it and the breakpoint still covers the churn. It reports success and achieves nothing |

Most of these produced a number that pointed the *right* way for the *wrong* reason, which is
why they survived review. Two were mine as orchestrator, both from accepting a number without
checking the mechanism — and one of those was an *unfavourable* number, which is the tell that
the bias is not optimism but incuriosity. **Skepticism applied only to good news is not
skepticism.** The countermeasures, in order of value:

1. **Group request lineages by append-only prefix match**, never by a hash of `messages[0]` — a
   benchmark harness runs concurrent sessions whose first message is byte-identical.
2. **Distinguish cumulative from unique on every token figure.** The overcount is 8–42×, so a
   cumulative number is off by an order of magnitude, not a rounding error.
3. **Instrument "did this component act?" separately from "did the metric improve?"** and require
   both before claiming a mechanism. `acted=0` beside a favourable delta is the tell.
4. **A trial where the baseline aborts is not a measurement** — exclude it, don't average it. Six
   such trials inverted the sign of the entire TB cost conclusion (§1a).
5. **Read the raw wire bytes.** The change-log dumps only record messages a component *already
   acted on*, so they structurally cannot answer "was this ever sent?"
6. **A component reporting that it acted is not evidence it acted usefully.** `Changed: true` and
   a non-zero `acted` counter both survive a mechanism that achieves nothing — see the Converse
   split. Assert the *effect* (did the breakpoint stop covering the churn?), not the activity.
7. **Check that a favourable metric had the opportunity to be caused by your change.** Three
   arms credited components that could not fire on that workload at all. Before attributing, ask
   what counter would be non-zero if the mechanism ran, and confirm it is.
8. **Verify the verifier.** Two "defects" in this effort were bugs in the checking script (a
   regex scraping the wrong table column; a stale binary one commit behind). A check that
   over-matches manufactures exactly the findings that waste a review cycle.
9. **A sum over heterogeneous tasks can be one task.** An interim TB delta read −40.2%; a single
   trial carried half of it, and dropping that one task gave −19.2%. Report the **median
   per-task ratio** and a **leave-one-out** on the top contributors beside any aggregate.

- **F0. Re-run the 6 degenerate TB baselines** and regenerate the comparison/baseline docs (§1a).
- **F1. Report `saved_tokens_unique` and cache-aware $, never raw cumulative byte ratio** (overcounts
  22–42×; unusable for tuning).
- **F2. Kill dead components** — *revised twice*: `failed_run` (0 acts, burns 28.8 s scanning) — hoist
  the `CacheAware` check so the regexes never run. **`cacheinject` is no longer in this list**, and the
  story behind it is the clearest example of F-1 in the whole document:
    1. It read as "0 acts, inert", which is *expected* for a Reformat that removes no tokens — so the
       reading was accepted.
    2. It was in fact discarding **every** breakpoint before the wire (46 applied, 0 forwarded): the
       writeback layer dropped them because its only targets are `tool_use` messages that bifrost
       cannot round-trip. Two full benchmark studies drew conclusions about placement while measuring
       a component whose output never left the process. Fixed in
       [#36](https://github.com/rossoctl/context-guru/pull/36).
    3. The first live measurement then looked *harmful* — +61.9% cache-write per step — and that was
       accepted too, by me, until review showed the proposed mechanism is **ruled out**: 0 of 106
       marks land above the agent's own breakpoint, and the arm's `acted=0` is a tautology of its
       design rather than proof the delta was placement.
  **Net position: `cacheinject` is removed from all nine presets** (#36) — not because it is proven
  harmful, but because there is a live cost signal with no explanation and no demonstrated benefit,
  which is not a defensible default. A `cachesplit` marker component now carries the volatile-tail
  split, which *does* have measured savings on CLI traffic. Whether placement helps at all remains
  **genuinely unanswered** and needs a properly-powered study, not another n=1.
- **F3. Tune per regime:** SWE = cache-read regime (optimise steps; note cross-turn dedup is refuted, §C); TB = output
  regime (optimise trajectory length). Same knobs, different settings, selected by measured context
  size.

---

## 4. Our edge — how context-guru wins on *every* workload

The competitors each own one idea and pay for it elsewhere: **rtk** is cache-safe-by-construction but
a weak one-sided lever with a byte-local guard that costs steps; **headroom** has the best lossless
layers and prose scorer but rewrites the live zone and collapses the breakpoint ladder, tripling
cache-write. context-guru is the only one positioned to hold **all** of the good ideas at once:

1. **Own breakpoint placement** (A1/A4) — a free −23% nobody else exploits; we already sit on the
   request and can read the real breakpoints.
2. **Be strictly cache-safe** (A2/A3/C3) — freeze-and-replay, tail-only, fail-closed: never rewrite
   the live zone (headroom's mistake), never expire a frozen decision.
3. **Compact *more* than headroom without the penalty** — add its lossless layers (D1–D5) *and* keep
   `mask`'s aggressive-but-reward-neutral offload, because our freeze-replay makes aggression safe.
4. **Unlock the sub-floor token mass** (C2) — a recurrence-aware floor reaches the 64% of tool-token
   mass currently invisible to the 3000-token threshold, LLM-free. (C1/`xdedup` is refuted — the
   re-sends it targeted are cached-prefix reads, already cheap; see §C.)
5. **Cover the built-in tools** (E1) — rtk's structural ceiling, our free extension.
6. **Reduce steps, and account in the currency of the bill** (B, E4) — the only thing correlated 0.95
   with cost, and the path to beating headroom on reward: register the expand tool (stop the re-work),
   gate small tasks, free context so long tasks finish within the timeout.

**Plausible stacked outcome:** sticky-anchor (−23%) + freeze-fix (recovers the ~$6 cache-write gap) +
tail-only (−44%) compose to **−57%** in simulation, before adding tool-schema compaction, the
recurrence-aware floor, and the step-reduction levers — i.e. a path to **substantially beyond headroom's
−16%**, while being strictly cache-safe and reward-neutral-to-positive. The prerequisites are the
three cache bugs (A2/A3) and the SSE buffering fix (B2, shipped); everything else compounds on top.
