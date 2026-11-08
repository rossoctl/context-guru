# Root causes of the differences between cg and the spec

**Status:** companion to [cg-differences.md](cg-differences.md), regenerated from the final commit of
branch `docs/context-operations-spec`. The cases, the spec and the design are in
`github.com/rossoctl/accountant` (v0.1.0).
**Reproduce:** `go test ./dash/ -run TestCasesAgreeWithCurrentLogic -v` from the repo root.

**Update, accountant `c628f22` (2026-11-02).** The tables and figures below come from the 63-case run on
accountant v0.1.0 and are not recalculated. Accountant has since added R54 and three cases, so the
harness now plays **66 cases, and 30 differ** (20 `differs`, 10 `notCovered`). The three new cases
are in section "Cause 9" at the end. The branch named above was closed without merging.

This file explains each of the root causes in cg's logic (`dash.Event.Price`). The case list,
the per-case figures and the limits of the method are in `cg-differences.md`. All figures here come
from the same run (63 cases, 27 differ).

**Total gap.** The sum of the thread-total gaps over the 27 differing cases is 1,273,201 µ$. One of them,
`rewrite/four-operations/hot/5m`, differs only in one line and has no total gap.

| # | Root cause | Cases | Gap (µ$) | Kind |
|---|---|---:|---:|---|
| 1 | No cache-break charge | 9 | 644,501 | cg defect |
| 2 | The rate follows novelty, not position | 3 | 29,475 | cg defect |
| 3 | Growth is clamped; an ended change is still credited | 3 | 38,400 | cg defect |
| 4 | No concept of a setting change | 4 | 378,350 | Different question |
| 5 | Keep-alive credit floor | 0 | 0 | Resolved: both cases agree |
| 6 | Credits booked outside the per-component figure | 4 | 27,450 | Adapter, not evidence |
| 7 | No check for an unreported edit | 1 | 1,500 | Missing check |
| 8 | The unique-versus-replay split has no session or thread | 3 | 153,525 | cg defect or open question |
| 9 | No sibling-thread input to the keep-alive rescue | 3 | see Cause 9 | Missing input |
| 10 | **NEW** No charge for a cache mark the host adds | 13 | see Cause 10 | cg defect |
| 11 | **NEW** The TTL tier follows the current request, not the stored entry | 3 | 13,500 | cg defect |
| 12 | **NEW** The keep-alive rescue needs the new turn to write less than the ping read | 2 | 41,400 | cg defect |
| 13 | **NEW** No size or range check on a reported operation | 4 | see Cause 13 | Missing check |
| 14 | **NEW** Cases not yet traced | 6 | see Cause 14 | Unknown |

## Cause 1: no cache-break charge

**What cg does.** `baselineDeltaUSD` builds the "bill without the change". It takes the bill the
provider sent and adds the price of the removed tokens. It does not re-bill the client body.

**Why this is wrong.** An edit inside the cached prefix changes the prompt above the cached tokens.
The provider cannot read them any more, so it writes them again. That extra write is already inside
the billed amount. So the no-change bill is too high, and the edit gets credit instead of blame.

**Example, `summary/break-then-payback`, request A.**

1. The true no-change bill is 8,175 µ$.
2. The bill with the summary is 17,400 µ$.
3. cg computes 17,400 + 2,850 = 20,250 µ$ as the no-change bill.
4. The gap is 12,075 µ$. This is 3,500 tokens × ($3.75 − $0.30).

**Example, `coref`.** The spec books +5,985 µ$ for the saving and −65,584.5 µ$ for the break. The net
is −59,599.5 µ$. cg books 19,950 × $3.75 = +74,813 µ$. The sign is wrong.

**cg already knows the cost.** `components/offload/prefix_econ.go` uses an 11.5 factor to decide
whether to break the prefix. The reported saving does not use it.

**A fix has two parts.** Each part is needed.

- Charge moved tokens × (write − read) to the edit that caused the move.
- Price the removed tokens by position (see cause 2).

Fixing only one still leaves a wrong figure.

**Unverified.** If the live pipeline never picks `coref` on such a prompt, six of the nine cases are
theoretical. This was not tested.

## Cause 2: the rate follows novelty, not position

**What cg does.** `uniqueRate` returns the write rate for the first removal. The only exception is a
summarizer on a live cache, which gets the read rate.

**What the spec does.** It prices a token by where it sits:

| Position | Tier |
|---|---|
| Before B\* | read |
| B\* to the last breakpoint | write |
| After the last breakpoint | input |
| No breakpoint in the body | input |

**Cases.**

- `after-last-breakpoint` and `no-breakpoint`: 7,000 tokens × ($3.75 − $3.00) = 5,250 µ$.
- `state-lost-reads-cache`: the usage shows 18,500 tokens read. So the 5,500 replaced tokens are
  reads. cg prices them as writes. The gap is 5,500 × $3.45 = 18,975 µ$.

**Note.** The summarizer exception is the same fix, applied to one component by name. The general fix
is to give `uniqueRate` the position or the usage evidence.

**Open point.** The spec's rule for B\* is not settled. A literal reading puts B\* at the end of
`system` in the `coref` case. Then cg's write-rate answer would be right. The earlier model review
read B\* as "the end of the longest prefix the request could have hit". The spec text should say
this.

## Cause 3: growth is clamped, and an ended change is still credited

**What cg does.** `ComponentSavedUSD` sets a negative gross to zero. A change that adds tokens, such
as `expand` or a restore, has no cost line. The request total is still right, because
`Event.Saved()` nets the whole request.

**Two separate failures.**

1. **Addition.** The spec books `expand` as −90 µ$ per hot turn. cg books 0. In `fixed-restore-cost`,
   the restore costs nothing and the `mask` is credited in full every turn. The gap is 17,700 µ$.
2. **Ended change.** In `restore-in-place` the original goes back and the mask ends. The provider
   holds the marker, so 6,000 tokens move from read to write. cg has no "end" event. It keeps
   crediting +1,350 µ$ and books no break. The gap is 20,700 µ$.

**Unverified.** It is not known whether the live pipeline reports `expand` as a component. If it does
not, cg has no row, which is different from a row of zero. The adapter passes `max(delta, 0)`, so the
test cannot tell the two apart.

## Cause 4: no concept of a setting change

**What cg does.** `AttributeCache` sends a model it has not seen to `cold_start`. Its comment says a
first use of a model must never be reported as a bust. cg does not read `effort`. It prices only its
own edits.

**Why it is not a defect.** A router or a client changed the setting. cg did not. cg's answer ("I
saved nothing") is correct for the question it asks. The spec asks a different question: "what did
the cache cost the user?"

**Decision needed.** If the product must show users that a setting change cost them a cache, cg needs
new logic. If not, mark these cases out of scope.

**Open and unverified.**

- Which rates price a model-change break. The case uses the small model (−36,800 µ$). At the large
  model's rates it is −138,000 µ$.
- That a real model renders `effort` ahead of the messages. This comes from `cached-prefix.md`, not
  from a test.

## Cause 5: keep-alive credit floor (resolved)

**What cg does.** `keepaliveSavedUSD` credits a ping only if the gap is above
`keepaliveCreditFloorMs`.

**What changed.** The earlier report listed two cases here (1-hour entry, and minimum lifetime). The
adapter did not pass `CacheTTL` or `Provider`, so the test took the 5-minute path and did not run the
branches. `Play` now sets both. Both cases agree with cg now, and neither is in `known-differences.json`.
The suspected inconsistency between `providerCacheTTLMs` and `keepaliveCreditFloorMs` is **not
confirmed** by them. It is still unmeasured on real traffic.

## Cause 6: credits outside the per-component figure

cg books the cachesplit credit and the head-TTL credit in their own columns. `Play` does not fill
them, so cg shows 0. These four cases do not test cg's logic. They show a gap in the adapter.

**Next step.** Feed the two credits in `Play`, then re-check.

## Cause 7: no check for an unreported edit

The spec refuses to price a body edit that no operation reports. cg has no such check. It books
1,500 µ$ for the size gap between the client body and the forwarded body. The mechanism was not
traced. The money is small. The risk is that an unexplained size change gets counted as a saving.

## Cause 8: the unique-versus-replay split has no session or thread

**What cg does.** `Recorder.MarkUnique` (`dash/capture.go`) decides whether a removal is new. Its key
is tenant, component and content key. It has no session and no thread.

**What the spec does.** Each thread has its own state, so a change reported on another thread, or in
another session, is new there. Three cases show it:

- `threads/change-stays-on-its-thread` and `threads/two-sessions-same-change/main-and-subagent`: the
  spec books 7,000 tokens as a write (26,250 µ$) per thread. cg books the repeat as a read (2,100
  µ$). Which is right depends on whether the thread has its own cache entry.
- `threads/fork-first-request/summary`: the spec keeps no history for a fork, so it prices the first
  request as a write. A fork reads its parent's entry by content, so here cg's read is the truer tier
  and the spec is the one that is off.

**Open question.** Should the library read the parent's state for a fork (`Thread.Parent`)?

## Priority

1. Fix cause 1 together with cause 2. They hold 673,976 µ$ of the 1,273,201 µ$ total gap and share
   one design flaw: the additive baseline.
2. Fix the adapter for cause 6 before judging it. Cause 5 is resolved.
3. Decide cause 4 as a product question, not a bug.
4. Decide cause 8 together with the fork rule in the accountant library.

## Cause 9: no sibling-thread input to the keep-alive rescue

Added after the run above. Accountant's R54 says a sibling thread of the session that read part of what
a ping read, inside the TTL, kept that part warm without the ping, so the rescue leaves it out. The
host reports it in `EntryFacts.SharedWarm`. `dash.Event.keepAliveSavedUSD` caps the credit by what the
last ping read (`KeepAliveRefreshed`) and has no sibling-thread input, so it credits the rescue in full.

| Case | Accountant | cg | Gap (µ$) |
|---|---|---|---:|
| `keepalive/shared-head-sibling/exact/5m` | credits 8,000 tokens | credits 10,000 | 6,900 |
| `keepalive/shared-past-ping-read/exact/5m` | credits nothing | credits in full | see the test output |
| `keepalive/shared-prefix-all-sibling/exact/5m` | no rescue, the ping stays a cost | credits in full | 34,500 |

They are listed in `dash/testdata/accountant/known-differences.json` as `notCovered`: cg prices
something the model does not. Whether cg should learn about sibling threads is open. The proxy path does
not report `SharedWarm` either, so `TestProxyPathMatchesTheAccountantCases` passes on these cases only
because it plays the case's own facts.

## New causes found on `main`

**Added 2026-11-08.** These causes come from a run of `TestCasesAgreeWithCurrentLogic` against context-guru
`main` (`f43d9fa5`) and accountant `8e16076` (114 cases, 58 differ). The harness is the one from
commit `4125331f~1`. `main` reverted the accountant integration in #903, so it has no such test now.
All 9 causes above still hold on `main`. None is resolved. The 28 cases below are not in
`known-differences.json`, so the old harness reports them as plain failures. Gaps are the first
differing line of each case, in µ$.

## Cause 10 (NEW): no charge for a cache mark the host adds

**What the spec does.** If the host adds a cache breakpoint, the provider writes the tokens before
it at the write rate. The client body would have billed them as input. The spec charges the host
tokens × (write − read tier) as a cost. It books it with no ChangeID, because the host reports no
operation for the mark (`markPremium`).

**What cg does.** cg books only what a component removes. A mark that adds a write premium has no
component, so cg books 0.

**Example, `markpremium/added-tail/hot/5m`.** The client has one breakpoint at `messages[0]`. The host
adds one at the last message. The provider writes 6,000 tokens at the 5m rate. The client body would
bill them as input. The mark costs 6,000 × ($3.75 − $3.00) = 4,500 µ$. The spec books −4,500. cg
books 0.

**Cases (13, all `markpremium/*`).**

| Case | Gap (µ$) |
|---|---:|
| `added-tail/first-request/5m` | 4,500 |
| `added-tail/hot/5m` | 4,500 |
| `added-tail/hot/1h` | 18,000 |
| `adding-op-before-head/first-request/1h` | 5,250 |
| `headttl-after-client-mark/first-request/1h` | 9,000 |
| `host-1h-mark-before-client-last/first-request/1h` | 6,750 |
| `host-1h-mark-on-evicted-entry/hot/1h` | 2,250 |
| `inside-previous-read/hot/5m` | 4,500 |
| `mixed-ttl/1h-inside-5m-last/5m` | 4,500 |
| `mixed-ttl/5m-inside-1h-last/5m` | 6,750 |
| `with-headttl-and-adding-op/first-request/1h` | 11,250 |
| `with-headttl/first-request/1h` | 8,250 |
| `with-mark-text/hot/5m` | 4,515 |

**Relation to other causes.** This is related to Cause 1, which also books only removed tokens. It is a
separate mechanism: Cause 1 is a missing break charge on an edit, and this is a missing charge for a
mark. Several of these cases use the 1h head TTL, so the cg side overlaps with Cause 6. The adapter
does not feed the head-TTL credit.

**Unverified.** It is not known whether the live proxy adds marks of this kind. If it does not, these
cases are theoretical for cg.

## Cause 11 (NEW): the TTL tier follows the current request, not the stored entry

**What the spec does.** An entry keeps the TTL it was written with. If a 1h entry is read by a request
that asks for 5m, the entry is alive, and the replayed tokens are reads. The case text says the older
library used the request's TTL, said "cold", and booked a 5m write. That was 12.5 times too much.

**What cg does.** `AttributeCache` and `keepaliveCreditFloorMs` take the TTL the returning request
asked for (`CacheTTL`). cg has no per-entry TTL.

**Cases (3).** In each, the spec books 12,000 µ$ and cg books 7,500 µ$. The gap is 4,500 µ$.

- `ttl/1h-entry-asked-at-5m/hot-after-10-minutes`
- `ttl/5m-entry-asked-at-1h/cold-after-30-minutes`
- `ttl/mixed-1h-head-5m-tail/read-after-10-minutes`

**Unverified.** The cg side was read from the figures and the case text. The code path that picks the
TTL was not traced. Whether a real client changes its TTL tier inside one session is not known.

## Cause 12 (NEW): the keep-alive rescue needs the new turn to write less than the ping read

**What cg does.** `keepaliveSavedUSD` returns 0 when `CacheWrite >= CacheRead`.

**What the spec does.** The rescue is the smaller of what the ping read and what the request read
(design §4.4, condition 4). It does not matter how much the new turn wrote.

**Example, `keepalive/ping-rescue-large-turn/5m`.** Turn 0 caches 6,000 tokens. A ping at minute 4
reads all 6,000. The request at minute 8 reads 6,000 and writes 20,000 new tokens. Without the ping
the entry would have expired at minute 5. The rescue is 6,000 × ($3.75 − $0.30) = 20,700 µ$. cg books 0,
because 20,000 ≥ 6,000.

**Cases (2).** `keepalive/ping-rescue-large-turn/5m` and `keepalive/ping-rescue-write-equals-read/5m`.
The gap is 20,700 µ$ in each.

**Relation to Cause 5.** The two cases in Cause 5 still agree. This is a different branch of the same
function. The write-versus-read guard is the cause here, not the credit floor.

## Cause 13 (NEW): no size or range check on a reported operation

**What the spec does.** The spec bounds every report (check 13). A report with a size above the bound
is not priced and the check `size_range` fails. Other bad input is handled the same way, such as a
negative cost or negative output tokens. A failed check does not stop the pricing of the honest
operations on the same request.

**What cg does.** cg has no such checks (see Cause 7 for the related missing check). Two things follow:

1. cg books 0 where the spec prices a negative net. These cases hold an insertion by one operator and
   a cut by another.
2. cg prices a huge input as if it were true.

**Cases (4).**

| Case | Spec | cg | Gap (µ$) |
|---|---:|---:|---:|
| `check/honest-chain-beside-malformed-rereport/priced` | −93,750 | 0 | 93,750 |
| `check/malformed-rereport-keeps-insertion/priced` | −93,750 | 0 | 93,750 |
| `check/size-after-inserting-operator/priced` | −18,750 | 0 | 18,750 |
| `check/size-above-client-body/keeps-active-change` | 1,500 | 600,000,001,500 | 600,000,000,000 |

**The last case is the worst result of the run.** A report of 2e12 tokens makes cg book
600,000,001,500 µ$ for one change. The spec does not price that report. Even with a small fix, an
unbounded `SizeBefore` must not become a credit.

**Unverified.** It is not known whether the live pipeline can send a size this large. The adapter
passes the case's value as given.

## Cause 14 (NEW): cases not yet traced

These six cases differ and are not in any cause above. The reason in each line comes from the case
text and the figures. The cg code was not read.

| Case | Spec | cg | Gap (µ$) | Probable reason |
|---|---:|---:|---:|---|
| `media/big-image-host-tokens-stay-exact/5m` | 3,000 | 15,000 | 12,000 | cg prices the image at its own estimate. The spec uses the host's token counts as given. |
| `media/big-image-hot-request-op-over-media/5m` | 0 | 15,000 | 15,000 | Same. |
| `media/big-image-small-conversation/bill-below-media-price/5m` | 1,313 | 6,562 | 5,249 | Same. The spec cuts the media to the bill. |
| `extent/no-breakpoint-long-body/hot/5m` | 900 | 1,125 | 225 | Rate by position, like Cause 2. |
| `rewrite/no-breakpoint/cut-inside-cache/5m` | −49,500 | 9,000 | 58,500 | The sign error of Cause 1, with no breakpoint. |
| `split/tail-unchanged-hash-differs-system/no-effect` | 0 | 10,350 | 10,350 | cg's cachesplit credits a split that moved no content. |

## Updated priority

Cause 13 comes first. It is the only one where a wrong input produces an unbounded figure. Then follow
the steps in the Priority section above. Add Cause 10 to the decisions for Cause 1, because both book
only removed tokens. Cause 12 is a small change in one function.
