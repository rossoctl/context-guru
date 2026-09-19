# `saved_usd` priced our tokens at the provider's rates — the measurement, and the correction

Closes #240.

Every savings figure this product publishes was computed by taking **our own tokenizer's count**
of the content a component removed and pricing it at **the provider's rate**. The two do not
measure the same thing, so every one of those figures was wrong. This page reports what they
were, what they are, how each error was measured, and which of the issue's own claims did not
survive measurement.

**Corpus:** the deployed service's dashboard database, snapshotted 2026-09-15 11:58 UTC —
**281,421 requests, 29,346 sessions, 19 tenants, 2026-08-17 → 2026-09-15**. The snapshot was a
read-only `sqlite3.backup()` of the live file; the service was never written to or restarted.

---

## 1. The headline, before and after

| | old | corrected | change |
|---|---|---|---|
| provider spend `SUM(cost_usd)` | $32,203.3367 | **unchanged** | — |
| context-guru's own model spend | $16.4819 | **unchanged** | — |
| `SUM(baseline_cost_usd)` | $32,306.3795 | $32,376.9608 | +$70.58 |
| **gross saved** | **$103.0428** | **$173.6241** | **+68.5%** |
| **net saved** | **$86.5609** | **$157.1422** | **+81.5%** |
| net saved, as a share of spend | 0.2688% | **0.4880%** | +0.219 pp |

Month-to-date, which is the window `/metrics` and Grafana publish:

| | old | corrected |
|---|---|---|
| `cg_saved_usd` | $53.6004 | $90.3410 |
| `cg_net_saved_usd` | $44.2255 | $80.9661 |

Spend does not move, and that is the point: `cost_usd` was never wrong. It is built from the four
token tiers the provider itself reported. Only the **counterfactual** — what the removed content
*would* have been billed — was computed from our own counts, and only that moves.

### Per component

Deduplicated (see §5.3), each row corrected by its own model's factor:

| component | old $ | corrected $ | change |
|---|---|---|---|
| searchfold | 46.8334 | **78.9319** | +68.5% |
| extract_llm | 21.4457 | **36.1510** | +68.6% |
| textclean | 12.0469 | **20.3075** | +68.6% |
| dedup | 4.6769 | **7.8821** | +68.5% |
| extract | 3.7383 | **6.3017** | +68.6% |
| format | 3.1941 | **5.3392** | +67.2% |
| cmdfilter | 2.0020 | **3.3623** | +67.9% |
| linecap | 1.2844 | **2.1651** | +68.6% |
| extract_llm_sweep | 1.0902 | **1.8377** | +68.6% |
| toon | 0.0472 | **0.0791** | +67.4% |
| cachesplit · failed_run · toolfilter | 0.0000 | 0.0000 | never acted |
| **total** | **96.3591** | **162.3574** | **+68.5%** |

The per-component shares barely move, because one factor dominates the corpus (98.4% of the
saving is on the sonnet-5/opus-5 tokenizer). **This does not make any component profitable that
was not.** `extract_llm` still spends real money on its own model calls; a 69% larger gross does
not by itself settle its verdict, and the Components tab's net column is where that is decided.

---

## 2. The defect, in plain terms

`schema.MessagesTokens` (`schema/schema.go:32`) counts a request by walking `messages` and
running `tokens.Count` over each message's **text**. Two consequences:

1. **The tokenizer is the wrong one.** `internal/tokens/tokens.go:18` loads `o200k_base` — an
   *OpenAI* BPE — and applies it to every model unconditionally, Claude included. The package
   comment is honest that it is "an accurate offline proxy"; the problem is what was then done
   with it.
2. **The scope is narrower than a bill.** No system prompt, no tool declarations, no JSON
   envelope. `BlockText` returns `""` for any block without a `text` field, and `Rewritable`
   says so outright: *"Anthropic tool_result, whose payload lives in fields MessageText never
   reads"*.

`tokens_before` and `tokens_after` come from that function. `Saved()` is their difference. And
`Event.baselineDeltaUSD` priced that difference at the provider's rates:

```
baselineDelta = unique·CacheWrite + (gross − unique)·repeatRate
```

`unique` and `gross` are ours. `CacheWrite` and `repeatRate` are the provider's. Multiplying one
by the other is the whole bug — a quantity in one unit, a price in another.

### A worked example, from a real turn

Turn from a captured Claude Code session, billed twice to establish both sides (§3):

| | our count | provider's count |
|---|---|---|
| before compaction | 26,087 | 87,613 |
| after compaction | 8,612 | 59,278 |
| **removed** | **17,475** | **28,335** |

At `aws/claude-sonnet-5`'s cache-write rate ($1.90/MTok):

```
reported:  17,475 × $1.90/MTok = $0.0332
truth:     28,335 × $1.90/MTok = $0.0538
under-report: 1 − 17,475/28,335 = 38.3%
```

The level gap on that same turn is 87,613 / 26,087 = **3.36×**, and the delta gap is **1.62×**.
Those are different numbers about different things, and conflating them is §4.

---

## 3. How the correction factor was measured

Define

```
f = (billed_before − billed_after) / (ours_before − ours_after)
```

A factor on a **difference**. Everything our counter cannot see — system prompt, tool
declarations, envelope — is present in both terms of the numerator and **cancels**. That is
what makes `f` measurable at all, and it is why the level ratio cannot stand in for it.

### 3.1 An instrument that looked right and was not

The obvious approach is `/v1/messages/count_tokens`. It gave `f = 0.9961` — our counts
apparently accurate to 0.4% — which would have refuted the issue outright.

It was wrong, and the way it is wrong is worth stating precisely, because the obvious sanity
check passes. On a **small** body `count_tokens` is exactly additive:

```
messages 1,024  +  tools 4,677  +  system 902  =  6,603   (= the full body)
```

On a **real 340 KB Claude Code body**, every variant returns the messages count alone:

```
full body (system 27 KB, tools 106 KB)                  = 51,569
tools removed                                           = 51,569
system removed                                          = 51,569
messages only                                           = 51,569
the same tools + system, with a 1-character message      = 35,221   <- so they ARE countable
```

35,221 tokens are silently dropped at exactly the sizes compaction happens at, and what survives
is a tiktoken count over `messages` — which is our own tokenizer, so it agrees with itself to
0.7%. The same body billed through `/v1/messages` returned **93,375**. That 1.8× discrepancy is
what exposed it.

The endpoint is therefore **unreliable in a size-dependent way**, which is worse than being
plainly broken: a small-body check would have certified it. The regime boundary was not
characterised and no number from this endpoint is used anywhere in this change.
`tokens.TestCountTokensIsNotABillingOracle` pins the additive property on a small body and
carries this finding so it is not rediscovered.

**First-pass correction:** an earlier draft of this page claimed the endpoint "ignores `system`
and `tools` entirely". That was inferred from the large-body probe alone and is false — the
small-body decomposition above disproves it. The defect is size-dependence, not blanket
omission.

### 3.2 The instrument used instead — pay for both arms

`apply.TestBilledPairColdReplay`. For each real captured turn: run the production pipeline, then
send **both** the before-body and the after-body to the gateway and read `usage` off each reply.

Two details carry the result:

- **Every `cache_control` mark is stripped**, so both arms bill their whole prompt as fresh
  input and each reply states the provider's count of that exact body. The first version instead
  appended a unique nonce to `system`; that does **not** work, and the test now says why — the
  cacheable *prefix* (tools, then the head of system) stayed byte-identical, so the arm hit a
  34,467-token cached prefix and its count was not a count of its body. The test asserts
  `cache_read == 0 && cache_write == 0` and fails rather than reporting a contaminated number.
- `max_tokens: 1`, because we are buying the usage block, not the answer.

Result, n=11 pairs per model:

| model | pooled f | median | range | implied under-report |
|---|---|---|---|---|
| claude-haiku-4-5 | **1.3686** | 1.5030 | 1.168 – 1.517 | 26.9% |
| claude-sonnet-5 | **1.6857** | 1.8197 | 1.519 – 1.897 | 40.7% |
| claude-opus-5 | **1.6857** | 1.8197 | 1.519 – 1.897 | 40.7% |

Sonnet 5 and Opus 5 returned **byte-identical billed deltas on every pair** (78,313 tokens both),
so they share a tokenizer and haiku-4-5 does not. That is why the factor is keyed per family and
not set from one global constant.

### 3.3 The corroborating instrument, and why a second one was needed

Those 11 pairs are successive turns of **two** session lineages — pairs 5–8 have identical
deltas — so the effective n is a handful, not 11. A single instrument at that n is not enough to
move a published figure.

`tokens.TestBilledFactorByContentClass` measures the same `f` per **content class**, on generated
text, at arbitrary n. It uses generated text deliberately: `request_content` holds 19 real
tenants' transcripts, and sending other people's source code to an external gateway to calibrate
a tokenizer is not a decision a measurement gets to make.

| content class | f (haiku-4-5) | f (sonnet-5) |
|---|---|---|
| hex_hashes | 1.0412 | 1.1468 |
| english_prose | 1.0506 | 1.5428 |
| tool_result_json | 1.1768 | 1.3271 |
| stack_trace | 1.2600 | 1.3486 |
| go_code | 1.3339 | 1.6887 |
| **grep_output** | **1.4728** | **1.7565** |

**`f` spans 1.04 to 1.76, so no single global factor is defensible** — and the class where our
tokenizer is *worst* is `grep_output`, which is exactly what `searchfold` removes, and
`searchfold` is 52% of all savings.

Weighting these per-class factors by this corpus's own component mix:

| model | end-to-end (§3.2) | class-weighted by composition | gap |
|---|---|---|---|
| haiku-4-5 | 1.3686 | 1.3150 | 4.1% |
| sonnet-5 / opus-5 | 1.6857 | 1.5774 | 6.9% |

**Two instruments with no shared machinery agree to within 4–7%.** The shipped constants are the
end-to-end figures, because that instrument bills real bodies through the real pipeline with
content mixed as it actually occurs; the class-weighted figures are the cross-check.

### 3.4 The pre-registered kill criterion, and what it says

Stated before the measurement: *if `f`'s interval spans 1.0, the direction is not established
and no headline moves.* It does not come close — **all 33 measured pairs across three models, and
all 12 content-class measurements, returned f > 1.0**. The direction is settled. The *magnitude*
rests on a handful of independent lineages and is stated as "roughly this", not to four
significant figures, in `tokens.BilledDeltaFactor`'s own doc comment.

---

## 4. Where issue #240 was wrong

The issue is right that the defect is real, and its arithmetic is exact. Three things in it do
not survive measurement.

### 4.1 The magnitude understated its own finding

The issue reports **12.4%** (f = 1.1416). Measured, it is **26.9%** on haiku and **40.7%** on the
tokenizer carrying 98% of this corpus. The issue's figure is low because it is one turn, and a
**cross-turn** one: 149,363 is turn 15's cache-write used as turn 17's counterfactual, while
`ours` grew 105,155 → 105,367 between them.

### 4.2 "Divergence narrows to 1.1–1.5× on large transcripts" is false

The issue argues the 3.38× `EstimatorDivergence` overstates the gap because it is dominated by
small transcripts, and that on large ones it "narrows to roughly 1.1–1.5×". Measured on 281,421
rows — ratio of provider-billed prompt to our `tokens_after`, pooled:

| transcript size | compacted | untouched |
|---|---|---|
| 10–50k | 3.472× | 3.159× |
| 50–100k | 3.349× | 2.302× |
| 100–200k | **3.711×** | 1.912× |
| >200k | **3.263×** | 1.744× |

On the compacted population it does not narrow at any size above 10k. Untouched rows do converge
(1.74× past 200k); compacted rows do not. Median fixed overhead is ~34k tokens, which cannot
account for a 3.7× ratio on a 150k prompt.

### 4.3 But that 3.3× is **not** the correction factor either

This is the trap the issue fell into and it is easy to fall into twice. Those ratios are of
**levels**. A level carries system prompt, tool declarations and envelope — none of which any
component removes. The delta factor for the same traffic is 1.37–1.69, not 3.3. Quoting the level
ratio as a correction would over-report by roughly 2×.

Both are now published side by side (`estimator_divergence` and
`estimator_divergence_compacted`) precisely so neither reads as the other. The old one was
computed on `tokens_before = tokens_after` — rows where *nothing was compacted* — which is the
**strict complement** of the rows `saved_usd` is nonzero on.

### 4.4 A correction to this PR's own review

The review comment on #240 asserts that `dash/declcredit.go`'s `FilterUSD` is a fix precedent,
"priced from a MEASURED billed count". It is not. `filtered_decl_tokens` traces to
`tokens.Count(t.Raw)` at `apply/toolfilter.go:184` — the same o200k estimator. What is measured
about `FilterUSD` is the *tier*, never the *quantity*. **There is no billed-count precedent
anywhere in this repo**, and anyone looking for one to copy will not find it.

---

## 5. The other defects this turned up

### 5.1 A fabricated cache-write premium on every non-Anthropic model

`modelinfo.go:280` and `table.go:118` filled a missing cache-write rate with `Input × 1.25`.
That multiple is Anthropic's published 5-minute premium. **No other family charges a per-token
premium to create a cache entry.** Every OpenAI and Gemini row therefore had its *savings*
priced 25% above anything that could have been billed.

Note the asymmetry, because it kills the argument that let this sit: `cost_usd` was unaffected
(those providers report `cache_write = 0`, so the rate multiplies nothing), while
`baselineDeltaUSD` applies `CacheWrite` to **our** saved-token count regardless of what the
request billed. So the fabricated premium reached the savings figure on traffic that could never
have paid it.

Fixed by `modelinfo.CacheWriteFracFor`: 1.25 for the Anthropic family, 1.0 otherwise, including
for unknown families — for a savings figure, the direction that does not invent value is the safe
one.

**Live impact on this corpus: at most $0.15.** OpenAI-dialect traffic is 27,974 requests but
carries only **$0.7733 of the $103.04** baseline delta. This is a correctness fix, not a headline
mover, and it is reported that way. Its importance is that it is an **over**-report, which is why
"the error is always conservative" was never a safe thing to believe. `TestCacheWritePremiumIsPerFamily`
pins **both** directions; a one-sided test is how that belief survived.

### 5.2 An over-specified test that encoded the defect

`TestLiteLLMPrices` asserted `read < input < write` for *every* model with filled cache tiers —
i.e. it *required* a creation premium everywhere. Its stated intent ("a cached request must not
be priced as FREE") is preserved; the universal-premium assertion is replaced with a per-family
one.

### 5.3 27,564 duplicate rows in `request_components`

The table has no primary key and no unique constraint. Raw `SUM(saved_usd)` is **$97.3106**
against a deduplicated **$96.3591** — **$0.9515, 0.99% of the total**, inflating whatever reads
the table without a `DISTINCT`.

Every duplicate is from **one day, 2026-08-22**; nothing since. So this is a historical incident,
not an ongoing leak, and **it is not fixed here.** A `UNIQUE` index cannot be created while the
duplicates exist, and de-duplicating them is a data migration that would rewrite history this
change deliberately leaves alone (§6). Recorded here with its exact cost so the next reader does
not rediscover it as a mystery discrepancy.

### 5.4 The comment that kept the defect alive

`dash/overview.go` ended its divergence note with:

> *"Dollars are unaffected: they come from the provider's own reported usage."*

True of `cost_usd`. False of `baseline_cost_usd`, which is `cost_usd + baselineDeltaUSD`. A reader
auditing exactly this was told not to. **That sentence is why the defect survived, and it is
deleted.**

### 5.5 The 1h-tier asymmetry — real, and inert

`event.go:642` adds the one-hour cache-write premium to `CostUSD` and never to the baseline delta,
so a genuine 1h row would have its cost corrected and its counterfactual left at the 5-minute
rate. **Empirically inert: `cache_write_1h = 0` on all 281,421 rows** — Bedrock grants the tier
only to the Claude 4.5 family and silently downgrades the models this service runs. Left as is,
with the gap documented, because the predicate needed to fix it properly (which row *wrote* the
entry) does not exist on the row being credited.

Separately worth an eye: 19,876 requests carry `cache_ttl = 'ephemeral_1h'` while booking 38,336
cache-write tokens and `cache_write_1h = 0`. That is the request *asking* for 1h and not getting
it — consistent with the downgrade, not evidence of a second accounting hole.

---

## 6. What was deliberately **not** done

- **No backfill.** 281,421 historical rows keep their original figures. Re-pricing them would
  rewrite measurements nobody re-measured, on a factor whose magnitude rests on a handful of
  lineages. Instead `requests.billed_token_factor` is added as an additive column whose **default
  of 0 marks a pre-fix row** — a different fact from "measured, correction 1.0" — and the
  dashboard renders a labelled tile while a window straddles the boundary. Additive with a
  default, so no schema-version bump and no history discarded.
- **No per-component billed counterfactual**, because there is no such observable. The provider
  bills per *request*; `saved_usd` is per *component*; several components act on one request; and
  `request_components` has no billed columns and cannot have any. Even per request, a billed delta
  needs a counterfactual you never observe. The worked pair in §2 exists only because we were
  willing to pay for both arms of a synthetic replay.
- **The token counts are unchanged.** The factor multiplies counterfactual *dollars* only.
  `tokens_before`, `tokens_after`, `saved_gross`, `saved_unique` are what component gates and
  triggers are evaluated against; moving those would change *what gets compacted*, which is a
  behaviour change wearing an accounting change's clothes.
- **`components/offload/extract_econ.go:132`** still contains `write = fresh * 1.25`, the same
  fabricated multiple, in `extract_llm`'s pre-call economics gate. Left alone for the reason
  above: it decides whether a call is *made*. Flagged for its own change.
- **`cachehistory.go` and `toolsuggest.go`** price `split_stable_tokens` and
  `filtered_decl_tokens` — also our own counts at provider rates, the same root cause. Not
  corrected, because `f` was measured on *removed message content* and neither of those is that:
  one is a cache-boundary offset, the other raw tool-declaration JSON. Applying a factor measured
  on one content class to another would be the extrapolation this whole exercise is about
  avoiding. Their figures are $0.0914 and $0.00 on this corpus.

---

## 7. Reproducing every number here

```sh
# The snapshot (read-only; the service is never written to or restarted)
sudo -n python3 -c 'import sqlite3;s=sqlite3.connect("file:/var/lib/context-guru/cg.db?mode=ro",uri=True);d=sqlite3.connect("cg.db");s.backup(d)'

# §1 headline
sqlite3 cg.db "select round(sum(cost_usd),4), round(sum(baseline_cost_usd),4),
  round(sum(baseline_cost_usd-cost_usd),4), round(sum(cg_llm_cost_usd),4) from requests;"

# §1 per component, DEDUPED (the table has no primary key — see §5.3)
sqlite3 cg.db "with d as (select distinct request_id, component, saved_usd from request_components)
  select component, round(sum(saved_usd),4) from d group by component order by 2 desc;"

# §4.2 level ratio by size, compacted vs untouched
sqlite3 cg.db "select case when tokens_before>tokens_after then 'compacted' else 'untouched' end pop,
  round(sum(fresh_input+cache_read+cache_write)*1.0/sum(tokens_after),3)
  from requests where token_accounting='complete' and tokens_after>100000 group by pop;"

# §3.2 f end-to-end — SPENDS MONEY (~$2/model over 11 pairs)
#   first capture real traffic through a local proxy:
LISTEN_ADDR=127.0.0.1:4100 DASHBOARD=true DASHBOARD_DB=/tmp/local.db \
  MODEL_PRICES=/etc/context-guru/prices.yaml CONTEXT_GURU_CAPTURE=/tmp/capture.jsonl \
  ANTHROPIC_UPSTREAM=$ANTHROPIC_BASE_URL ./bin/context-guru-proxy --preset agent &
#   then, with a settings.json pointing Claude Code at http://127.0.0.1:4100/anthropic:
CONTEXT_GURU_CAPTURE=/tmp/capture.jsonl CG_LIVE_URL=$ANTHROPIC_BASE_URL CG_LIVE_KEY=$TOKEN \
CG_LIVE_MODEL=claude-sonnet-5 CG_LIVE_PAIRS=11 \
  go test ./apply/ -run BilledPairColdReplay -v -timeout 30m

# §3.3 f by content class — SPENDS MONEY (~$1/model)
CG_LIVE_URL=$ANTHROPIC_BASE_URL CG_LIVE_KEY=$TOKEN CG_LIVE_MODEL=claude-sonnet-5 CG_CLASS_REPS=3 \
  go test ./internal/tokens/ -run BilledFactorByContentClass -v -timeout 20m

# §3.1 the broken instrument, if you want to see it fail
curl -s -X POST "$ANTHROPIC_BASE_URL/v1/messages/count_tokens" \
  -H "authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -H 'anthropic-version: 2023-06-01' -d @body.json          # then again with tools/system removed
```

Both live tests **skip** without their environment variables, so CI needs no fixture, no
network and no money.

---

## 8. After the redeploy

`cg_saved_usd` and `cg_net_saved_usd` are month-to-date, so they will not jump to §1's corrected
figures at restart — they will drift up as corrected rows accumulate and the pre-fix rows age out
of the window. The Overview's "Savings on the OLD arithmetic" tile reports exactly how much of
the current window is still on the old definition. When it disappears, the whole window is
corrected.
