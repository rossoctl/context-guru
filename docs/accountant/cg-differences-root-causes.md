# Root causes of the differences between cg and the spec

**Status:** companion to [cg-differences.md](cg-differences.md), regenerated from the final commit of
branch `docs/context-operations-spec`. The cases, the spec and the design are in
`github.com/rossoctl/accountant` (v0.1.0).
**Reproduce:** `go test ./dash/ -run TestCasesAgreeWithCurrentLogic -v` from the repo root.

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
