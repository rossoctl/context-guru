# `coref` implementation status

What exists in the tree, what is deliberately inert, and what has to happen next. Split out of
[the proposal](coref-compaction.md), which is the design argument and should stay readable as one —
this is the part that goes stale with every commit.

**Related:** [the proposal](coref-compaction.md) · [component reference](../components/coref.md) ·
[cheat sheet](../reference/coref-glossary.md) · [measured density](../results/coref-density.md)


Two pieces exist, and the split between them is the point: the **mechanism** is a matter of getting the
definition of a reference right, which a known-answer fixture can settle; the **thresholds** are a
matter of what real traffic looks like, which only §7's pass can settle. So the first is built and the
second is not, and the component is configured to be inert on anything that depends on the second.

**Built.**

- [`internal/coref`](https://github.com/rossoctl/context-guru/blob/main/internal/coref/coref.go) — the Tier-1 index: identifier
  tokenizer, novel-token (echo) exclusion, boilerplate exclusion, sibling exclusion, reference count,
  recency from the head, consume lag, used-fraction, and the open/closed/unreferenced predicate. No
  bifrost, no components, no tokenizer dependency, so it is a pure function of a flattened message list
  — deliberately, because it must stay interchangeable with `coref.py`'s definition. The Go fixture is
  the twin of `coref_fixture.py` down to the four known answers **and the negative control**: with the
  echo guard disabled the `src/config.py` read flips out of `unreferenced` and measured cuttable mass
  falls, and the test fails if it does *not* flip — the control is asserted, not just run once.
- [`components/offload/coref.go`](https://github.com/rossoctl/context-guru/blob/main/components/offload/coref.go) — the Offload
  component, with each of §5's constraints as a tested behaviour rather than a comment: latched
  decisions replayed byte-for-byte even when fresh evidence would reclassify the span (constraint 1,
  and `repairLostFreeze` deliberately not consulted), keep→cut only (2), prefix mutation on purpose
  under a per-session `rewrite_budget` (3), `freeze`/`reapplyFrozen` wired from the start (4),
  `<<cg:HASH>>` + stash + kept-verbatim (5), and side-effect-free planning so a batch that fails a gate
  leaves the request byte-identical (6).
- §4's arithmetic as an actual gate, not a note: `min_batch_frac` for batching, and `break_even`
  applying `S × T > 11.5 × W` with `T` estimated from the transcript's observed growth rate and `W`
  bounded to the *cached* span (content past the cache boundary would be written this turn regardless).
  The counter-intuitive consequence from §6 is what the test pins: at the window edge `T ≈ 0` and the
  pass correctly declines.

**Deliberately not built, and why the "measure first" rule in §7 is not being broken.** §7 says nothing
should be built before the substrate is measured. What that rule protects against is *calibrating* a
component against numbers nobody has — so the implementation is scoped to the part that has no
calibration in it. `cut_unreferenced` needs no threshold: "no later turn used anything this output
introduced" is a fact about the transcript. `cut_closed` needs two (`closed_dist`, `open_reps`), which
are precisely what §7 produces, so it defaults to **off** and the shipped values are placeholders
carried over from `coref.py`'s defaults for comparability, not recommendations. `coref` is in **no
preset** for the same reason.

**Also not built, and deliberately: the deferral gate.** `min_batch_frac` implements the token
argument and is a poor proxy for the deferral argument — it cannot ask whether this cut is the
*decisive* one. The design, the measured numbers (a bar high enough to avoid paying two cache-writes
is 20–25% of the request; Tier-1 finds 4–10%) and a three-step order of attack are in
[the deferral gate](coref-compaction.md#the-deferral-gate-designed-unquantified). Step one is to
measure whether the prize is reachable at all, using `modes.Tracker`'s existing reset detection —
which needs nothing new and may make the rest unnecessary. Nothing else there should be built first.

Also not built: the Tier-2 LLM escalation ([open questions](coref-compaction.md#9-open-questions)), and the incremental per-session reference index. The
index is currently recomputed per firing turn — acceptable because the trigger makes firings rare, but
it is the latency question in the proposal's [open questions](coref-compaction.md#9-open-questions) and it is unmeasured.

**Measured, on three corpora.** The pass has run — on **Claude Code transcripts, UltraHorizon runs and
LOCA-bench trajectories**, none of which is the eval-box capture set (unreachable). Full write-up and
caveats: [co-reference density](../results/coref-density.md).

The single most useful result is that the three corpora **disagree by a factor of three**, so
`unreferenced` mass is a property of the workload rather than a constant:

| | Claude Code (interactive) | UltraHorizon | LOCA-bench |
|---|---|---|---|
| `unreferenced` | 23% | 78% | 95% |
| `closed` | 15% | 8% | **0%** |
| `open` | 60% | 13% | 4% |
| …restricted to outputs with ≥20 later turns | 21% | 70% | 70% |

Interactive work on a coherent codebase keeps returning to the same files and errors; benchmark tasks
survey, extract, and move on. The last row bounds the obvious bias (an output near the end has no later
turns that *could* reference it) and the ordering survives it: **benchmark traffic carries ~3.3× the
unreferenced mass of interactive traffic.** LOCA's 0% `closed` is also §8's own prediction landing — it
argued LOCA would be a Tier-2/3 stress test where references arrive transformed past what a substring
match can see, and an exact matcher finds not one output in 166 that was referenced once or twice and
then left alone.

Four things it settles, and one it overturns:

- **`cut_unreferenced` is justified as the default** — 21% of mass on interactive traffic and ~70% on
  benchmark traffic, with no calibrated threshold and no model call. Decision rule one from §7 is
  answered yes on every corpus.
- **A reference consumes a median 18.7% of what its output introduced** (11.5% on UltraHorizon).
  Hypothesis A — "took one value, does not need the rest" — is confirmed rather than assumed.
- **Tier-2 leakage measures ~2% of model turns.** Unpacking that, because it is a proxy and not a
  direct measurement: Tier 2 is a reference that arrived *transformed* (the model summed the rows or
  converted the units), so by definition no substring match can find it. What can be counted instead is
  a **symptom** — a model turn that states a numeric value appearing nowhere in any earlier message. If
  the model says "3 seconds" and `3` is nowhere upstream, it computed that number from something, and
  that something was almost certainly a tool output. On the interactive corpus 2% of turns look like
  that, which says the deterministic ceiling is not badly compromised and a zero-LLM first version is
  viable.

  Three caveats. It is a *lower* bound even for Tier 2: only numeric transformations leave this trace, so
  a reworded finding is invisible to it. Tightening the identifier rules (see the results write-up) also
  blinded it further — bare numbers now need 5+ digits, and most computed values are small — so its
  **0% on LOCA means "none among tokens the tokenizer still accepts", not "none"**.

  And the third is a scope point worth keeping straight: **this proxy is Tier-2 only, and Tier 3 has no
  measurement at all** — not a degraded one, none. A semantic reference ("as I noted earlier", "per the
  schema") carries no shared token *and* no novel numeric, so nothing here can see it, by design rather
  than by regression. So on a corpus with 0% `closed` and 40% `opaque`, the honest reading is that
  **Tier-2 and Tier-3 references there are both common and unmeasured**, for different reasons: Tier 2
  has a detector that is nearly blind, Tier 3 has none. Tier 2 needs its own detector; Tier 3 needs a
  model call, which is why it sits in open questions rather than in a measurement.
- **Break-even is workload-dependent, and better on benchmarks than on long interactive sessions**:
  median required `T` is 95 turns for Claude Code (15/30 sessions clear it) against 17 for UltraHorizon
  (7/10) and 14 for LOCA (4/9) — the cut is a far larger share of a smaller transcript. §4's arithmetic
  holds everywhere; batching moves break-even from unreachable to *comfortable on benchmarks* and
  *marginal on long interactive sessions*, so decision rule three still applies and steps plus deferred
  agent-compaction remain the load-bearing justification. One trap: a break-even figure measured against
  a window the traffic never used is a construction, not a result — UltraHorizon reads 0/10 at a 200k
  window purely because its peak request is 30k and the trigger never fires.
- **Overturned: distance is not merely a lossy proxy, it is nearly inert.** Sweeping `closed_dist` over a
  10× range moves closed mass by 2–3 points; sweeping `open_reps` from 2 to 6 moves it by 18. And 44% of
  all mass was last referenced 40+ messages ago while 60% is `open` — most referenced mass is old *and
  still hot*. A distance-based A/B split would confidently cut repeatedly-referenced content. §3's
  reframe is load-bearing, `open_reps` is the only dial worth tuning, and `closed_dist` should be left
  alone.

One methodological result deserves promoting out of the write-up, because it nearly invalidated the
measurement: **the identifier/prose rule decided the answer.** An earlier tokenizer accepted any 10+
character token, so `description`, `transparency`, `efficiency` and `conditions` scored as references and
referenced mass came out at 71% instead of 60%. A manufactured reference makes an output look
load-bearing, so that class of bug fails by **silently declining to compact** — invisible to any metric
that counts only what the component did. Every false positive is now a regression case in
`internal/coref/coref_test.go`, and the residual (lowercase hyphenated compounds, indistinguishable from
real names like `context-guru`) is bounded at ~6 points of *under*-reporting rather than argued away.

**What has to happen next**, in order:

1. ~~Re-run `coref.py` over `capture-tb` / `capture-swe` / `capture-swebench` on the eval box.~~
   **Done** — [eval-box measurement](../results/coref-evalbox.md). `unreferenced` is **28%** on
   `capture-swebench` (50 sessions, 433 outputs), the best measured and double the interactive
   figure, confirming §8's claim that SWE-bench is the Tier-1-rich substrate. Two findings came with
   it: a session-key collision in `coref.py` was silently discarding 98% of that capture (fixed —
   `metadata.user_id` carries the real session id), and the corpus is **too shallow to test
   deferral at all** (peak request 12.6k against a 167k threshold). `capture-tb` and `capture-swe`
   turn out to be smoke captures (6 and 2 outputs), so only one of the three supports a claim.
2. Then, and only on that corpus, flip `cut_closed` on. `open_reps: 3` is the conservative setting;
   `closed_dist` is inert and should stay at its default.
3. `observe` mode on real traffic to read `expand` rate — the precision inner loop from §4 — before any
   scored benchmark run.
4. Only then §8's benchmarks, with the multi-seed and don't-stop-at-first-significance guards.

~~Separately and in parallel, because it needs no API budget and no eval box: measure how often the
agent's own compaction is reachable at all.~~ **Done** — [reachability](../results/coref-reachability.md).
17% of sessions, 29% past 200 model turns; and firing at the crossing rather than late removes the
deficit term, which makes 20–60 turns of headroom affordable where the density pass had 0/19. The
deferral gate is therefore worth building, but it should gate on *fire early*, not just on batch size.

`cut_closed` nonetheless **stays off**: it is 20% of mass on `capture-swebench` and 0% on LOCA, so
the workload spread that made it undefendable is unchanged by having the right corpus.

## What the held-out experiment removed from this list

A separate [selection experiment](../results/coref-selection-experiment.md) ran ten arms against
held-out ground truth. It **closed three questions** that were queued here, and it did so mostly by
ruling things out — which is the cheapest kind of progress this list can make:

- **Do not build a model-in-the-verdict variant.** Eight model arms across two models and four
  prompt shapes; none beat the deterministic index on both axes, and no intersection or union of
  index and model beat the index alone. The intermediate design (`coref` supplies evidence,
  `extract_llm`'s prompt supplies the verdict) is refuted, not merely unproven.
- **Do not specify trims as free text.** Models returned text that was not verbatim in the original
  in the majority of trim verdicts. Any trim path must go through `extract_llm`'s sandboxed-filter
  mechanism, which enforces containment structurally
  (`internal/extract/contain.go`), rather than through a "return the part worth keeping" prompt.
- **Do not pursue replacing the agent's summarizer.** Sustained selective removal is `f × g` with
  `f < 1`, so it multiplies time-to-threshold by `1/(1−f)` — 1.05–1.32× on measured traffic — and
  cannot hold a context flat at any aggression setting. `coref` is a deferral play permanently.

And it **added one item**, ahead of everything above because it is a correctness issue in shipped
code rather than a calibration question:

0. ~~**Fix the kept-verbatim guard before `coref` goes in any preset.**~~ **Done.**
   `MarkKeptVerbatim` keyed by content hash with no session component, so one expand exempted that
   byte-identical content in *every future session* — permanently eroding yield on exactly the
   recurring content worth cutting — and the one-byte flag shared the payload LRU with the
   multi-kilobyte stashes it guards, so it could be evicted and the guard silently lost.

   `keptKey` is now session-scoped (the loop it prevents is intra-session by construction, so that
   is the minimal correct scope), `store.KeptPrefix` is pinned against LRU eviction, and the proxy
   threads `apply.Trace.Session` into the expand loop so the mark lands under the same id the
   pipeline compacted under. Empty session is a no-op rather than a global mark. Covered by
   `TestKeptVerbatimDoesNotLeakAcrossSessions` and
   `TestMarkKeptVerbatimIgnoresAnEmptySession`. Rationale in
   [the proposal, §5.8](coref-compaction.md#5-hard-constraints-the-codebase-imposes).

Two other things it did **not** settle, and neither is cheap: nothing here touches **reward**, and
the experiment ran on captured traffic with a single firing point per transcript. The ordering above
is unchanged — reward is still the gate.
