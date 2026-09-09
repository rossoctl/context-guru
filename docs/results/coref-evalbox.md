# The eval-box measurement, finally run

Every co-reference document in this repo carries the same caveat: *"none of this is the eval-box
corpus."* Implementation status (`docs/proposals/coref-implementation.md`) makes re-running on
`capture-swe` / `capture-tb` / `capture-swebench` **step 1**, ahead of everything else, because
those are the captures §8's acceptance criteria are written against.

It has now run, on the box itself. It cost **$0** — the captures already existed.

It also found a bug in `coref.py` that was **silently discarding 98% of the corpus**, and the
headline result inverts a framing that runs through all the earlier documents.

## The bug: a session key that collapses benchmark traffic

`coref.py` grouped requests into sessions by hashing `json.dumps(first_user_message)[:200]`. That is
sound for interactive traffic, where every session opens on a different human sentence. It is
**catastrophic on benchmark traffic**, where every task's instruction opens with the same long
standard preamble.

Measured on `capture-swebench.jsonl`: the 200-character prefix has only **19 distinct values, and the
most common one covers 1,771 of 1,795 requests.** Because only the largest member of each group is
analysed, 18 of the 19 groups contributed nothing but stray single-message calls, and the
measurement reported **one session**.

The capture already carried the right key all along — the Anthropic clients pack
`{device_id, account_uuid, session_id}` into `metadata.user_id`. Preferring it recovers **50
sessions** from the same file:

| | before the fix | after |
|---|---|---|
| sessions | 1 usable (of 19 groups) | **50** |
| tool outputs ≥300 tok | 25 | **433** |
| tool-output mass | 20,655 tok | **355,771 tok** |

A **17× larger corpus**, from the same bytes. This is the same class of defect as the `conv`
collision already fixed for `cc_capture.py` — fixed there, never fixed for real captures — and it
fails the same silent way: it does not error, it just measures less and reports confidently.

!!! warning "It is worth stating what this means for reading the earlier docs"
    Any figure previously produced from a real capture with no explicit `conv` field was computed
    over whatever survived the collision. The three corpora in
    [the density pass](coref-density.md) were all converted with explicit `conv` keys by
    `cc_capture.py` / `runlog_capture.py`, so **they are unaffected**. But it is the reason the
    eval-box captures had looked empty on earlier glances.

## The result on `capture-swebench` (SWE-bench Verified, 50 sessions)

| bucket | outputs | mass | share | |
|---|---|---|---|---|
| `opaque` | 42 | 24,781 | 6% | no evidence — never cut |
| **`unreferenced`** | 151 | 99,845 | **28%** | the shipped default cut |
| `closed` | 85 | 74,380 | 20% | opt-in |
| `open` | 155 | 156,765 | 44% | keep |

Alongside: reference consumes a median **14.3%** of what its output introduced (the
"took a value, dropped the rest" pattern, strongest of any corpus measured); Tier-2 numeric evidence
2%; `open_reps` is again the dial and `closed_dist` again nearly inert.

### This is the best yield yet, on the corpus that counts

`cut_unreferenced` in context:

| corpus | `unreferenced` | `+closed` |
|---|---|---|
| Claude Code (interactive) | 13% | 28% |
| LOCA-bench | 22% | 22% |
| **SWE-bench Verified (eval box)** | **28%** | **48%** |
| UltraHorizon | 51% | 57% |
| Terminal-Bench (eval box, n=3 — see caveat) | 71% | 71% |

§8 predicted SWE-bench would be the Tier-1-rich substrate, and it is: **more than double the
interactive yield**, with a healthy `closed` share (20%) where LOCA had 0%. The proposal's
benchmark-selection argument is confirmed on the corpus it was written about.

## But the same corpus removes the deferral case entirely

**Peak request, median: 12,607 tokens.** Against an agent-compaction threshold of 167,000.

These sessions are not remotely long enough to compact. So on the authoritative corpus the largest
claimed win in the proposal — deferring the agent's own compaction — **cannot occur at all**, not
because the cut is too small but because the pressure never arrives. That is consistent with
[reachability](coref-reachability.md), which found the prize present in only 17% of interactive
sessions; here it is 0%.

And the token economics stay thin. At a window this traffic actually uses (20k), **6 of 48 sessions
clear break-even** (`S × T > 11.5 × W`; median cut 2,550 tok against a 9,028-token rewritten suffix,
needing T ≈ 37). Measured against a 200k window it is 0/48 — the window artifact the density pass
warned about, and a reminder that a break-even figure without a window the traffic used is a
construction, not a result.

## The caveat that turned out to matter most

**Two of the three eval-box captures are too small to conclude from.**

| capture | sessions | outputs ≥300 tok | mass | peak request |
|---|---|---|---|---|
| `capture-swebench.jsonl` | 50 | 433 | 355,771 tok | 12,607 |
| `capture-tb.jsonl` | 3 | 6 | 4,707 tok | 6,391 |
| `capture-swe.jsonl` | 1 | 2 | 897 tok | 4,201 |

`capture-tb`'s striking 71% `unreferenced` rests on **four outputs**. It is a smoke capture, not a
corpus, and the same is true of `capture-swe`. Only `capture-swebench` supports a claim.

**This inverts the caveat every earlier document carried.** Those documents apologised for measuring
31 interactive sessions / 1,344 outputs instead of "the real corpus" — and the real corpus is one
usable capture of 50 shallow sessions plus two smoke files. The interactive corpus was **larger and
deeper** than the thing it was deferring to. The honest ranking is that `capture-swebench` is the
most *relevant* corpus and the interactive one is the most *substantial*, and neither is sufficient
alone.

## What this settles

| question | answer |
|---|---|
| Yield on the corpus the criteria are written against | **28% (`unreferenced`), 48% (`+closed`)** — the best measured, and double interactive |
| Was §8 right that SWE-bench is the Tier-1-rich substrate? | **Yes**, confirmed |
| Can `coref` defer agent compaction here? | **No** — peak request 12.6k vs a 167k threshold. Structurally impossible on this corpus |
| Do the token economics work? | **6/48 sessions**, and only at a realistic window |
| Is `cut_closed` now safe to enable? | **Still no.** 20% here vs 0% on LOCA — the workload spread that keeps it off is unchanged |
| Reward | **Still unmeasured.** Still the gate. The box can now run it |

Next: this box has Harbor, Docker with authenticated Hub pulls, 16 CPUs and x86_64, so the scored
benchmark runs the acceptance criteria actually require are runnable here for the first time.

See also: [density](coref-density.md) · [selection experiment](coref-selection-experiment.md) ·
[reachability](coref-reachability.md) · the proposal (`docs/proposals/coref-compaction.md`)
