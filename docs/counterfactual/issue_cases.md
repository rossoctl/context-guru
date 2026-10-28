# Test cases derived from open and closed issues

**Status:** plan. None of these cases is written as a JSON file yet.
**Part of:** [`design.md`](design.md). Section references (§5.2, R45, Q12) refer to that document.
**Builds on:** [`examples.md`](examples.md) (the first eight cases) and [`more_examples.md`](more_examples.md)
(the scenario plan; IDs like B3 or C4 below point to it).
**Source:** 20 issues of `rossoctl/context-guru`: #580, #532, #567, #496, #493, #555, #175, #199, #197,
#230, #214, #232, #47, #45, #324, #696, #586, #481, #584, #501. I read each issue body. I did not read the
code they cite and I ran nothing.

**The JSON cases in `counterfactual/opscases/cases/` are the source of truth.** They were written after this
plan and reviewed by three models. Where a figure here differs from the JSON, the JSON is right. The known
differences: the searchfold case (3) uses 33,000 read and a 14,500-token break; breaks the client made (cases 4,
15 second variant, 17) move no tokens, because B\* is at or before the client's first change (§5.1, §6.2); case 6
uses two `ChangeID`s, one for each occurrence (§4.2 rule 6); case 8's three sessions share one `ChangeID`.

## 1. How the issues were used

The library prices one thing: the tokens a change saved or caused, by tier, on one request. An issue
gives a case only when its fault can be seen in that price, in a cache effect, or in a check. If the
fault is in a policy (when an operator acts), in a dashboard query or in a price table, the library
cannot show it, and no case is written.

| Result | Issues |
|---|---|
| **Case written** (17 issues, 18 cases) | #580, #532, #567, #496, #493, #555, #175, #199, #197, #214, #232, #47, #45, #696, #481, #584, #501 |
| **No case: outside the library** | #230 (approval estimator inside the econ trigger), #324 (price table lookup: the library has no dollars), #586 (dashboard readers of the stored credit) |

Reasons for the three:

- **#230** is about how the trigger learns the adjudicator's approval rate. The library sees only the
  removal that results. A trigger test in `components/` is the right place.
- **#324** is about a model id that falls to a wrong price row. The library returns tokens by tier and
  the host prices them (R1, R49), so the library cannot make this mistake. The one thing worth keeping:
  the same case file priced at two rate tables gives the same tokens (G1).
- **#586** is about several dashboard views summing the stored credit instead of the capped ledger. The
  library produces no stored credit. Case 13 below is the nearest, and it tests the library side only.

## 2. Format of a case here

Each case has: the **source issue**, the **situation** (what a request sequence looks like), the
**steps** (the calls to `Compute`), the **expected** result in tokens by tier, the **spec rule** it
tests, and a **status**.

- **ready**: the spec already decides the expected result. The case can be written as JSON now.
- **needs decision**: the expected result depends on an open question. The case says which.
- **exposes a gap**: the case is likely to fail against the spec as written. That is the reason to
  write it.

Rates are `fixture-v1`: input $3.00, cache write 5m $3.75, cache write 1h $6.00, cache read $0.30 per
million tokens. Tokens are exact; µ$ is shown only to make the conservation check readable.

---

## 3. Keep-alive

### Case 1: a ping that missed and wrote the cache restarts the clock (#580)

*ID proposal:* `keepalive/ping-miss-restarts-clock/5m`. *Related:* B2, B5. *Status:* ready.

**Situation.** The thread's entry is 20,000 tokens. The keeper pings after the 5-minute life ended, so the
ping **misses** and writes the entry again. A request comes after the life measured from the last turn
but inside the life measured from the ping.

| Step | Time | What happens |
|---|---|---|
| 0 | 0:00 | Turn 0 writes 20,000 tokens. |
| ping | 6:00 | The ping reads 0 and writes 20,000. The entry is alive again from 6:00. |
| 1 | 9:00 | Turn 1 reads 20,000 and writes 500 new tokens. 9 minutes after turn 0, 3 minutes after the ping. |

**Expected.**

- The entry gap on step 1 is measured from `EntryFacts.LastPing` (the later of `PrevPrint.At` and
  `LastPing`, §5.2). It is 3 minutes, so the phase is `hot`. No `ttl_expiry` verdict is possible.
- A **rescue** effect: 20,000 tokens, write to read, credited to the keep-alive window.
- The ping's own spend: 20,000 tokens at the write tier, a cost.
- Conservation: with the ping, 75,000 (ping write) + 6,000 (read) + 1,875 (write) = 82,875 µ$. Without
  it, the request is cold and writes 20,500 tokens: 76,875 µ$. Net **−6,000 µ$**, which is exactly one
  read of the entry. A ping that misses pays for itself at the read price only.

**Tests.** `LastPing` must move for a ping that **wrote** (`CacheWrite > 0`), not only for one that
read. A library that moves it only on `CacheRead > 0` returns `cold` for step 1.

### Case 2: a ping that timed out but did refresh the cache (#532)

*ID proposal:* `keepalive/ping-timeout-unknown-result/5m`. *Related:* B1. *Status:* needs decision.

**Situation.** The keeper sent a ping for a 199,000-token prompt. The proxy gave up after 60 seconds, so
the host has **no usage** for the ping. The provider probably billed it. The next turn comes 8 minutes
46 seconds after the last turn and reads 199,103 of 199,490 tokens.

| Step | Time | What happens |
|---|---|---|
| 0 | 0:00 | Turn 0 writes the prompt. |
| ping | 4:31 | The host sent it and got no answer. `EntryFacts.LastPing` is set. The ping has no usage. |
| 1 | 8:46 | The request reads 199,103 tokens. |

**Expected.**

- Step 1 is `hot` **by evidence** (§5.2 row 4 and R45): `CacheRead` is above 0 although the gap from the
  last turn is past the 5m life. When `LastPing` is set, the gap is measured from it, so the clock alone
  already says `hot`.
- A rescue is credited to the window **only if the ping is known to have refreshed the entry**. Here
  the only proof is the read on step 1. The decision to take: credit the rescue from the read on the
  request, and price the ping as `Unpriced` with a reason such as `ping_result_unknown`.
- The ping's cost is **not** zero in truth. The result must not show a rescue with no cost on the other
  side of it without saying so.

**Decision needed.** The design has no form for a ping whose usage is unknown. Options: (a) the host
reports a `ModelCall` with empty usage and the library marks the window `Estimated`; (b) the host
estimates the ping from the prompt size it sent; (c) the library prices the rescue and leaves the ping
`Unpriced`. This case is the test for whichever is chosen. Without it, a slow gateway shows a good
rescue with a missing cost, which is the opposite of what #532 reported for the dashboard.

---

## 4. Breaks caused by an operator

### Case 3: a component changes a cached message on a warm request (#496)

*ID proposal:* `rewrite/searchfold-inside-prefix/hot/5m`. *Related:* A1, `examples.md` example 6.
*Status:* ready.

**Situation.** The thread cached 50,000 tokens on the previous turn (tools and system 8,000, messages
42,000). Now `searchfold` shortens message 10, a 3,000-token tool output, to a 500-token form. Message 10
starts after 28,000 tokens. The client added 2,000 new tokens. The previous request was 40 seconds ago,
so the cache is hot.

**Expected (step 1).**

| Item | Tokens | Tier | µ$ |
|---|---:|---|---:|
| Client body, no change (baseline) | 50,000 read, 2,000 written | | 22,500 |
| Forwarded body, billed | 28,000 read, 21,500 written | | 89,025 |
| `searchfold` saved | 2,500 | read | +750 |
| Break caused by `searchfold` | 19,500 (the marker 500 and 19,000 after it) | read to write | −67,275 |
| **Net** | | | **−66,525** |

Check: 22,500 − 89,025 = −66,525.

**Tests.**

- The break has the `ChangeID` of `searchfold` (§5.2 row 1). #496 reports that the dashboard labelled
  7 such requests `hit`. The library's phase for the request must not be a miss with an unknown cause.
- The result must show the **break as a separate effect**, not only a lower net, so a host can show
  "this operator cost $0.067 here".

### Case 4: the same partial read with no operator that explains it (#496, hypothesis 2)

*ID proposal:* `break/partial-read-no-operator/hot/5m`. *Related:* A2, A1. *Status:* ready.

**Situation.** The same usage as case 3 (28,000 read, 21,500 written), but the thread reports **no
operation** that touches the cached part. The only operation is on the new tail.

**Expected.** The bodies differ inside the cached prefix with no `ChangeID` to claim it: a break with
**empty `ChangeID`** (R31, §5.2 row 2). It is nobody's cost and it is not hidden. It appears in
`CacheEffects` with `Caused` tokens and in no operator's total.

This is the case that separates "an operator did it" from "something else changed the bytes", which
#496 could not decide from the dashboard alone.

### Case 5: a decision flips on a cached message (#493, `dedup`)

*ID proposal:* `rewrite/decision-flips-inside-prefix/hot/5m`. *Related:* A1, D3. *Status:* ready for
steps 1 to 2; step 3 needs decision (Q12).

**Situation.** Message 5 is a copy of message 2. `dedup` is off on turn 1 and on from turn 2.

| Step | What happens |
|---|---|
| 1 | `dedup` off. The thread caches messages 0 to 6. Nothing reported. |
| 2 | `dedup` on, 30 seconds later. It replaces message 5 (2,000 tokens) with a 100-token pointer. Message 5 is inside the cached part. |
| 3 | `dedup` replays the same replacement on the next hot turn. |
| 4 | The earlier copy leaves the conversation (the agent compacts it). `dedup` stops replacing message 5, so the original returns. |

**Expected.**

- **Step 2:** a break with the `ChangeID` of `dedup`. Saved 1,900 tokens at the read tier. Caused: the
  tokens after message 5 that move from read to write, plus the pointer.
- **Step 3:** no break. The forwarded prefix is the same as on step 2, so the library reports only the
  saving of 1,900 tokens at the read tier. This is the "replay is stable" half of #493's proposal.
- **Step 4:** the content comes back. The forwarded prefix changed inside the cached part again, and
  no operator reports it, because the change **ended**. The expected result is a break with the
  `ChangeID` of the ended change or an empty one. **That is open question 12** (§4.5): the rule that
  finds the end of a change is not defined. This step is the test for it.

### Case 6: a replay that lands at depth on content the provider holds in its original form (#199)

*ID proposal:* `rewrite/replay-at-new-position/hot/5m`. *Related:* A1, `rewrite/replay/hot-hot-cold`.
*Status:* ready.

**Situation.** An operator compacted a 6,000-token output at message 3 on the turn it arrived, so the
provider holds the compacted form there. The same content appears again at message 12, was not
compacted when it arrived (the operator was throttled), and is now inside the cached prefix.

**Expected.**

- Step A (message 3, replayed): no break, saving at the read tier. The provider already holds the
  compacted form (the case the replay bypass is for).
- Step B (message 12): the operator compacts it now. The forwarded prefix differs from the cached one at
  message 12, so there is a **break** and the saving is at the read tier. The break is larger than the
  saving whenever the suffix is large.
- The result must give both as **one `ChangeID` with two spans** (C3), with the break only on the second
  span. A library that keys the replay on content alone would call both spans free.

**Why it matters.** #199 is a gate bug in the operator. The library is how a user finds out that the
gate failed: the cost appears as a break with a `ChangeID`, not as a lower saving with no explanation.

### Case 7: two operators with the same content key (#197)

*ID proposal:* `check/changeid-shared-between-operators`. *Related:* H5. *Status:* ready.

**Situation.** `extract_llm` and `extract_llm_sweep` both compact content M. Both derive their change
identity from the content hash alone. The host reports both operations with the **same `ChangeID`**
but different `Operator` names and different replacement sizes.

**Expected.** The `unique_change` check (§6.5) fails and names both operators. Pricing continues (a
failed check does not stop it). The two operations are priced separately, by their own sizes.

**Why.** #197 says the invariant "a record belongs to the component that wrote it" is not expressible by
the key. The library can at least refuse to merge two operators' savings under one ID without telling.
Variant: the same `ChangeID` with the **same** operator on two requests is the normal replay and must
**not** fail the check.

### Case 8: a model call whose removal was refused (#214)

*ID proposal:* `spend/without-rewrite/refused`. *Related:* D5. *Status:* ready.

**Situation.** `extract_llm` calls a model to shorten an output (a `spend`: 6,000 input, 400 output),
but the reserve is full, so the pipeline refuses the removal. No `rewrite` is reported for it.

**Expected.**

- The `spend` is priced: `Caused` tokens, no `Saved` anywhere.
- No break, no rescue, no cache effect: the forwarded body equals the client body.
- The request's net is **negative** by exactly the call's cost.

**The repeated version.** The same on 3 sessions at once, each paying 2 calls (#214: "the waste
multiplies by the number of sessions"). Each session's net is negative by its own calls only. No
session carries another's cost. This is also a test that the Tracker keys on the session (case 12).

### Case 9: a break that pays back over many turns (#232, `coref`)

*ID proposal:* `rewrite/break-then-payback-turns/hot/5m`. *Related:* `examples.md` example 7.
*Status:* ready.

**Situation.** `coref` removes a 20,000-token output inside the cached prefix, with 20,000 tokens after
it. The edit stays active on every later turn.

| Step | Cache | Saved | Caused | Net µ$ |
|---|---|---|---|---:|
| 1 | hot | 20,000 read | break: 20,000 read to write | 6,000 − 69,000 = **−63,000** |
| 2 to 11 | hot | 20,000 read each | none | +6,000 each |
| 12 | hot | 20,000 read | none | +6,000 |
| 13 | cold (a 20-minute gap) | 20,000 written | none | +75,000 |

**Expected.**

- Cumulative net by `ChangeID`: −63,000 after step 1; it reaches 0 after step 11 (−63,000 + 10 × 6,000
  = −3,000) and turns positive at step 12 (+3,000).
- The cold turn on step 13 credits 20,000 tokens at the **write** tier (the content would have been
  written), a single large credit.

**Why.** #232 is about the horizon `T` that the trigger uses to decide whether the break will be repaid.
The library supplies the other side: the measured payback. A host can compare its forecast with this
case's cumulative figure. The case also pins the count the report `cg-differences.md` corrected from 11
to 10.

---

## 5. Cache boundary and unknown state

### Case 10: the state is lost, and the request already reads from the cache (#47)

*ID proposal:* `boundary/state-lost-reads-cache`. *Related:* A5, H6. *Status:* ready.

**Situation.** The Tracker has no entry for the thread (a restart, an eviction, or a pin budget that was
full). The request reads 30,000 tokens and an operator shortens a message at depth.

**Expected.**

- `Boundary` is **nil**, `penalty_unknown` is in `Unpriced` (§6.4).
- The saving is **gross only**: the removed tokens are priced at the **read** tier (the extent that
  `CacheRead` covers), never at write.
- The net is marked `Estimated`. A host must not show it as measured.

**Why.** #47 is the fail-open case: an unknown boundary was treated as "nothing is cached, mutate
everything". The library takes the safe direction for the gross figure and says plainly that it does not
know the net. The test is the sentence "this is not a measured net" (§6.4).

### Case 11: the work of one request is not seen as part of the fill (#555)

*ID proposal:* `conservation/baseline-size-from-result`. *Related:* G2. *Status:* needs decision.

**Situation.** A conversation has 380,000 raw tokens. Operators removed 220,000 and added 0, so the
forwarded body is 160,000 tokens. The gate in #555 wants the 380,000 (the fill), but only has the
160,000 (the billed input).

**Expected.** From `Result`, the client-side size can be rebuilt without any new field:
`billed tokens + sum of Saved − sum of Caused` over all tiers = 380,000 (conservation, §6.5). The case
asserts that this holds, and that `Checks` is empty.

**Decision needed.** Whether the library should **expose** that sum as a field, such as
`Result.ClientTokens`, so a trigger can read the fill from the same place as the saving. The case holds
either way. If no field is added, the case documents that the trigger must compute it. The trigger
change itself belongs to #555 and is not tested here.

---

## 6. Scope of the state

### Case 12: two sessions with the same content (#175)

*ID proposal:* `state/session-isolation`. *Related:* H3. *Status:* ready.

**Situation.** Sessions A and B both contain an output with the same bytes. In A, the agent expanded it,
so the operator keeps it verbatim there. In B nothing was expanded.

| Step | Session | What happens |
|---|---|---|
| 1 | A | The operator compacts the output, then the agent expands it. The change ends in A. |
| 2 | B | The operator compacts the same content. |

**Expected.** B's result shows the full saving, with no sign of A. `ThreadKey(session, thread)` already
includes the session (`counterfactual/state.go`), so the library keeps A's end of change inside A's
Tracker. The case is the guard: A's `ThreadState` has no effect on B.

### Case 13: two tenants share a session id (#501)

*ID proposal:* `state/tenant-collision`. *Related:* H3, H4. *Status:* **exposes a gap**.

**Situation.** Tenant A has 3 requests on session `s`, thread `main`. Tenant B then sends 10 requests on
session `s`, thread `main`. Then tenant A sends a fourth.

**Expected.** A's fourth request is priced as if B had never existed: its `PrevPrint` is A's third, and
its `Version` is unchanged by B.

**The gap.** `ThreadKey` is `session + "\x00" + thread.ID`. It has **no tenant**. Under the current
key, B's requests replace A's `ThreadState` and the fourth request compares with B's last one, which is
the library version of #501's `ReplayProjectedTokens` going from 190 to 1,140. Either the key takes a
tenant, or the host must put the tenant into `Session`. The design does not say which, and
`RequestMeta` has no tenant field. This case is expected to fail until that is decided.

---

## 7. Operators that report success but achieve nothing

### Case 14: `cachesplit` reports a split but the breakpoint still covers the churn (#45)

*ID proposal:* `split/reported-not-delivered`. *Related:* E1, E2. *Status:* ready.

**Situation.** On Bedrock Converse the breakpoint is its own array entry placed after the block. The
operator inserts the volatile half **before** that entry, so the breakpoint still covers the volatile
text. The operator reports a `rewrite` and `cachesplit` shows as active. The volatile half changes on
the next turn.

| Step | What happens |
|---|---|
| 1 | The system block (stable 3,000 tokens, volatile 300) is written: 3,300 written. |
| 2 | The volatile half changed. The breakpoint still covers it. The provider reads 0 of those 3,300 and writes them again. |

**Expected.** **No `split` effect.** The library sees from the usage and the forwarded breakpoint
position that the stable half was not read. The result shows a break (the volatile edit) and no saving
credited to `cachesplit`. Compare E1, where the breakpoint lands after the stable half and the usage
shows 3,000 read: there the effect is `split`, 3,000 tokens write to read.

**Why.** The library prices from usage and breakpoints, not from what an operator says it did. #45 is the
class "reports acting, achieves nothing", and this is the case that the library does not repeat it.

### Case 15: `cachesplit` has nothing to split in the new request shape (#567)

*ID proposal:* `split/marker-not-in-system`. *Related:* E2. *Status:* ready.

**Situation.** The volatile snapshot sits in the second text block of `messages[0]`, after both `system`
breakpoints. `cachesplit` finds no marker in `system` and reports no operation.

**Expected.** No operation, no `split` effect, an empty `Ops`. The thread total is 0 on every step. Both
`system` breakpoints cover stable text, so the prefix is read on every hot turn.

**Variant, and the open point of #567.** The volatile block lies **inside** a span that a `messages[0]`
breakpoint hashes. Step 2 changes only that block. Expected: a break from `messages[0]` with an empty
`ChangeID` (a client-made change, case 4). This variant is the measurement #567 says it did not make:
it shows what an operator would save if it moved that breakpoint.

---

## 8. Content added back and content removed by the client

### Case 16: a fixed restore adds the original back (#481)

*ID proposal:* `rewrite/fixed-restore-cost/hot/5m`. *Related:* C4. *Status:* ready.

**Situation.** An operator compacts a 5,000-token output to a 500-token marker (saves 4,500). On turn 4
the model expands the marker. With `fixed_restore` on, the marker stays and the full 5,000-token
original is appended as a new message at a fixed position near the tail.

| Step | Cache | Saved | Caused |
|---|---|---|---|
| 4 (expand turn) | hot | 4,500 read | 5,000 written (the copy is new) |
| 5 | hot | 4,500 read | 5,000 read |
| 6 | hot | 4,500 read | 5,000 read |

**Expected.** Net per hot turn from step 5: +1,350 − 1,500 = **−150 µ$**. The first restore turn costs
5,000 × $3.75 = 18,750 µ$. **The credit of the original compaction must not be shown without the restore
beside it.** This is #481: the dashboard counted the restore once and the compaction every turn.

**Check.** The restore is a `rewrite` that **grows** the body (`SizeAfter > SizeBefore`, C4). Its
`Caused` tokens are positive and its `Saved` is 0. A library that clamps growth to 0, as cg does for an
added tool (case 3 in `cg-differences.md`), fails this case.

**Open point.** With `fixed_restore` off, the original goes back **in place**, and the compaction is no
longer credited at all. #481 asks whether today's figure handles it. The second variant of the case is
that: step 5 forwards the original at message 7, and the expected saving is 0, not 4,500.

### Case 17: the client removes content by itself (#584)

*ID proposal:* `client-edit/shrink-no-operator`. *Related:* A1, A3. *Status:* ready.

**Situation.** The client drops three tool declarations (8,000 tokens) from `tools` on its own, with
no operator. The decision of #583 and #584 is that what the client removes is not ours to count.

**Expected.**

- `Ops` is empty, so no operator gets a `Saved` figure.
- The cache effect is a break at `tools` with an **empty `ChangeID`** (the whole cache moves from read to
  write), or nothing if the client edited after B\*. In both cases nobody's saving rises.
- The thread total counts no saving for the 8,000 removed tokens.

**Variant (option 2 of #584).** The client removed them because a recommendation said so, and a
`ChangeID` of kind `setting` or `rewrite` is reported by the recommender. Expected: the saving appears
under that `ChangeID` and not under the operator. That depends on which option the repo owner picks, so
only the first half is `ready`.

### Case 18: the estimated size is off from what the provider billed (#696)

*ID proposal:* `estimate/billed-ratio-differs`. *Related:* G2. *Status:* needs decision (Q3).

**Situation.** `tool-prune` and a `mask` report `Size` in bytes only. The library converts the bytes
with the request's own ratio (forwarded body bytes ÷ billed input tokens), here 4 bytes per token. The
removal was 40,000 bytes, so the credit is 10,000 tokens. The removed content was JSON tool output,
which has fewer bytes per token than the request on average (3.5), so the provider's real change was
11,400 tokens.

**Expected.**

- Every `PricedOp` built from a converted size has `Estimated: true`. No operator sends a token figure,
  so there is none to leave unchanged.
- The `conservation` check compares billed tokens with the sum of saved and caused. When the conversion is
  off by this much, the check reports a **detail with numbers only** (R7) and does not stop pricing.

**Decision needed.** The request ratio is already the correction #696 asks for, and the library has what it
needs (`Usage`). What is open is whether a content class (`text`, `json`, `binary`) on `Size` is worth
adding to reduce the error (design §11, question 3). The case stays as a test of the **marking** and the
check either way. A content class would add a second expected column.

---

## 9. Summary table

| Case | Issue | Tests | Status |
|---:|---|---|---|
| 1 | #580 | `LastPing` moves on a ping that wrote | ready |
| 2 | #532 | A ping with unknown usage | needs decision |
| 3 | #496 | Break with a `ChangeID` on a warm request | ready |
| 4 | #496 | A break with no operator | ready |
| 5 | #493 | A decision that flips; replay is stable; the change ends | ready, step 4 needs Q12 |
| 6 | #199 | One `ChangeID`, two spans, a break on one | ready |
| 7 | #197 | `unique_change` check across operators | ready |
| 8 | #214 | A `spend` with no rewrite | ready |
| 9 | #232 | Payback over turns, then a cold credit | ready |
| 10 | #47 | Unknown boundary, `penalty_unknown`, gross only | ready |
| 11 | #555 | Client size from billed + saved − caused | needs decision |
| 12 | #175 | Session isolation | ready |
| 13 | #501 | Tenant collision on `ThreadKey` | **exposes a gap** |
| 14 | #45 | No `split` effect when the breakpoint does not move | ready |
| 15 | #567 | Marker absent from `system` | ready |
| 16 | #481 | A restore that grows the body | ready |
| 17 | #584 | A client-made shrink credits nobody | ready, variant needs a decision |
| 18 | #696 | Estimated size against billed | needs decision (Q3) |

**Open questions these cases touch.** Q3 (estimates, case 18). Q12 (end of a change, cases 5 and 12).
New, not yet in the design: how a ping with unknown usage is reported (case 2); whether `Result` exposes
the client-side token total (case 11); where the tenant lives (case 13).

**Suggested order.** Cases 1, 3, 8 and 16 first: they use rules the spec already decides and each
catches a fault that an issue reported. Then 13 and 2, which are the likely design findings.

## 10. Limits of this document

- **The expected figures were computed by hand** from the spec's rules and the issue text. Cases 1, 3,
  9 and 16 pass the conservation check by hand. The others give tokens in words, not a full table,
  and need a number pass when they are written as JSON.
- **The situations are reconstructions.** Each takes its shape from the issue body. The token counts in
  cases 1, 3, 9 and 16 are mine, chosen to make the arithmetic readable. None of them is a figure from
  production traffic.
- **I did not read the code behind the issues**, so a claim such as "`LastPing` must move for a ping that
  wrote" is a requirement from the issue, not a check of the library.
- **Issue state is a snapshot of 2026-10-28.** #580, #567, #496, #493, #555, #199, #324, #696, #586,
  #481, #584 and #501 were open; the others were closed.
