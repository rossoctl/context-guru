# `below_trigger` is not the trigger

Production reports 7,660 requests / 380,764,158 tokens / $744.62 of spend as
`uncompressed_reason = below_trigger` — 44% of requests and 29% of spend that
context-guru "never even tried" to compact. That reads as a mis-set threshold, and the
obvious next move is to lower it.

It is not a threshold. This page is the measurement that says so, and the arithmetic for
why lowering it is a regression rather than a tuning.

Read-only analysis of `/var/lib/context-guru/cg.db` (opened `mode=ro&immutable=1` on a
copy; nothing in production was modified or restarted), plus deterministic replay of two
real corpora through `apply.BodyOpts`.

## 1. What actually sets the label

`dash/event.go`'s `uncompressedReason` picks `below_trigger` when **no component
mutated anything** and nothing was saved:

```go
acted := 0
for _, c := range e.Components {
    if c.Mutated { acted++ }
}
if acted == 0 { return ReasonBelowTrigger }
return ReasonNoSavings
```

`components.Trigger` is not consulted anywhere in that decision. And of the eight
components actually deployed (`format, toon, dedup, cmdfilter, extract, extract_llm,
failed_run, cachesplit` — `mask`, `skeleton`, `summarize`, `agentdiet`, `smartcrush` have
zero rows), exactly **one** calls `Trigger.Fires`: `extract_llm`. `extract` uses `Trigger`
only for its per-item `OutputFloor`; the other six never reference it.

So the name attributes to a threshold what is really "every reducer looked and found
nothing it could shrink". On all 7,660 requests every component in the pipeline **ran**:

| component | rows on `below_trigger` requests | mutated |
|---|---|---|
| format, toon, dedup, cmdfilter, extract, cachesplit | 7,660 | 0 |
| failed_run | 7,171 | 0 |
| extract_llm | 898 | 0 |

## 2. Why they found nothing: there is no reducible surface

100% of context-guru's mutations in production land in `tool_result` content blocks
(54,687 captured rows, every `path` matching `messages.N.content.M.content`). Split the
gated traffic by message count and the reason is immediate:

| messages | requests | tokens | $ | frozen % | attempted |
|---|---|---|---|---|---|
| 1 | 1,772 | 95,605,269 | 62.53 | 0.0 | 95,605,269 |
| 2–4 | 2,427 | 111,272,432 | 146.53 | 0.5 | 110,756,373 |
| 5–20 | 669 | 13,575,795 | 55.12 | 68.2 | 4,319,246 |
| 21–100 | 1,457 | 51,938,151 | 173.04 | 64.9 | 18,254,040 |
| >100 | 1,335 | 108,372,511 | 307.40 | 89.4 | 11,481,005 |
| **total** | **7,660** | **380,764,158** | **744.62** | | |

**4,199 requests / 206,877,701 tokens — 54.3% of the gated tokens — carry four messages
or fewer.** The largest single group is 734 `claude-cli` requests averaging 120,245
tokens with **one** message and **zero** declared tools: Claude Code's own
conversation-summarisation call, which flattens the whole transcript into a single user
text block. There is no `tool_result` in it to reduce. For comparison, of the 5,138
requests that *did* compact, three had 2–4 messages.

The remaining 45.7% is the opposite problem: long transcripts, 65–89% frozen, whose
eligible tail holds no candidate the reducers recognise.

## 3. The counterfactual: lift every floor and measure

`apply/trigger_counterfactual_test.go` replays a capture through the deployed
deterministic pipeline (arm A) and through the same pipeline with **every size floor
dropped to 1** (arm B) — which is what "the trigger lifted" means on a pipeline where no
component but `extract_llm` consults `Trigger`. Same corpus, same session, same evolving
cached-prefix boundary.

```
CG_TRIGGER_CF=1 CONTEXT_GURU_CAPTURE=/path/capture.jsonl \
  go test ./apply -run TriggerCounterfactual -v
```

### SWE-bench claude-code, 1,795 real requests, 19.1M tokens

| class | n | tok_before | attempted | rm: deployed | rm: lifted | delta |
|---|---|---|---|---|---|---|
| 1 msg | 24 | 124,219 | 124,219 | 0 | 0 | **0** |
| 2–4 msgs | 100 | 223,821 | 158,249 | 0 | 0 | **0** |
| 5–20 msgs | 376 | 1,705,498 | 1,122,736 | 5,269 | 10,923 | 5,654 |
| 21–100 msgs | 1,031 | 10,415,220 | 5,796,215 | 278,640 | 344,953 | 66,313 |
| >100 msgs | 264 | 6,670,066 | 3,183,219 | 202,140 | 269,954 | 67,814 |
| **total** | 1,795 | 19,138,824 | | | | **139,781** |

Requests rescued from `below_trigger` by lifting every floor: **0 of 24.**

Neither arm ever removed more than it attempted, which is the check that separates a
saving from cache invalidation — a component that rewrites frozen content reports a large
`removed` and produces a *larger* provider bill. The harness fails the test if any class
crosses that line. Run-to-run variation on the lifted arm is about ±150 tokens (0.1%).

### Real interactive Claude Code, 70 requests, 2.5M tokens

| class | n | tok_before | attempted | rm: deployed | rm: lifted | delta |
|---|---|---|---|---|---|---|
| 2–4 msgs | 9 | 72,937 | 57,302 | 0 | 0 | **0** |
| 5–20 msgs | 30 | 918,537 | 159,296 | 0 | 0 | **0** |
| 21–100 msgs | 31 | 1,555,110 | 42,926 | 0 | 0 | **0** |

All 70 requests are `below_trigger`, and lifting every floor to 1 removes **zero** extra
tokens and rescues **zero** requests.

The delta the lifted arm does produce lands entirely in classes that were *already*
compacting — it shaves a little more off requests that acted, it does not reach the gated
ones. Priced at the tiers actually billed, 139,781 **gross** tokens is $0.021 at
`cache_read` (0.1x) and $0.266 at `cache_write` (1.25x); against production's measured
13.1x re-send overcount that is roughly 10,700 net-new tokens, or **$0.02**.

## 4. The frozen distribution does not mean what it looks like

The frozen share is bimodal (p10 0.00, p25 0.00, p50 0.135, p75 0.995, p90 0.9996, mean
0.436; 5,302 requests fully unfrozen, 1,073 exactly 100% frozen), which invites the rule
"the lower mode is unfrozen, so acting there is free". **It is not.** `frozen_tokens =
tokens_before − attempted_tokens`, and `attempted_tokens` counts messages above
`MaxCachedIdx` — context-guru's *own* boundary, not the provider's cache state. Classify
every request by why its boundary is where it is:

| why frozen = 0 | provider cache | requests | tokens_before | cache_read | $ |
|---|---|---|---|---|---|
| first turn of session | hit | 555 | 338,772 | 822,142 | 10.94 |
| first turn of session | miss | 1,212 | 13,000,211 | 0 | 142.47 |
| **shrink (compaction reset)** | **hit** | **3,092** | **204,242,568** | **404,376,878** | **169.18** |
| shrink (compaction reset) | miss | 243 | 6,731,305 | 0 | 33.78 |
| grew but frozen = 0 | hit | 102 | 6,480,009 | 14,836,028 | 18.80 |
| grew but frozen = 0 | miss | 98 | 6,792,086 | 0 | 98.84 |

The lower mode is overwhelmingly **compaction resets that still cache-HIT**: 3,092
requests carrying 404,376,878 cache-read tokens. `modes.Boundary` resets to 0 on any
shrink, so `MaxCachedIdx` becomes −1 and every message looks mutable — while the
provider's prefix is hot. 3,075 of those requests (202,754,029 tokens) are exactly the
ones labelled `below_trigger`.

That makes the asymmetry, at the tiers actually billed. The denominator is the
`below_trigger` ∩ cache-hit ∩ `frozen_tokens = 0` set — the traffic a frozen-aware trigger
would newly open — which carries **405,279,861** cache-read tokens:

| | tokens | $ |
|---|---|---|
| remove 1% of that class | 4,052,798 @ cache_read 0.1x | **+$0.62** (sonnet-5) / +$1.54 (opus-5) |
| break the prefix match on it (read → write, +1.15x fresh) | 405,279,861 | **−$708.43** (sonnet-5) / −$1,771.07 (opus-5) |

The re-anchoring figure is the full exposure, not the expected loss: context-guru replays a
frozen decision byte-for-byte on later turns, so a rewrite that repeats identically stops
costing anything after the turn that introduced it. What it does cost is that first turn,
on a prefix the provider is currently serving from cache — and there is one such turn per
compaction, which on this traffic is 3,092 of them.

Roughly **1,000:1 against**. Opening this class up is the exact failure the net-savings
rule names: gross removal up, cache re-anchoring cost far larger.

## 5. Recommendation: change nothing, and here is the arithmetic

The only `Trigger` in the deployed configuration is `codesmart`'s
`extract_llm.trigger.min_request_tokens: 3000`. Lowering it to 0 reaches:

* **1,112 requests, 801,516 tokens, $19.59** — 0.21% of the gated tokens. Mean request
  size 721 tokens.
* `extract_llm` was in the pipeline for only 898 of them.
* At a generous 10% removal that is ~80,000 tokens, worth **$0.012** at the `cache_read`
  tier those requests are billed at.
* It would admit ~1,112 new cheap-model calls. Production's measured rate is $17.22 for
  78 calls ≈ $0.22/call, so **≈ $245 of new LLM spend** to chase $0.012.

`extract_llm`'s own economic gate is already suppressing calls for this reason
(`advisory: suppressed: cache-aware, saving below call cost`, 43 calls), and the
component is single-handedly responsible for production's −$10.14 net.

**No trigger change is recommended.** No threshold in `components/trigger.go` and no
`trigger:` setting in any preset is what declines the 380.7M gated tokens. 54.3% of them
have no `tool_result` surface at any floor; the rest sit behind a cache boundary where
acting costs ~1,000x what it saves. A frozen-aware trigger that "acts where acting is
free" was designed and then dropped: the mode it would open is measurably cache-hot, so
its measured value is $0.04 against a $708 exposure.

What the number *does* say is that the 380.7M tokens are real and are not reachable by
the reducers we have. They are reachable, if at all, by a **prefix**-level reduction —
tool definitions are 19.8% of a large interactive request and 59.2% on SWE-bench, and no
component even sees the `tools` array — and by giving the flattened
compaction transcript a class a reducer can recognise. Both are different work from
tuning a trigger.

## 6. Cross-checked on the sanctioned instrument

The same two arms, replayed through `bench/ab.sh` (which drives
`apply/sweep_capture_test.go` against a clone pinned at `d9e2f24`, so config is the only
difference between arms) on all four real-capture arms:

| capture | before | deployed removed | floors-lifted removed | attempted / frozen |
|---|---|---|---|---|
| short (5 req) | 90,186 | 0 | **0** | — |
| long (35 req) | 1,614,822 | 0 | **0** | 159,051 / 1,455,771 (90.1% frozen) |
| mixed (21 req) | 676,028 | 0 | **0** | — |
| cold (9 req, `CG_IDLE=430`, n=3) | 165,548 | 0 | **0** | — |

Zero on every arm, both arms, and — because these arms make no model call — bit-stable
across three identical cold runs, unlike the LLM path where the bench measured the sign of
the net flipping between identical runs (+$0.0097 / +$0.0026 / −$0.0086). Independently,
the bench found the shipped default removing 0 tokens on all four arms with `cached_prefix`
dominating the gate reasons: that is the cache boundary declining, not a threshold.

Methodology note for any follow-up: `apply` computes coldness only on the `Tracker` path,
so a replay without a `modes.Tracker` measures a WARM turn no matter what the injected
clock says. Both harnesses here pass a real `modes.Tracker`.

## 7. Is it the change or the corpus?

Both replay corpora are read/grep-heavy, and a trigger change that helped build-log or
JSON-heavy traffic would score ~0 on them. So the flat replay result alone does not settle
the question — but it is not what settles it. **The DB is the whole production population,
not a sample:** 14,407 requests, 12 tenants, 865M tokens. Its verdict does not depend on
corpus choice:

* 54.3% of the gated tokens carry ≤4 messages, so they hold no `tool_result` block, and
  100% of context-guru's mutations in production land in `tool_result` blocks. No threshold
  reaches content that is not there.
* The remainder sits behind a cache boundary where the measured asymmetry is ~1,000:1
  against acting.
* The one `Trigger` actually deployed can reach 0.21% of the gated tokens, for ~$245 of
  new LLM spend against $0.012 of value.

The honest summary is therefore: **the corpus limits how precisely the residual can be
measured; the population settles the direction.** No re-tuning was done against either
corpus.

### Reproducing

```bash
# counterfactual table, deterministic, no LLM calls, no cost
CG_TRIGGER_CF=1 CONTEXT_GURU_CAPTURE=/path/capture.jsonl \
  CG_TRIGGER_CF_IN_RATE=1.52 go test ./apply -run TriggerCounterfactual -v

# the pinning test: below_trigger fires with every floor at 1
go test ./apply -run BelowTriggerIsNotTheTrigger -v
```
