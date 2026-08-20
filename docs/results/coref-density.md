# Co-reference density on real agent traffic

The measurement pass [`coref-compaction.md` §7](../proposals/coref-compaction.md) says must run before
the `coref` component is calibrated. It has now run, on **three corpora** — and the headline is that they
disagree by roughly 4x, which is itself the most useful result. These numbers are the SECOND
version: review of PR #80 found a defect that inflated the first set badly, and
[what review changed](#what-review-changed) records both the defect and the delta.

All of it cost **zero API dollars**: the runs already happened and left their logs on disk.

## The three corpora

| Corpus | What it is | Sessions | Outputs ≥300 tok | Tool-output mass | Median peak request |
|---|---|---|---|---|---|
| **Claude Code** | Interactive research/documentation work on one project, via [`cc_capture.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/cc_capture.py) | 31 | 1,344 | 1,329,009 tok | 179,660 tok |
| **UltraHorizon** | `loopb_uh` benchmark runs (sequence-exploration game, `python_execute`), via [`runlog_capture.py`](https://github.com/rossoctl/context-guru/blob/main/deploy/harbor/runlog_capture.py) | 10 | 134 | 170,477 tok | 30,220 tok |
| **LOCA-bench** | `all_trajectories.json` from 35 LOCA task runs (MCP filesystem/sheets/email envs) | 9 | 166 | 591,932 tok | 52,258 tok |

```sh
# interactive traffic — Claude Code writes a transcript of every session it runs
python3 deploy/harbor/cc_capture.py /tmp/cc.jsonl ~/.claude/projects/<proj>/<session>.jsonl

# benchmark traffic — the harnesses already logged their request bodies
python3 deploy/harbor/runlog_capture.py /tmp/uh.jsonl   --label ultrahorizon .../llm_calls.jsonl
python3 deploy/harbor/runlog_capture.py /tmp/loca.jsonl --label loca         .../all_trajectories.json

python3 deploy/harbor/coref.py /tmp/uh.jsonl window=32000 fire_frac=0.6 sweep=1
```

!!! warning "None of this is the eval-box corpus — which has since been measured, and is *smaller*"
    §8's acceptance criteria are written against `capture-swe` / `capture-tb` (SWE-bench Verified and
    Terminal-Bench, cache-read-dominated, the incumbent regression floor). Those captures live on the eval
    box and were not reachable. What is measured here is real agent traffic from three *other* workloads.
    It answers §7's questions with data instead of argument, and it turns out to answer them
    **differently per workload** — which is a stronger reason to re-run on the eval box, not a weaker one.

    **That re-run has since happened** ([eval-box measurement](coref-evalbox.md)), and it reverses the
    apology in this box. `capture-swebench` is 50 **shallow** sessions (433 outputs, peak request
    12,607 tokens), and `capture-tb` / `capture-swe` turn out to be smoke captures of 6 and 2 outputs.
    The three corpora on this page are **larger and deeper** than the corpus they defer to. Read
    `capture-swebench` as the most *relevant* corpus and these as the most *substantial* — neither is
    sufficient alone.

## The headline: reference density is a property of the workload

Measured with the `opaque` class and the opportunity floor both in force (see
[what review changed](#what-review-changed) — the first version of these numbers was materially wrong):

| | Claude Code | UltraHorizon | LOCA-bench |
|---|---|---|---|
| `opaque` — introduced nothing trackable, **no evidence** | 8% | 20% | **40%** |
| `unreferenced` — introduced identifiers, nothing used them | **13%** | **51%** | **22%** |
| `closed` — value taken, survives above | 15% | 6% | 0% |
| `open` — recent, repeated, or too new to judge | 62% | 21% | 36% |
| **cut at the shipped default** (`unreferenced` only) | **13%** | **51%** | **22%** |

Interactive work on a coherent codebase keeps returning to the same files, symbols and errors, so 62% of
its tool-output mass is still load-bearing. UltraHorizon's game-exploration traffic surveys, verifies, and
moves on, so half its mass is provably dead. `coref`'s value is not a single number; it is
workload-dependent, and it differs by ~4× across three corpora.

LOCA is the interesting case and the reason the `opaque` class exists: its raw `unreferenced` share looked
like **95%**, and 40 points of that was mass the index cannot see into at all (11 outputs averaging 22k
tokens — bulk record and spreadsheet dumps of human-readable values), with most of the remainder outputs
too near the tail to have been referenced yet. The honest free cut there is 22%, not 95%.

### And LOCA behaves exactly as §8 predicted

§8 argued LOCA would be a **Tier-2/3 stress test** rather than a showcase for exact matching, because its
BigQuery/Sheets/Excel envs *aggregate and compute over* tool results, so references arrive transformed past
the point where a substring match can see them. The signature is unmistakable: **0% closed — not one
output in 166 was referenced once or twice and then left alone** — alongside the largest `opaque` share of
any corpus at 40%. References there are either immediate-and-repeated (the `open` 36%) or invisible to an
exact matcher. That is the predicted result, and it is the strongest reason never to read `unreferenced`
as "unused" on that corpus.

## What §7's other questions came back with

**A reference consumes a small fraction of what its output introduced** — median 18.7% (Claude Code),
11.5% (UltraHorizon), 50% (LOCA). Hypothesis A — "took one value out of a large response, does not need
the rest" — is confirmed on the two long-horizon corpora and much weaker on LOCA, whose outputs are
smaller and more thoroughly consumed.

**Distance is not the discriminator. Repetition is.** On the Claude Code corpus, sweeping `closed_dist`
over a 10× range (4→40 messages) moves closed mass by 2–3 points; sweeping `open_reps` from 2 to 6 moves
it by 18:

| `open_reps` ≥ | `closed_dist` 4 | 8 | 12 | 24 | 40 |
|---|---|---|---|---|---|
| 2 | 10% | 10% | 10% | 9% | 9% |
| **3** | 16% | 15% | **15%** | 14% | 13% |
| 4 | 21% | 20% | 20% | 19% | 18% |
| 6 | 28% | 27% | 26% | 25% | 23% |

The corroborating figure: **44% of all Claude Code tool-output mass was last referenced 40+ messages
ago**, and yet 62% of mass is `open`. Most referenced mass is *old and still hot*. A policy separating
case A from case B by distance — the original framing — would confidently cut repeatedly-referenced
content believing it was taking the safe early cut. §3's reframe from distance to open-vs-closed is not a
refinement; it is the difference between the policy working and not. **`closed_dist` is not worth tuning;
`open_reps` is the dial.**

**Break-even is workload-dependent too, and it is worse than the first pass claimed** — necessarily, since
`opaque` and tail-protected outputs left the cut set and `S` shrank:

| | Claude Code | UltraHorizon | LOCA-bench |
|---|---|---|---|
| median cut `S` | 10,539 tok | 14,164 tok | 21,234 tok |
| median rewritten suffix `W` | 157,189 tok | 26,044 tok | 49,611 tok |
| turns `T` needed | **138** | **23** | **34** |
| sessions whose observed `T` cleared it | **9/30** | **4/8** | **2/6** |

§4's arithmetic holds everywhere, and the margin is thin everywhere: **roughly a third of sessions repay
the cache-write on tokens**, and on long interactive transcripts the median session would need 138 more
turns. Batching moves break-even from impossible (T > 276 for a single early cut) to *merely unlikely* on
tokens alone. This is the third decision rule in §7 firing: **`coref` must be justified on step reduction
and on deferring the agent's own compaction, and evaluated that way.** `corr(Δsteps, Δcost) = +0.95` says
tokens were never the interesting axis; these numbers say it is not even a supporting one.

Window choice matters here and is easy to get wrong: measured against a 200k window, UltraHorizon shows
**0** sessions clearing break-even — but its peak request is 30k, so `fire_frac × window` is never reached
and `T` collapses to zero by construction. At a 32k window (matching what those runs actually held) it is
4/8. A break-even figure is meaningless without a window the traffic actually used.

## Can it defer the agent's own compaction?

The proposal's largest claimed win is pushing back Claude Code's self-compaction (167k on a 200k
model). Of the 31 Claude Code sessions, **19 passed that threshold**, so the question is answerable
on this corpus. The requirement is not merely to clear the threshold but to stay clear — cutting to
exactly the line buys one turn, and then you either eat the compaction or pay a *second* cache-write
at maximum `W`:

```
required cut  ≥  (usage − threshold)  +  growthPerTurn × headroomTurns
```

| headroom bought | required cut, as a share of the request | achievable with `unreferenced` + `closed` |
|---|---|---|
| H = 0 (bare clear) | 7.3% | 10/19 |
| H = 20 | 12.6% | 5/19 |
| H = 40 | 18.0% | **0/19** |
| H = 60 | 23.5% | **0/19** |

Mean available cut is **4.4%** of the request (`unreferenced`) and **9.6%** (`+closed`), against a
mean deficit of 12.9k on a ~180k request and mean growth of ~514 tokens/turn. So a bar high enough
to avoid paying twice — 20–25%, which is what 40–60 turns of headroom costs — is a bar Tier-1
matching **cannot clear on this corpus**.

The same figures condemned the original `min_batch_frac: 0.15`, which admitted **1 of 19** sessions
with `cut_closed` on and **0 of 19** at the shipped cut set. It is now 0.05 (16/19), recorded as a
starting point rather than a claim.

!!! danger "The deficit column is an artifact of firing LATE, and the peak column is not usable at all"
    Peak request ≈180k and deficit ≈13k are shaped by `cc_capture.py` segmenting at 180k tokens, so
    peaks cluster there by construction. The durable finding is the one independent of it:
    **available cuttable mass is 4–10% of the request.**

    Two later corrections, both from [the reachability pass](coref-reachability.md):

    1. **The deficit term should be ~0, not 7.3%.** Segmenting at 180k places the measurement point
       *past* the threshold; at the moment the agent actually compacts, usage *is* the threshold by
       definition. Fired at the crossing, the requirement collapses to `growth × headroom` — and the
       H=40 row below goes from 0/19 to affordable. The table measures a late fire, not the design.
    2. **Absolute request sizes are not recoverable from this corpus at all.** Claude Code
       transcripts are trees (25–51 forks, 338–632 leaves per compacted transcript) and the
       `parentUuid` graph is too fragmented to reconstruct the active branch. A linear read spans
       multiple context windows. Duplicate tool-output mass is only 3% pooled / 2% median, so the
       **share**-based results on this page are unaffected — but read every absolute token figure as
       indicative only.

The design consequence — a gate that asks whether `coref`'s cut is the *decisive* one rather than
whether it is large, and what it would take to know the distance to the threshold — is worked
through in the proposal's
[deferral gate](../proposals/coref-compaction.md#the-deferral-gate-designed-unquantified). It is
unbuilt, and deliberately so: how often the prize is reachable at all has never been measured, and
`modes.Tracker`'s reset detection answers that with no new machinery.

## What review changed

Review of PR #80 raised a counter-example that turned out to invalidate the first version of every number
above. It is worth recording in full, because the defect was invisible in the arithmetic.

**The counter-example.** A tool output returns records; the model's next turn references one:

```jsonc
[{"name": "david", "id": 123, "address": "foobarbaz"},
 {"name": "osher", "id": 235, "address": "banana"}]
// model: "I need to remember david 123 address."
```

The reference is real, but the value needed — `foobarbaz` — was never copied into a model turn. The model
referenced an **anchor** in order to point at a payload it did not restate. So §3's "any reference is a
surviving copy" is too strong, and `closed` cannot rest on "referenced once, long ago" alone.

**The worse defect it exposed.** Run through the actual index, that output yields **zero** trackable
tokens: `david`, `123`, `foobarbaz` are short lowercase words and a 3-digit number, precisely what the
precision rules below exclude. With no novel tokens there are no references, so it scored `unreferenced` —
**the class the default configuration cuts.** Two states satisfy `refs == 0` and they are opposites:

| state | meaning |
|---|---|
| introduced 200 identifiers, nobody touched one | evidence of deadness → safe cut |
| introduced nothing the index can see | **no evidence** → no opinion |

`opaque` is now its own class and is never cut, and an **opportunity floor** (`min_later_turns`) stops an
output too near the tail from being scored as unused. The delta on the same corpora:

| | Claude Code | UltraHorizon | LOCA-bench |
|---|---|---|---|
| `unreferenced` as first reported | 23% | 78% | 95% |
| `unreferenced` after the fix | **13%** | **51%** | **22%** |
| of which reclassified `opaque` | 8% | 20% | **40%** |
| sessions clearing break-even | 15/30 → **9/30** | 7/10 → **4/8** | 4/9 → **2/6** |

The first version would have deleted 40% of LOCA's tool-output mass on no evidence at all, under the
default config. Both the class and the floor are now tested on both sides of the implementation.

## Two further methodological results

### 1. The identifier/prose rule decided the answer

An earlier run of this measurement reported **71%** of Claude Code mass as referenced. Nothing about the
corpus changed — only the rule deciding whether a token is an identifier or an English word. The original rule accepted any token of 10+ characters, any
token containing punctuation anywhere, and any bare number of 3+ digits, and its top
reference-producing "identifiers" were:

```
395  description      140  transparency     116  final_score      104  conditions
142  forever_fetch    135  2026             112  integration       94  persistent
```

`description`, `transparency`, `integration`, `efficiency`, `conditions`, `orientation`,
`effectiveness` — all ≥10 characters, all ordinary English. Plus `e.g.` / `try:` / `None:` / `memory.`
(prose plus a sentence mark) and `2026` (a year). Each collision manufactures a reference, and a
manufactured reference makes an output look load-bearing — so this class of bug fails by **silently
declining to compact**, which is invisible to any metric that only counts what the component did.

The rules were tightened to require *interior* structure after trimming edge punctuation, a digit, or
camelCase — with no bare length rule and no stopword list (a stopword list does not survive a change of
domain, or of language). After the fix the top drivers are `forever_fetch`, `final_score`, `OpenManus`,
`db_state`, `ANTHROPIC_BASE_URL`, `session_id`, `v1/messages`, `GraphStore`, `json.dumps`,
`os.path.join` — identifiers. Every false positive above is now a regression case in
`internal/coref/coref_test.go`, because the Go component and this script must agree or the thresholds
measured here are calibrated for a different algorithm than the one that ships.

**Residual, bounded rather than argued.** Lowercase hyphenated compounds (`cross-session`, `end-to-end`,
`dead-end`) still pass, and nothing structural separates them from real names like `context-guru` or
`terminal-bench`. Rejecting the whole class as a sensitivity arm moves cuttable mass from 39% to 45%. The
shipped tokenizer is therefore **conservative by ~6 points** — it under-reports what can be cut, which is
the safe direction.

### 2. That fix also blinded the Tier-2 detector, and the report reflects it

`derived_evidence` — the Tier-2 proxy, "numeric values the model states that appear nowhere earlier" —
runs over the same token set. Requiring bare numbers to carry 5+ digits or a separator means most
*computed* values (sums, counts, converted units — small numbers) no longer register at all. Tier-2
evidence now reads 2% on Claude Code and **0% on LOCA**, and that 0% must not be read as "LOCA has no
transformed references": the 0% closed share says the opposite. It means *no transformed references among
the tokens this tokenizer still accepts*. Precision for the primary signal was bought at the cost of
recall for the caveat signal. Measuring Tier 2 properly needs its own detector, not this one.

Keep the scope straight, because it is easy to over-read a 0%: this proxy covers **Tier 2 only**. Tier 3
— a semantic reference like "as I noted earlier" — carries no shared token *and* no novel numeric, so it
has **no measurement here at all**, by design rather than by regression. The two therefore fail
differently: Tier 2 has a detector that is nearly blind, Tier 3 has none. On LOCA, with 0% `closed` and
40% `opaque`, the defensible statement is that Tier-2 *and* Tier-3 references are both common there and
both unmeasured — not that transformed references are absent.

## What this settles, and what it does not

**Settled enough to act on:**

- `cut_unreferenced` (the shipped default) is justified **on yield** everywhere, and its size is
  workload-dependent: 21% of mass on interactive traffic, ~70% on benchmark traffic, with no
  calibrated threshold and no model call. Its **accuracy** is a separate question this pass could
  not ask, and [the held-out experiment](coref-selection-experiment.md) since answered it:
  **11% false-drop**, irreducible, and a lower bound. Read the yield figures here alongside that.
- `closed_dist` is nearly inert; `open_reps` is the dial. Leave the recency threshold alone.
- Break-even needs a window the traffic actually used, or it reports a construction rather than a result.

**Not settled — and why `cut_closed` stays off by default:**

`closed` mass is 15% on interactive traffic, 8% on UltraHorizon and **0% on LOCA**. A knob whose yield
ranges from 0% to 15% by workload has no defensible default, and none of these corpora is the one §8's
acceptance criteria are written against. Enabling `cut_closed` per config for a measured arm is
reasonable; shipping it on is not.

Still outstanding, unchanged: the eval-box re-run on `capture-swe`/`capture-tb`, `observe`-mode `expand`
rate as the precision inner loop, and only then the scored benchmarks.

## Caveats

- **Not the eval-box corpus** (see the warning above) — the single largest caveat.
- **Small n, and one author's traffic.** 31 + 10 + 9 sessions; the Claude Code corpus is mostly one
  project. No seeds, no variance estimates. Every figure is a point estimate of unknown spread.
- **Tail bias is now guarded, not merely bounded** — `min_later_turns` (default 8) treats an output with
  too few later model turns as `open`. Before that guard existed, LOCA's headline was the most affected.
  Note the guard is justified **structurally** (it stops a batched pass preferring the newest
  context) and **not** by accuracy: swept against held-out ground truth, `min_later=0` gives more
  mass at a *lower* false-drop rate. See
  [the experiment](coref-selection-experiment.md#5-min_later_turns-does-not-earn-its-keep-on-this-metric-measured).
- **Session boundaries are reconstructed.** Claude Code transcripts are cut at 180k tokens to approximate
  compaction boundaries the transcript does not record; UltraHorizon runs are cut where the harness's own
  context wipe drops the message count. Measuring across a boundary the model cannot see across would
  invent cuttable mass out of the harness's reset.
- **`thinking` blocks count as reference-bearing** — model-authored, and the real wire body carries them.
- **Token counts are a ~4-chars/token proxy**, consistent with `analyze_content.py`. Fine for shares, not
  for billing arithmetic.
- **Two additive fields** (`turn_tokens`, `conv`) were added to what `coref.py` accepts, for converted
  logs only. A real capture sets neither and its behaviour is unchanged.

See also: [the proposal](../proposals/coref-compaction.md) · [the held-out selection
experiment](coref-selection-experiment.md) · [the component](../components/coref.md) ·
[glossary / cheat sheet](../reference/coref-glossary.md) · [improvement plan](improvement-plan.md)
