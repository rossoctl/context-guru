# Co-reference density on real agent traffic

The measurement pass [`coref-compaction.md` §7](../proposals/coref-compaction.md) says must run before
the `coref` component is calibrated. It has now run, on **three corpora** — and the headline is that they
disagree by a factor of three, which is itself the most useful result.

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

!!! warning "None of this is the eval-box corpus"
    §8's acceptance criteria are written against `capture-swe` / `capture-tb` (SWE-bench Verified and
    Terminal-Bench, cache-read-dominated, the incumbent regression floor). Those captures live on the eval
    box and were not reachable. What is measured here is real agent traffic from three *other* workloads.
    It answers §7's questions with data instead of argument, and it turns out to answer them
    **differently per workload** — which is a stronger reason to re-run on the eval box, not a weaker one.

## The headline: reference density is a property of the workload

| | Claude Code | UltraHorizon | LOCA-bench |
|---|---|---|---|
| `unreferenced` — nothing later used it | **23%** | **78%** | **95%** |
| `closed` — value taken, survives above | 15% | 8% | 0% |
| `open` — recent or repeated → keep | 60% | 13% | 4% |
| cuttable at shipped thresholds | 38% | 86% | 95% |

Interactive work on a coherent codebase keeps returning to the same files, symbols and errors, so 60% of
its tool-output mass is still load-bearing. Benchmark tasks survey, extract an answer, and move on — so
**three to four times as much of their mass is never referenced again**. `coref`'s value is not a single
number; it is workload-dependent, and it is much larger on benchmark traffic than on the traffic I
measured first.

### The bias in that, quantified rather than waved away

An output near the end of a transcript has no later turns that *could* reference it, so short sessions
inflate `unreferenced` for free. LOCA sessions average ~18 turns, so this had to be bounded. Restricting
to outputs with at least N later model turns:

| min later model turns | Claude Code | UltraHorizon | LOCA-bench |
|---|---|---|---|
| ≥ 0 (all) | 23% | 78% | 95% |
| ≥ 5 | 23% | 77% | 91% |
| ≥ 10 | 22% | 76% | 80% |
| ≥ 20 | **21%** | **70%** | **70%** |

So LOCA's 95% is substantially tail bias — its honest range is 70–95% depending on how much opportunity
you demand. UltraHorizon (78% → 70%) and Claude Code (23% → 21%) are robust. **The ordering survives
every cut: at a common ≥20-later-turns bar, benchmark traffic has ~3.3× the unreferenced mass of
interactive traffic.** That is the finding.

### And LOCA behaves exactly as §8 predicted

§8 argued LOCA would be a **Tier-2/3 stress test** rather than a showcase for exact matching, because its
BigQuery/Sheets/Excel envs *aggregate and compute over* tool results, so references arrive transformed
past the point where a substring match can see them. What an exact matcher reports on LOCA is 95%
unreferenced and **0% closed — not one output in 166 was referenced once or twice and then left alone.**
References there are either immediate-and-repeated (the 4% `open`) or invisible. That is the predicted
signature, and it is the strongest reason not to read `unreferenced` as "unused" on that corpus.

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
ago**, and yet 60% of mass is `open`. Most referenced mass is *old and still hot*. A policy separating
case A from case B by distance — the original framing — would confidently cut repeatedly-referenced
content believing it was taking the safe early cut. §3's reframe from distance to open-vs-closed is not a
refinement; it is the difference between the policy working and not. **`closed_dist` is not worth tuning;
`open_reps` is the dial.**

**Break-even is workload-dependent too, and better on benchmarks:**

| | Claude Code | UltraHorizon | LOCA-bench |
|---|---|---|---|
| median cut `S` | 16,432 tok | 15,479 tok | 51,022 tok |
| median rewritten suffix `W` | 159,183 tok | 26,044 tok | 51,532 tok |
| turns `T` needed | **95** | **17** | **14** |
| sessions whose observed `T` cleared it | 15/30 | 7/10 | 4/9 |

§4's arithmetic holds everywhere, but the margin differs sharply. On a 180k interactive transcript a
batched cut is ~10% of the request against a huge rewritten suffix, so it needs 95 more turns. On
benchmark traffic the cut is a large share of a small transcript, so it needs 14–17 — and most sessions
have that. Batching moves break-even from unreachable (T > 276 for a single early cut) to *comfortable on
benchmarks* and *marginal on long interactive sessions*.

Window choice matters here and is easy to get wrong: measured against a 200k window, UltraHorizon shows
0/10 sessions clearing break-even — but its peak request is 30k, so `fire_frac × window` is never reached
and `T` collapses to zero by construction. At a 32k window (matching what those runs actually held) it is
7/10. A break-even figure is meaningless without a window the traffic actually used.

## Two methodological results

### 1. The identifier/prose rule decided the answer

The first run of this measurement reported **71%** of Claude Code mass as referenced and 28% as cuttable.
The corrected run reports 60% and 38%. Nothing about the corpus changed — only the rule deciding whether a
token is an identifier or an English word. The original rule accepted any token of 10+ characters, any
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
recall for the caveat signal. Measuring Tier-2 properly needs its own detector, not this one.

## What this settles, and what it does not

**Settled enough to act on:**

- `cut_unreferenced` (the shipped default) is justified everywhere, and its size is workload-dependent:
  21% of mass on interactive traffic, ~70% on benchmark traffic, with no calibrated threshold and no
  model call.
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
- **Tail bias is bounded, not eliminated** — see the ≥N table; LOCA's headline is the most affected.
- **Session boundaries are reconstructed.** Claude Code transcripts are cut at 180k tokens to approximate
  compaction boundaries the transcript does not record; UltraHorizon runs are cut where the harness's own
  context wipe drops the message count. Measuring across a boundary the model cannot see across would
  invent cuttable mass out of the harness's reset.
- **`thinking` blocks count as reference-bearing** — model-authored, and the real wire body carries them.
- **Token counts are a ~4-chars/token proxy**, consistent with `analyze_content.py`. Fine for shares, not
  for billing arithmetic.
- **Two additive fields** (`turn_tokens`, `conv`) were added to what `coref.py` accepts, for converted
  logs only. A real capture sets neither and its behaviour is unchanged.

See also: [the proposal](../proposals/coref-compaction.md) · [the component](../components/coref.md) ·
[glossary / cheat sheet](../reference/coref-glossary.md) · [improvement plan](improvement-plan.md)
