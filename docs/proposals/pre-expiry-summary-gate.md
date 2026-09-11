# The pre-expiry summary gate

How `summarize` decides to compact, and how that work is executed. The gate has two halves: **when**
to compact — only at a moment when the cache write it costs was going to be paid anyway — and **how**
to run it so that deciding to compact cannot itself become the expensive outcome.

The second half exists because a live run showed the synchronous design turning its own best case
into its worst case.

## What the run showed

An end-to-end run on `claude-haiku-4-5` (real 200,000 window, `min_request_frac: 0.9`,
`cache_state: pre_expiry`) built a session to 196,208 billed tokens, then waited 250s to land in
the pre-expiry window. Three things happened in sequence:

| Turn | What happened |
|---|---|
| 18 | trigger fired; the summarizer call **hung for its full 300s budget** (`cg_latency_ms=300,128`, `upstream_ms=2,838`, `summarize_timeouts: 1`). It failed open, so the **full 197k transcript** went upstream — and the cache entry, which had ~50s of life when the request arrived, had died during our own stall. 197,879 tokens written. |
| 19 | arrived 310s idle → phase **Cold** → `cache_state: pre_expiry` does not permit Cold → declined in 3ms. No summary. |
| 20 | `400 prompt is too long: 203,705 tokens > 200,000 maximum` |

The same call shape, measured directly against the same gateway, completes in **5–14 seconds**
(48,717 input tokens after summarize's own 280,000-char trajectory cap). So the model was not slow;
our call stalled, and the root cause was not isolated. It does not need to be, because the stall
exposed three defects that are independent of why it happened.

**Defect 1 — the budget is wider than the window it fires in.** `summarizeCallTimeout` is 300s;
`pre_expiry_seconds` is 60. A slow call therefore *guarantees* the entry dies while we hold the
request, and we pay the full uncompacted rewrite plus a wasted model call, on precisely the turn
meant to save money.

**Defect 2 — the miss is misattributed.** Turn 18 recorded `cache_miss_reason = prefix_change`.
Nothing changed the prefix; the entry expired inside the pipeline. Idle is measured at *arrival*
(250s, still warm), so an expiry caused by our own latency is invisible to that label. This
incidentally vindicated the decision to credit no `prefix_change` turn: it is what stopped the
measurement crediting us for damage we caused.

**Defect 3 — declining on Cold is how the session hit the wall.** A session that goes from warm
straight past expiry, never observed inside the 60s slice, never compacts at all — and then grows
into the provider's hard limit.

## The rule this design follows

> **Only ever spend a cache write that was going to be spent anyway.**

Which means the summary must ride a real user turn. Never a request of our own.

That rule reorders the cache phases, and the ordering is the opposite of the one the first design
assumed. With `F` the full prefix and `C` the compacted one:

| Phase | Without compaction | With compaction | Verdict |
|---|---|---|---|
| **Warm** | `0.1F` read | `1.25C` write, and a live entry destroyed | **the only harmful case** |
| **Pre-expiry** | `0.1F` now, then `1.25F` on the next turn *if* it lands after expiry | `1.25C` now, `0.1C` later | better, **given an assumption** about the next turn's timing |
| **Cold** | `1.25F` — the entry is gone, this turn pays a write regardless | `1.25C` | **strictly better, unconditionally** |

So Cold is the **reliable** half and pre-expiry is the half carrying an assumption. The first
design had this backwards because it reasoned about not destroying something valuable, rather than
about riding a payment already being made.

Cold is also the *reachable* half: pre-expiry needs a request to arrive inside a 60-second slice of
a 300-second lifetime, which is a coincidence you wait for, while any gap longer than the TTL
produces a Cold turn on the next request.

**Hence the default becomes `cache_state: pre_expiry_or_cold`** — "compact whenever it cannot
hurt", the complement of Warm.

### The safety condition that makes permitting Cold sound

`CachePhase` reports Cold when `Ctx.ColdCache` is set, and that flag is a **known false positive on
keep-alive'd sessions**: `proxy/keepalive.go` never updates the tracker, so the flag reads cold
while the entry is alive — the −$708 mechanism `proxy/promexport.go:807` documents. Permitting Cold
off the flag would compact **live** prefixes on exactly the sessions the keeper is paying to
protect.

So the trigger accepts Cold only when the **arithmetic** says so: a known TTL and idle time whose
difference is `<= 0`. The flag alone is not sufficient. `Ctx.CacheRemaining()` already separates
"unknown" from "expired", which is what makes this expressible.

## Execution model

### 1. The hot path never waits for a summary it has not got

On a turn where the trigger fires, the component does **not** call the model inline. It starts the
summary on a **detached** context and forwards immediately, using whatever checkpoint already
exists (which is the existing replay path, unchanged).

This deletes Defect 1 rather than tuning it. There is no budget to get wrong on the hot path
because the hot path does not wait.

The detached context is load-bearing: today the call derives from `c.Ctx`, the request's context,
so a background call would be cancelled the moment the response is written. It needs its own
context and its own budget.

### 2. No pings, and no requests of our own

A ping would hold the **old, full** prefix alive at `0.1F` a time — the prefix we are about to
replace. Once a summary exists we invalidate it anyway, so a ping pays to preserve something we
intend to discard. Expiry costs us nothing once the summary is ready: the next turn pays `1.25C`
either way.

### 3. A later turn waits, but only up to a cap

If a turn arrives while a summary is still in flight for that session, it **waits** — up to
`summary_wait_seconds`, default **120**.

Waiting is worth it because the alternative is forwarding the full transcript: paying `1.25F`, and
at high fill risking the provider's hard limit. Ten seconds of latency against a 197k-token write
(~$0.22 on haiku, ~$2.70 on Opus at 0.9 of 1M) is a good trade on a turn where the agent is already
waiting seconds for the model.

**Expect this to be the normal path, not a rare one.** The trigger fires only after an idle gap of
240s+, so the user is active immediately afterwards, and in an agent loop the next request arrives
1–3 seconds later while the summary needs 5–14s. So a typical episode carries **one turn with a
~5–15 second stall**. The 120s cap is for the pathological tail — the 300s hang actually observed —
not for the common case.

### 4. Single-flight per session

Never two summaries in flight for one session. A busy session must not queue parallel model calls
over the same transcript.

### 5. The background budget outlives the wait

A summary that lands after we stopped waiting is still useful — the next turn splices it. So the
background call keeps a longer budget than the wait cap, and simply holds nobody up.

### Why this needs no new splice path

The checkpoint is anchored by `CoveredCount` **from the head** (`summarize.go:545`:
`boundary := start + cp.CoveredCount`), so appended turns extend the verbatim tail and leave the
covered prefix `msgs[1:1+CoveredCount]` byte-unchanged. An asynchronously-produced summary
therefore only has to land in the store; `tryReuse` splices it on a later turn exactly as it
splices a synchronous one today.

This is also why a user message arriving mid-summary does **not** invalidate the work — it costs a
turn of lateness, not the summary. Discard is forced only when the covered prefix itself changed: a
client-side compaction, or an `expand` restoring content into it. That case wants incremental
summarization rather than a discard, and is filed separately.

## Accounting

The measurement counts realized positives only, and every cost we incur.

- **Credit** on each turn inside the span, from what the provider actually billed: `0.1 × (F − C)`
  on a `hit` turn, `1.25 × (F − C)` on a `ttl_expiry` turn.
- **No credit on the compaction turn itself.** At that moment nothing has been saved — we have only
  spent. The saving arrives on later turns.
- **Debit** every cache write inside the span that we plausibly caused, not only the first.
- **Debit a summarizer call even when its result is discarded.** We spent the money.
- **Unfinished spans are reported, not excluded.** Excluding them drops exactly the cases where we
  paid and did not recoup, which is survivorship one level below the coverage line. Their net is
  computed and labelled as still moving, so the settled total stays comparable.

Whether an unrecouped episode is actually a loss is arithmetic, not an assumption: we are behind
only when `1.25C > 0.1F`, i.e. `C > 0.08F`. At better than roughly 12.5:1 compression the
compaction turn is cheaper on its own turn, before any later read saves anything. So the net is
computed rather than presumed negative.

## Residual risks, named

**The wait cap protects latency, not the ceiling.** If it expires and there is no earlier
checkpoint — first episode of a session, slow summarizer, transcript already past the window — we
forward untouched and the provider rejects the request, exactly as on turn 20. `replayStale` covers
the case where a previous checkpoint exists; nothing covers the first one. This is inherent to
"fail open, always"; the async path makes it much less likely rather than impossible.

**A stall is still a stall.** One turn per episode pays 5–15s. Agents with aggressive client-side
request timeouts will see it.

**The 300s hang is unexplained.** The trajectory cap and the goal text are both correctly bounded,
so the summarizer prompt was ~48k tokens. Gateway queueing after a burst is the leading hypothesis
and is unproven. The async design removes its consequences without explaining it, which is the
right order — but it remains an open question worth instrumenting.

## What this changes in the tests

`TestSummarizeReplaysItsCheckpointOnATurnTheTriggerDeclines` currently asserts "turn 1 compacts,
turn 2 replays byte-identically". Under this design turn 1 forwards untouched and starts the work,
and turns 2 onwards replay. The test is rewritten rather than dropped: it still guards the property
that matters, which is that a turn the trigger declines must not revert to the full transcript.

New coverage needed: a turn arriving mid-summary waits and gets the summary; a turn arriving after
the cap expires forwards without it; two turns in quick succession start only one summary; a
detached summary completes after its triggering request has been answered; Cold from the arithmetic
is permitted while Cold from the flag alone is not.
