# Where cg's saving differs from the expected figure

**Status:** report, generated from commit `5accdd46` of branch `docs/context-operations-spec`.
**Reproduce:** `go test ./dash/ -run TestCasesAgreeWithCurrentLogic -v` from the repo root. The root
module and `counterfactual/` must be linked by `go.work`.

## 1. What this report compares

context-guru (**cg**) edits LLM requests. It shortens tool outputs, removes unused tool
declarations and replaces old messages with a summary. Each edit makes the request cheaper, and cg
reports how much money that saved. The figure comes from `dash.Event.Price` in `dash/event.go`.

The `counterfactual` library (the "spec") computes the same saving by one shared rule. The rule is
the **counterfactual**: *if this change had never been made, how would the provider have billed these
tokens on this request?* Where the spec and cg disagree, one of them is wrong, or they answer
different questions. This report groups the disagreements by the cause in cg's logic.

**How the two sides are produced**

- **Expected figure.** Each case in `counterfactual/opscases/cases/` is a sequence of requests. The
  expected tokens were written by hand from the spec's rules. Each case passes two checks: billed
  bill minus no-change bill equals tokens saved minus tokens caused, and each dollar total matches
  the document it came from. This run did **not** call `counterfactual.Compute`. So "expected" means
  "what the spec's rules give", not a measured result.
- **cg's figure.** The test calls the real `dash.Event.Price` for every request of every case. The
  only code written on that side is the adapter `Play` in `dash/counterfactual_play_test.go`. It
  turns a case into a `dash.Event`. It treats the first request of a change as a "unique" removal
  and later requests as "replays".

**Result.** There are **59 cases**. **33 agree** with cg to within 1 µ$ (rounding). **26 differ**.
The test passes because each known difference is listed in
`counterfactual/opscases/differences.json`: 18 as `differs` and 8 as `notCovered`. A listed case that
starts to agree fails the test, so the list cannot go stale. The previous report had 9 cases and 5
differences. The set grew because the case library grew, not because cg changed.

## 2. Terms

| Term | Meaning |
|---|---|
| Tier | The rate a token is billed at: **input** (fresh), **cache write** (1.25 times input on Anthropic), **cache read** (0.1 times input). |
| Hot / cold | Hot: the provider still holds the cached prefix, so it reads it. Cold: the entry expired, so the prefix is written again. |
| Prefix, breakpoint | The provider caches the start of the prompt (tools, then system, then messages) up to a `cache_control` breakpoint. |
| B\* | The end of the prefix the provider would have read on this request if no edit had happened. |
| Saved / caused | Tokens the provider did not bill because of an edit, and tokens it billed because of an edit. |
| Break | An edit changes the prompt inside the cached prefix. The provider cannot read from the first changed position onward and writes it again. It is a **cost** of the edit. |
| µ$ | Micro-dollar, $0.000001. |

**Rates in most cases** (per million tokens): input $3.00, cache write $3.75, cache read $0.30,
output $15.00. Some cases use a second model with its own rates, given in the case file. These are
test rates, not production rates.

**The two pricing rules.** The spec prices a removed token at the tier it would have had at its
*position*: before B\* a read, after B\* and up to the last breakpoint a write, after the last
breakpoint input. cg prices it by *novelty*. The first time content is removed it is a **write**
(`uniqueRate`, `dash/event.go:894`). On later requests it is the tier this request paid
(`repeatRate`, `dash/event.go:1163`). cg builds the no-change bill by adding the priced removed
tokens to the billed cost (`baselineDeltaUSD`, `dash/event.go:1102`). It never re-bills the client
body. Causes 1 to 3 all follow from this design.

## 3. Summary of root causes

| # | Root cause in cg | Cases | Spec total − cg total (µ$, sum of case gaps) | Kind |
|---|---|---:|---:|---|
| 1 | No cache-break charge: an edit inside the cached prefix is never billed to the edit | 9 | 644,501 | cg defect |
| 2 | The rate follows novelty, not position or usage | 3 | 29,475 | cg defect |
| 3 | Growth is clamped to zero and an ended change is still credited | 3 | 38,400 | cg defect |
| 4 | cg has no concept of a setting change | 4 | 378,350 | different question |
| 5 | The keep-alive credit floor is wrong, or the adapter hides it | 2 | 72,657 | cg defect or adapter, unverified |
| 6 | Credits that cg books outside the per-component figure are not fed by the adapter | 4 | 27,450 | adapter, not evidence |
| 7 | No check for an edit that no operation reports | 1 | 1,500 | missing check |

Gaps are the absolute difference of each case's thread total (section 10 lists every case). Causes
1 and 2 overlap in six cases. Each case is counted once, under its main cause.

---

## 4. Cause 1: no cache-break charge

**The logic.** `Event.Price` has no term for tokens that moved from read to write because an edit
changed the prompt above them. The extra write cost is already inside the billed figure. cg counts
it in the bill and never charges it to the edit. So the edit looks cheaper than it is, and often
looks like a gain when it is a loss.

cg knows this cost elsewhere. `components/offload/prefix_econ.go:14-20` states the rule
(`S × T > 11.5 × W`) that decides whether to break the prefix. The decision uses the 11.5 factor.
The saving reported afterward does not.

| Case | Spec µ$ | cg µ$ | Gap µ$ | Note |
|---|---:|---:|---:|---|
| `rewrite+spend/summary/break-then-payback/5m` | 16,750 | 28,825 | 12,075 | Break only. 3,500 tokens move read to write (500 summary + 3,000 tail). cg already prices a summarizer's removal as a read, so the gap is exactly the break: 3,500 × ($3.75 − $0.30). |
| `rewrite/coref-break-inside-prefix/hot/5m` | 33,524 | 167,935 | 134,411 | Cause 2 adds to it. `coref` cuts a 20,000-token output inside the cached prefix. The spec books a loss, cg books +74,813 µ$ for that edit on turn 7. |
| `rewrite/break-then-payback-turns/hot/5m` | 76,920 | 214,920 | 138,000 | Same sign error as `coref`. Later hot turns and the cold turn agree. |
| `rewrite/searchfold-inside-prefix/hot/5m` | −49,275 | 9,375 | 58,650 | The 14,500-token break is not charged. Issue 496: the dashboard showed a hit. |
| `rewrite/replay-at-new-position/hot/5m` | 10,050 | 44,550 | 34,500 | A second occurrence is compacted at depth. |
| `rewrite/two-spans-two-tiers/hot/5m` | −43,275 | 32,625 | 75,900 | One read span and one write span. cg uses one rate and no break. |
| `rewrite/toolschema-mid-session/break-then-saving/5m` | −81,600 | 8,100 | 89,700 | Trimming a cached tool declaration breaks the whole prefix once. cg books +7,500 µ$ on that turn where the spec books −82,200 µ$. |
| `rewrite/decision-flips-inside-prefix/hot/5m` | −26,805 | 7,695 | 34,500 | Request 2 breaks the cache. Request 4 returns the original and the spec charges a 6,000-token break to the ended change (design open question 12). |
| `rewrite/expand-first-injection/break-once/5m` | −66,765 | 0 | 66,765 | An injected tool declaration changes `tools` and breaks the whole prefix once (−66,675 µ$), then costs −90 µ$ per turn. cg books neither. Also cause 3. |

**Worked example, `summary/break-then-payback`.** Request A: the provider reads 8,000 tokens (tools
+ system) and writes 4,000. With no summary it would read 21,000 and write 500. The no-change bill
is 8,175 µ$. The bill with the summary is 17,400 µ$. cg computes the no-change bill as the billed
17,400 µ$ plus the 9,500 removed tokens at the read rate (2,850 µ$) = 20,250 µ$. The true
no-change bill is 8,175 µ$. The 12,075 µ$ gap is the break.

**What was not verified.** `chargeRowCosts` and `causedWriteUSD` (`dash/compactepisode.go`,
`:727`, `:801`) charge the whole cache write of a compaction turn to the summary, as a stated upper
bound. This run did not call that code. Reading it, the charge would be 4,000 × $3.75 = 15,000 µ$
for request A, against the spec's 12,075 µ$. So the episode view may be close but not equal. It is
unverified which view a user sees. It is also unverified whether the live pipeline would choose
`coref` on a prompt like this one. If it would not, the `coref` cases are theoretical.

---

## 5. Cause 2: the rate follows novelty, not position or usage

**The logic.** `uniqueRate` returns `CacheWrite` for any first removal that is not a summarizer.
It does not look at where the content sits (inside the cached prefix, after B\*, after the last
breakpoint, or with no breakpoint) or at what the usage block shows.

| Case | Spec µ$ | cg µ$ | Gap µ$ | What the spec does |
|---|---:|---:|---:|---|
| `boundary/state-lost-reads-cache` | 1,650 | 20,625 | 18,975 | With no history, the usage still shows 18,500 tokens read. A read in the usage is evidence. The 5,500 replaced tokens are a read ($0.30), not a write ($3.75). |
| `rewrite/after-last-breakpoint/input/5m` | 21,000 | 26,250 | 5,250 | Tokens after the last client breakpoint are billed as input ($3.00), not write. |
| `rewrite/no-breakpoint/input/5m` | 21,000 | 26,250 | 5,250 | With no breakpoint in the body every token is input. |

The six position cases in section 4 also carry this error. It is not separate money there. Fixing
the break without fixing the rate would still leave `coref` pricing 19,950 tokens at the write
rate. Fixing the rate alone would still leave the break uncharged.

**Open point in the spec.** The rule for B\* (design §5.1) snaps to the client's own breakpoints. A
literal reading puts B\* at the end of `system` for the `coref` case, and then the removed tokens
are writes, which is cg's answer with no break. The earlier four-model review (section 9) concluded
that B\* is the end of the longest prefix the request's own breakpoints could have hit, which is
message 11. The spec text should say this. Until it does, the `coref` expected figure depends on an
undecided rule.

---

## 6. Cause 3: growth is clamped to zero and an ended change is still credited

**The logic.** `ComponentSavedUSD` clamps a negative gross to zero (`dash/event.go:849-852`). A
change that *adds* tokens has no cost in cg. `Event.Saved()` (`dash/event.go:557`) works on the
whole request, so the request total can still net correctly. The per-change line does not. Cg also
has no term for a change that ends: it keeps crediting the change that is no longer applied.

| Case | Spec µ$ | cg µ$ | Gap µ$ | Note |
|---|---:|---:|---:|---|
| `rewrite/four-operations/hot/5m` | 81,879 | 81,879 | 0 | Totals agree. Only the `expand` line differs: the spec books −90 µ$ per hot turn, cg books 0. |
| `rewrite/fixed-restore-cost/hot/5m` | 525 | 18,225 | 17,700 | A restore grows the body. The spec books the growth as a cost. cg books 0 for it and credits the `mask` in full every turn (issue 481). |
| `rewrite/restore-in-place/hot/5m` | −2,475 | 18,225 | 20,700 | The original goes back and the mask ends. The provider holds the marker, so 6,000 tokens move read to write. cg keeps crediting the mask (+1,350 µ$) and books no break (issue 481). |

**Unverified.** Whether the live pipeline reports `expand` as a component at all. If it does not,
cg has no row for it, and the difference is "no row", not "a row of zero". The adapter passes
`max(delta, 0)` the same way the clamp does, so the test cannot tell the two apart.

---

## 7. Cause 4: cg has no concept of a setting change

**The logic.** cg measures what *its own edits* saved. A router that changes the model, or a client
that changes `effort` or toggles web search, is not a cg edit. `AttributeCache`
(`dash/event.go:1181`) sends a model it has not seen to `cold_start` and never reports it as a bust.
cg does not read `effort`. `baselineDeltaUSD` adds only removed tokens. So cg books nothing.

| Case | Spec µ$ | cg µ$ | Gap µ$ |
|---|---:|---:|---:|
| `setting/model-change/break` | −36,800 | 0 | 36,800 |
| `setting/effort-change/break-from-messages` | −110,400 | 0 | 110,400 |
| `setting/web-search-toggle/break-from-system` | −120,750 | 0 | 120,750 |
| `setting/nil-position/penalty-unknown` | −110,400 | 0 | 110,400 |

Four related cases agree because they have no break to price: `setting/effort-change/read-intact-no-break`
and others.

**This is not a defect in cg.** It is a different question. The difference matters only if the
product must show a user that a setting change cost them a cache. All four are marked `notCovered`.

**Open and unverified.**

- Which rates price a model-change break. The case uses the small model's rates (−36,800 µ$). At
  the large model's rates it is −138,000 µ$. Against "stayed on the large model" the net is
  −25,250 µ$, which mixes the break with the rate difference. This is design open question 10.
- That a real model renders `effort` ahead of the messages only. This comes from
  `cached-prefix.md`, not from a test. Public documentation establishes only that a change to
  thinking parameters breaks the cache.
- `setting/nil-position/penalty-unknown` assumes a setting with no position falls back to the read
  the usage shows, plus a `penalty_unknown` entry. cg has no unpriced list.

---

## 8. Causes 5 to 7: smaller or weaker differences

### Cause 5: keep-alive credit floor (2 cases, unverified)

`keepaliveSavedUSD` credits a ping only when the gap exceeds `keepaliveCreditFloorMs`
(`dash/event.go:1069`).

| Case | Spec µ$ | cg µ$ | Gap µ$ |
|---|---:|---:|---:|
| `keepalive/ping-on-1h-entry/no-rescue/1h` | −3,045 | 31,455 | 34,500 |
| `keepalive/ping/minimum-lifetime/no-rescue` | 20,694 | 58,851 | 38,157 |

- **1h entry.** The spec measures the gap against the TTL of the breakpoint that *wrote* the entry
  (1h), so a 9-minute return is no rescue. cg takes the TTL the *returning* request asked for. The
  adapter does not pass `CacheTTL`, so the test ran with the 5-minute default. This run did not
  measure a real 1h session.
- **Minimum lifetime.** For GPT-5.6 and later the floor is OpenAI's documented *minimum* lifetime
  (30 minutes). A minimum proves no expiry, so the spec gives no rescue. The comment on
  `providerCacheTTLMs` says OpenAI's minimum must not be used as proof of expiry, yet
  `keepaliveCreditFloorMs` uses it as the floor. This looks like a real inconsistency. The adapter
  leaves `Provider` unset, so the test fell back to the 5-minute floor and did not run that branch.
  **Unverified**: set `Provider` in `Play` to confirm.

### Cause 6: credits booked outside the per-component figure (4 cases, adapter)

cg books `CachesplitSavedUSD` and the head-TTL credit in their own columns. `Play` does not feed
them, so cg shows 0.

| Case | Spec µ$ | cg µ$ |
|---|---:|---:|
| `split/delivered/stable-half-read` | 10,350 | 0 |
| `ttl/head-1h/never-read` | −6,750 | 0 |
| `ttl/head-1h/read-after-3-minutes` | −6,750 | 0 |
| `ttl/head-1h/read-after-20-minutes` | 3,600 | 0 |

These cases are **not evidence about** cg's cachesplit or head-TTL logic. They show what the
adapter does not cover.

### Cause 7: no check for an unreported edit (1 case)

`unpriced/unknown-kind-and-unreported-edit`: the spec (§6.5 checks 1 and 6) refuses to price a body
edit that no operation reports, and an unknown kind is left unpriced. cg has no such check and
books 1,500 µ$ for the size gap between the client and the forwarded body. The mechanism was not
traced.

---

## 9. Cases that agree with cg

33 of 59 cases agree. They are **not** evidence of a problem. For a plain removal of new content,
a removal before B\* read when hot and written when cold, or a keep-alive ping and its rescue, the
two rules give the same answer.

Two of them differ by 1 µ$ and count as agreeing (rounding):
`keepalive/ping-timeout-unknown-result/5m` and
`rewrite+spend/extract-llm-sweep/fallback-ask-fresh-input/5m`.

Examples: single rewrite, hot (26,250 µ$); rewrite replayed hot, hot, cold (54,600 µ$); tool
removal with three spans (62,400 µ$); keep-alive ping and the request it rescues (31,455 µ$);
four operations at the request level (81,879 µ$); three refused-spend cases; five split cases that
have no effect; session isolation.

## 10. Every case, thread total in µ$

Cases that agree are omitted. See section 9.

| Case | Cause | Spec | cg | Gap |
|---|---:|---:|---:|---:|
| boundary/state-lost-reads-cache | 2 | 1,650 | 20,625 | 18,975 |
| rewrite/after-last-breakpoint/input/5m | 2 | 21,000 | 26,250 | 5,250 |
| rewrite/no-breakpoint/input/5m | 2 | 21,000 | 26,250 | 5,250 |
| rewrite+spend/summary/break-then-payback/5m | 1 | 16,750 | 28,825 | 12,075 |
| rewrite/break-then-payback-turns/hot/5m | 1 | 76,920 | 214,920 | 138,000 |
| rewrite/coref-break-inside-prefix/hot/5m | 1 | 33,524 | 167,935 | 134,411 |
| rewrite/decision-flips-inside-prefix/hot/5m | 1 | −26,805 | 7,695 | 34,500 |
| rewrite/expand-first-injection/break-once/5m | 1 | −66,765 | 0 | 66,765 |
| rewrite/replay-at-new-position/hot/5m | 1 | 10,050 | 44,550 | 34,500 |
| rewrite/searchfold-inside-prefix/hot/5m | 1 | −49,275 | 9,375 | 58,650 |
| rewrite/toolschema-mid-session/break-then-saving/5m | 1 | −81,600 | 8,100 | 89,700 |
| rewrite/two-spans-two-tiers/hot/5m | 1 | −43,275 | 32,625 | 75,900 |
| rewrite/four-operations/hot/5m | 3 | 81,879 | 81,879 | 0 (line only) |
| rewrite/fixed-restore-cost/hot/5m | 3 | 525 | 18,225 | 17,700 |
| rewrite/restore-in-place/hot/5m | 3 | −2,475 | 18,225 | 20,700 |
| setting/model-change/break | 4 | −36,800 | 0 | 36,800 |
| setting/effort-change/break-from-messages | 4 | −110,400 | 0 | 110,400 |
| setting/web-search-toggle/break-from-system | 4 | −120,750 | 0 | 120,750 |
| setting/nil-position/penalty-unknown | 4 | −110,400 | 0 | 110,400 |
| keepalive/ping-on-1h-entry/no-rescue/1h | 5 | −3,045 | 31,455 | 34,500 |
| keepalive/ping/minimum-lifetime/no-rescue | 5 | 20,694 | 58,851 | 38,157 |
| split/delivered/stable-half-read | 6 | 10,350 | 0 | −10,350 |
| ttl/head-1h/never-read | 6 | −6,750 | 0 | 6,750 |
| ttl/head-1h/read-after-3-minutes | 6 | −6,750 | 0 | 6,750 |
| ttl/head-1h/read-after-20-minutes | 6 | 3,600 | 0 | −3,600 |
| unpriced/unknown-kind-and-unreported-edit | 7 | 0 | 1,500 | 1,500 |

In 30 request steps the *baseline bill* also disagrees with the spec's. In 27 of them cg's no-change
bill is higher, for example 33,675 against 14,700 µ$ (`boundary/state-lost-reads-cache` step 0). In 3
it is lower (`split` and `ttl` cases, cause 6). This is the additive baseline of section 2 made
visible.

## 11. Limits of this report

- **Expected figures are by hand.** This run did not execute `counterfactual.Compute`. A wrong
  case file or a wrong rule is not caught by the conservation check, which catches arithmetic
  errors only.
- **The tests use a flat 4 bytes per token** and a fixture model with no tokenizer correction. The
  real cg factor (1.69 for Sonnet and Opus, `internal/tokens/billedfactor.go`) is not an input, and
  no case tests another ratio.
- **The adapter `Play` is not the live pipeline.** It supplies the unique-versus-replay split that
  `FromTrace` computes in production. A different split changes cg's figure for causes 1 and 2. It
  also omits `Provider`, `CacheTTL`, the cachesplit credit and the head-TTL credit (causes 5 and 6).
- **B\* is not settled in the spec** (section 5).
- **No production traffic was used.** How often any of these situations occurs is not known.
- **The line numbers in this report** match commit `5accdd46`. They moved since the previous report.

## 12. Earlier independent review (not repeated)

For the previous version of this report, four models on the LiteLLM proxy each checked the first
five cases (summary break, coref, added tool, model change, effort change) from general knowledge
and public prompt-caching documentation. They did not see the repo. The models were Opus 5.5, Sonnet
5.5, `azure/gpt-6.1-sol` (in place of a refused `aws/gpt-6-astra`) and `azure/FW-Kimi-K3` (in place
of an absent `aws/kimi-k2.5`). **They were not run again for this version.** Their verdicts are
opinions, not measurements.

| Cause here | Earlier case | Consensus |
|---|---|---|
| 1 | Summary that breaks the cache; edit inside the cached prefix | Spec right, 4 of 4 for each. Cached content would have been read, so a write-rate price has the wrong sign. |
| 3 | Added tool declaration | Spec right, 3 of 4. Totals match and cg's clamp hides a real cost. |
| 4 | Model change; effort change | Different question, 4 of 4 and 3 of 4. The effort premise is unverified. |

All four confirmed the arithmetic of the earlier totals. They also agreed on the B\* reading in
section 5. Two errors they found in the earlier text (the tail is 3,000 tokens, and `coref` repays in
about 10 more turns, not 11) are corrected here.
