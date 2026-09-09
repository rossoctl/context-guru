# Iteration 019 — offline probes: what is actually wrong with the merged design

**Type:** offline probe series, no LOCA run. **Cost:** under $2 of gateway spend.
**Instrument:** direct calls to `aws/claude-sonnet-5` on the benchmark gateway (never through
Context Guru), one synthetic 14.6k-token transcript, 4 trials per arm.

Iteration 018 left three things unexplained: the merged design kept ~94% of outputs, `merged_trim`
was ~1 across every arm, and solves fell 16 → 13 → 8. This iteration probes the decision itself
offline instead of buying another $234 LOCA run, and the answer is that **merged has never been
tested in the configuration it was measured good in.**

## 1. Corrections to earlier claims in this repo

Recorded first, because decisions were taken on them.

* **`iter014/results.md`'s "negative answer" is unsafe.** It concluded from `merged_keep` 88% that
  "the model declines to act on evidence". But that arm ran **2,078 decisions across 2,030 calls =
  1.02 candidates per call**, and `merged_kept_whole_batch` **never fired**, which rules out the
  alternative explanation (calls returning well-formed empty arrays contribute zero counted
  decisions). Bulk adjudication is a *comparative* mechanism measured at ~15 candidates; at 1.02 the
  arm was running the **per-output** design already refuted at 6% live-kept. The iteration measured a
  starved variant and attributed the result to the design.
* **The cause was the coref pre-filter, not a per-call cap.** During this work I attributed the
  starvation to `llmMaxPerReq` at `extract_llm.go:818`. That is wrong: `s6-merged.yaml` sets no
  `llm_max_per_request`, so the guard `if e.llmMaxPerReq > 0` never fired. The cause was the prefix
  eligibility pre-filter (149,681 candidates removed), already fixed at `extract_llm.go:695`.
* **Current batch size is 2.63, not 15.** iter018: 2,873 decisions / 1,092 calls. The binding
  constraint is now `min_tokens: 3000` — few outputs in this workload are that large.
* **`AllowCachedPrefix`'s comment is wrong on one point.** It states "the tail restriction is not a
  safety property of the model call, it is a cache-cost property". It is also an *information*
  property. See §4.

## 2. The model can read a cached prefix — the cache idea works

Riding the already-cached context instead of re-sending excerpts was proposed to give the model the
evidence it lacks. Measured on the live route:

| call | tools | `tool_choice` | cache_write | cache_read |
|---|---|---|---|---|
| prefix + appended ask | yes | — | 0 | **19,595** |
| same | yes | `none` | 0 | **19,595** |
| same | **omitted** | — | 0 | 19,129 *(separate entry)* |

* Appending a trailing user message to an identical prefix lands a **full cache read, no write** —
  ~10% of fresh input, ≈$0.03 per call at 100k, ≈$61 across a 2,000-request run.
* `tool_choice: none` is **not** part of the cache key, so tool_use replies can be suppressed free.
* `tools` **are** part of the key; omitting them creates a separate entry.
* This route **rejects assistant prefill** ("the conversation must end with a user message"), which
  the appended-ask shape satisfies naturally.
* Requires `model: {source: request}` — caches are per-model, so a haiku call cannot read sonnet's
  cache. `components.go:91` already supports this.

## 3. Transport versus judgment — the model must never carry text

Two failure classes were conflated. **Judgment** ("is this spent") is what a model is for.
**Transport** (naming an item, reproducing text) is what it is worst at, and the design asks for it.

| what the model must emit | outcome |
|---|---|
| opaque `tool_use_id` values | **hallucinated** — answered `toolu_01…07` for `toolu_probe_00…07` |
| small integer labels | **0 bad labels in 40+ trials** |
| short verbatim quotes | **0 non-verbatim of 59 quotes** |
| `trim` retained records, verbatim | **8 of 9 invented** (production: `merged_trim` 1, `merged_trim_not_contained` 8) |

**Rules:** short labels for identity, short verifiable quotes for evidence, and never ask the model
to reproduce content. A positional selector (`keep_records: [0,5,9]`) is *not* a fix — it requires
annotating the content, which is impossible when the content is read from the cache, and breaks when
`format` has reformatted the JSON. A value-based selector (`keep_where: {field, values}`) survives
both.

## 4. Relevance is local; need is global

The forward-looking question ("will this be needed") was assumed answerable from the goal plus the
output. It is not. **Need = relevance minus what has already been captured elsewhere**, and the
second term lives in the later turns. So *both* halves of the decision require the same window:

| | needs the later turns for |
|---|---|
| backward (coref / veto) | reuse an exact matcher missed |
| forward (extract_llm) | whether the information was already extracted and recorded |

This is why the tail restriction mattered: on tail content there are few later turns, so relevance ≈
need, and the local prompt was sound. `allow_cached_prefix: true` moves the model onto old outputs —
where the gap is widest — while still asking the local question. The design compensated with the
index as a pre-filter; **merged bypasses that pre-filter**, so merged-on-full-body has *neither* the
index veto nor the window. Both safeguards absent, and that coincides with solves 16 → 13 → 8.

## 5. Trim is dead weight

Offered across 21 opportunities in these probes, `trim` was chosen **zero** times, and the
keep/drop/trim and keep/drop arms were **identical on every metric**. Production agrees: 1 accepted
in ~5,000 decisions. What trim was for is already covered twice — `mergedResidue` leaves a shape
descriptor, and `tryMark` stashes the original, verified recoverable end-to-end (120/120 rows).

A speculation of mine was **refuted**: I predicted a binary contract would improve drop quality by
simplifying the question. It changed nothing.

## 6. The false drop is a policy error, not a comprehension error

The trap: batch 3's value is restated later only in transformed form (`1200` → `"1.2k"`), so the
index sees `refs=0`, while the user's standing instruction ("reconcile ledger batches 0-**3**") is
incomplete for it. Correct answer: **keep**.

Asked the factual question with no removal decision attached, the model was right **3/3**:
`{"batch3_reconciled": false, "batches_with_filed_totals": [0,1,2]}`. It has the facts and drops
anyway. The cause is our own instruction — `BuildBulkPrompt` says *"WHAT THE AGENT IS DOING NOW
(judge relevance toward this)"* — and batch 3 is genuinely irrelevant to the current step while
still required by an outstanding obligation. **The criterion was never stated: "spent" as
"not needed for the current step" versus "not needed by any unfinished obligation".**

This is a production harm mechanism, not a synthetic curiosity: LOCA tasks are multi-step single
instructions, so a current-step criterion systematically drops data needed by later steps of the
same instruction — silently.

### What fixed it, and what did not

| arm | Tier-2 false drop | found all 3 truly-spent |
|---|---|---|
| control — relevance to current step | 4/4 | 4/4 |
| **criterion restated** (obligations count) | **4/4** | 4/4 |
| criterion **+ must name and quote the obligation** | **2/4** | 4/4 |
| solo, batch = 1 | 4/4 | n/a |

**Stating a better rule is inert. Requiring the model to emit the evidence is what moves it.** Arms 2
and 3 carry identical criterion text; only the required output field differs.

Pooled over four independent runs (**exploratory, not pre-registered**): controls **14/14** false
drops, forced-evidence **9/14**, Fisher exact ≈ p 0.04. Yield never suffered — all 28 trials found
the genuinely-spent outputs, and none became over-cautious.

**9/14 is 64%.** The forcing function moved the veto from *always wrong* to *usually wrong*. That is
a detectable improvement and an unusable capability: single-digit false-drop is the bar for removing
things silently. This is **not** a population false-drop rate — the case was selected for difficulty,
n is 14, and the index's documented 11% comes from 1,119 decisions on real traffic. What it
establishes is that the capability is unreliable exactly where it was supposed to add value, and the
deterministic index fails the same case for free.

Batch = 1 dropped the output **4/4**, including when keep was correct — `coref-selection-experiment.md`
finding 1 (6% live-kept when shown one output) replicating in a different harness.

## 7. Methodology note: self-reports are reliable about beliefs, not about causes

Asked to critique its own instructions, the model named `"Filed. Continue."` as a false-closure cue
that tripped it. Removing that cue made the false drop **worse** (3/4 against 2/4). The *factual*
half of its self-report was accurate and independently confirmed; the *causal* half was wrong.

Self-reporting earned its place here by generating a cheap testable hypothesis that took two minutes
to eliminate. Use it that way — as a hypothesis generator requiring validation like any other
method — not as evidence.

## 8. Limits

Synthetic single transcript, adversarially constructed by the author, n=4 per arm, ground truth for
the trap is the author's judgment (defensible — no total was ever filed for batch 3 — but a
judgment). This is decision quality, which `docs/results/measurement-limits.md` establishes cannot
speak to reward. Nothing here measures reward.

## 9. What follows

1. **Cut `trim`.** Never chosen, only verdict requiring transport, fully covered by residue + expand.
2. **Make the criterion explicit and force the evidence** — obligations (a)/(b)/(c), named and quoted
   per candidate. Free in yield, and the only thing measured to help.
3. **Do not build on the veto.** It is the justification for the model call and it fails ~2/3 of the
   time on the case it exists for.
4. **Merged has still never run bulk-sized.** Batch is 2.63; the 58% result came from ~15. Lowering
   `min_tokens` is a config change, not a code change. Any decision to close merged should come after
   that, otherwise it closes a design that was never once run as specified.
5. Correct `iter014/results.md` in place, and fix the `AllowCachedPrefix` comment.
