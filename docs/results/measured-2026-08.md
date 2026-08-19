# Where the savings actually are — measured, August 2026

This page records a measurement pass over real production traffic and real Claude Code
sessions, and the changes it produced. It is written to be uncomfortable where the numbers
are uncomfortable: at the start of this pass **context-guru was net negative**, and saying
so plainly is the only way the fixes mean anything.

Corpus: 14,298 requests / 1,771 sessions over 2.2 days on the hosted service, plus five
fresh Claude Code CLI sessions driven through a local proxy carrying the same configuration.

## 1. The headline, honestly

| | |
|---|---|
| provider spend | $2,523.67 |
| compaction saved | $6.96 (0.28% of spend) |
| context-guru's own LLM spend | $17.22 |
| prefix split (`cachesplit`) | $0.03 |
| **net** | **−$10.23** |

The loss was concentrated in a single account and, within it, a single 401-request session
(−$16.86). Every other tenant sat between $0.00 and +$2.38.

**A dashboard tile was reporting this figure in green.** It had one threshold step, at
`null`. That is fixed: a negative net now renders red.

## 2. Why the deterministic components looked weak

They are not weak. They are aimed at the 9% of the bill we are allowed to touch.

**85.8% of a real Claude Code request is frozen by cache safety.** Measured on 13 fresh
requests through a local proxy: `tokens_before` 557,445, `attempted_tokens` 79,323 (14.2%),
`frozen_tokens` 478,122. Deployment-wide the figure is 45.95% frozen, and of the 54%
attempted, 1.85% removed.

The freeze is correct. Rewriting content that sits inside a live cached prefix forces a
cache re-write of everything after the edit at 1.25× the fresh rate. Checked arithmetically:
re-anchoring a 150k-token prefix costs ~$0.71 against ~$0.0076/turn of saving, so it needs
~93 further turns to break even. The freeze stays.

And 90.74% of all input tokens are billed as cache reads at $0.342/MTok — 12.6× cheaper
than a cache write. A component that removes a token from a warm cached prefix is competing
against the cheapest tier the provider sells.

Two things that are NOT the reason, both checked and discarded:

- **`skip_file_reads` in AUTO mode.** Fires 23 times in the whole corpus. It sits behind
  `below_output_floor` and `cached_prefix` in the gate order, which have already removed
  every candidate it would have seen. Not a lever.
- **A broken gate.** Every gate histogram says "nothing matched", not "declined":
  `no_filter_match`, `no_earlier_identical_output`, `no_obvious_noise`, `not_json_shaped`.
  Zero component errors across 14,262 requests.

Measured by the metric the sibling projects use — reduction of the content actually
filtered — our components are competitive: `failed_run` 88.9%, `dedup` 83.3%,
`cmdfilter` 37.9%, `extract` 17.8%, 34.7% overall across 22.4M tokens.

## 3. How we measure against how the neighbours measure

The same 8.51M removed tokens, valued four ways:

| convention | value |
|---|---|
| gross tokens × fresh input rate | $32.35 |
| unique tokens × fresh rate | $2.48 |
| **ours — each token at the rate it was actually billed** | **$6.96** |
| our own LLM spend to achieve it | $17.22 |

Adopting a gross-tokens-at-the-fresh-rate convention would multiply our published number by
4.65× with no change to the code. We do not do that, and we should not start. Two specific
traps that convention hides: re-sent content re-counted at 1.0× when its true counterfactual
is 0.1×, and an unweighted mean of per-request percentages with zero-saving requests
excluded from the denominator.

One neighbour has a genuine **architectural** advantage that is not a measurement artifact:
it filters through a pre-tool hook, so reduced content never enters the transcript, is never
cached and never re-sent. A proxy rewriting an already-cached prefix cannot reach that
position by compacting better. If large savings on this traffic are the goal, that is a hook,
not a component.

## 4. The defects found, and what changed

### `extract_llm` made calls that could not succeed — 92% syntax rejection

On real traffic, `claude-haiku-4-5` returns a plausible filter program on essentially every
call, and the sandbox threw 12 of 13 away. The model writes **Python**, not Starlark:
`any(k in ln for k in ids)` is a generator expression, which Starlark does not have.

Worse, this was undiagnosable. `RunExtractionDetail` collapsed a syntax rejection, a
transport error and "the model never replied" into one string, `"no usable program or
reply"` — three causes with opposite fixes, reported identically.

Fixed in three parts:

- The sandbox rejection reason now escapes to the call record (`program rejected: …`,
  `model call failed: …`, `OUTPUT was not a string`).
- The prompt contract now states what Starlark is not, naming the constructs models actually
  reach for: generator expressions, f-strings, `while`, `try/except`, set literals,
  `sorted(key=…)` over a mutable closure.
- Regression tests pin both the reason surfacing and the contract text.

**Measured, same real 33,932-character file read, three runs each:** before, 0/3 produced
output. After, 3/3, at 56%, 83% and 55% reduction.

### The economic gate under-priced its own calls by 21–31×

`callCost` estimated the prompt as `preamble(1463) + min(candidate, 5000) + 200` ≤ 6,663
tokens. Under `context: full` — which every cold sweep uses — the rendered transcript IS the
prompt. Measured on production: five calls on ONE request each sent ~138,000 prompt tokens.
For a 3,433-token candidate the gate compared an expected saving of $0.0077 against an
estimated cost of $0.0046 and allowed; the call cost $0.1422 and removed 0 tokens.

The real figure was already computed two lines from the gate, as `promptOverhead`, and
thrown away. The gate now uses it.

Related and separately damning: **94% of that account's extraction spend ($16.38 of $17.48)
was made against the gate's own written advisory that it would lose money.** `fire_on: size`
demotes the gate to advisory and leaves the per-turn caps as the only brake. The gate was
right; it was overridden by configuration.

### Our own extraction calls were not prompt-cached

Five calls on one request, `cache_read = 0` and `cache_write = 0` on all five: the same
~138k transcript sent fresh five times. The conversation context is identical across a
request's candidates, but it sat in the user message, which is not cacheable.

It is now a trailing **system** block, inside the prefix the client marks — but only when the
request will make more than one call. That condition is the whole design: a cache write costs
1.25× fresh, so paying it for a single call is a 25% loss with nothing to read it back.

Verified against the live gateway: first call `cache_creation_input_tokens: 30007`, repeat
calls `cache_read_input_tokens: 30007, cache_creation: 0`. On a five-call request that is
$0.114 → $0.038, a **67% reduction**. It also lifts the prefix over the model's minimum
cacheable size — the invariant preamble alone is ~1,463 tokens, below `claude-haiku-4-5`'s
4,096 floor, so on haiku nothing was being cached at all.

### `format` and `toon` were blind to 89% of the large-JSON mass

Claude Code tool results arrive wrapped in a tool-runner envelope:

```json
{"ok": true, "exit_code": 0, "stdout": "{\n  \"total\": 50, … \"tasks\": [ {…} ×50 ]}"}
```

`format` parsed the outer envelope, re-encoded it, and saved 9 tokens of 6,459 (0.1%).
`toon` saw the outer object, concluded `not_uniform_object_array`, and never descended. The
uniform record array it was built for is one level down, inside a JSON-**escaped string**.
This single fact explains `format`'s `not_json_shaped` at 98.4% and `toon`'s
`not_uniform_object_array` at 72.8%.

Of the large low-reduction JSON blobs, 673/673 carry their payload in a `stdout` string, and
537 of them (2,098,762 tokens, 89% of that mass) contain a repeated-record array.

Both components now descend one string level (toon two, because the array is a field of a
wrapper object inside the string). Measured on the envelope fixture: toon 1,784 → 483 tokens
(**72.9%**), format 1,784 → 872 (**51.1%**) — both previously zero.

### The freeze survives cache expiry — found, deliberately NOT changed

`Ctx.TailOnly()` reads `CacheAware` and `MaxCachedIdx` and never reads `ColdCache`. So on a
turn whose cache has provably expired we still freeze 92.6% of the transcript to protect a
cache that is gone. That slice is 3.4% of requests and **21% of all spend** ($434 over two
days), earning $0.43.

`ColdCache`'s own doc comment shows this was a deliberate blast-radius decision, not an
oversight, and it agrees the rewrite would be safe. Flipping it silently would change what
`mask`, `failed_run` and `collapse` do on live traffic for deployments that asked for none of
it. So it stays gated, and the honest note is recorded here: **this is the largest single
unclaimed saving on the page**, and it wants an opt-in switch and an A/B, not a one-line flip.

### `cachesplit` looked dead because one counter was missing

`cg_component_runs_total` emitted `ran`, `acted` and `reverted`. `cachesplit` is
Mutated-never-Acted **by design** — the split removes no content tokens, it moves them out of
the hashed prefix. So the "hit rate" panel computed `acted/ran` = 0% and painted red the one
component with a measured −34.1% cost effect. `mutated` and `discarded` are now exported and
the panel counts activity, not just action.

`cachesplit`'s $0.03 is, separately, honest and genuinely small: 1,799 mutations, credit
granted on 3 requests. The credit conditions are strict on purpose. What was missing was any
way to tell "ran 53 times and was honestly refused credit" from "never ran".

### Gate reasons were invisible

`acted: 0` is the one number a diagnosis cannot use — it cannot distinguish a component with
nothing to do from one whose guard is misfiring. The per-component gate histogram existed
in-process and in the database and was exported nowhere; the one Grafana panel that would
have shown it hard-errored with `maximum of series (500) reached`, because the log line packs
the counts into the label value. Now exported as
`cg_component_gate_declines_total{component,gate}` and the panel reads Prometheus.

## 5. What the dashboard now says instead of a bare cost

The complaint that started this pass: users saw what `extract_llm` cost and concluded the
product was worthless. They were reading a real number with no counterpart.

Per component, per request, valued at the tier the request actually paid:

```
saved_usd(c,r) = u·CacheWrite + (g − u)·tier(r)
  g = saved_gross, u = min(saved_unique, g)
  tier(r) = CacheRead   if cache_read > 0                            (warm: replay from cache)
          = CacheWrite  if cache_write > 0 && cache_write >= fresh    (cold/TTL: prompt re-billed)
          = Input       otherwise                                     (non-caching backend)
net_usd(c) = Σ_r saved_usd(c,r) − Σ_r llm_cost_usd(c,r)
```

The `Σ` over turns **is** the amortization — realized turn by turn as the frozen reduction
replays, not projected. It reconciles against the stored request-level total to 0.9% on
production data and exactly on fixtures, and that identity is now a test.

One correction to the intuition that motivated this: the first request after a TTL expiry is
**not** a wash. A cache miss re-bills the whole prompt in proportion to transcript size, so a
token removed on a TTL-expired turn saves the cache-write rate — **12.5× what the same token
saves on a warm turn.** The cold turn is the single most valuable place to have already
compacted, not a break-even. The code had this right; the verbal argument had it backwards.

Also now visible, and deliberately **not** netted: on transcripts over 60k tokens, a turn
following one of our own mutations is ~3× more likely to take a `prefix_change` cache miss,
at an excess of roughly $24 (+$39 in the largest band) over this window. That is larger than
every saving on the dashboard. It is observational — mutation is not randomly assigned — so
it ships as a labelled diagnostic, not a subtraction, until an A/B settles it.

## 6. Left undone, on purpose

- **`skeleton` stays compiled out** behind `//go:build cg_skeleton`. Skeletonizing a file the
  agent is about to edit is a correctness risk, not a cost risk. Evaluated locally only.
- **`extract_llm` stays in the pipeline.** Disabling it is worth +$17.22 over two days and is
  the largest single number available, but it is a deletion, not a fix; the four defects above
  are the fix.
- **The 1-hour cache tier is untested.** `ttl_expiry` costs $434 over two days, 21% of spend.
  Asking for `cache_control.ttl: "1h"` costs 2× on writes instead of 1.25×, but only on the
  writes that happen. That is arithmetic on a $434 line against a $6.96 line, and it wants one
  tenant A/B'd rather than a global change.
