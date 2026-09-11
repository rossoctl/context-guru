# `timely-compact` — compacting a nearly-full context while the cache is still worth something

**Status:** proposal. Nothing here is implemented. Two of its three claims are already measured
elsewhere in this repo; the third — how often the trigger would fire — is not, and is measurable
today without shipping anything.

**The ask this answers:** above some fill of the context window (0.9 was proposed), when the prompt
cache is about to go cold, compact — because the session is very likely to reach 100% where the
agent compacts anyway, and doing it earlier converts a ~940k-token cache-write into an ~80k one.

Two questions, answered in order:

1. can the existing pre-expiry trigger carry this? **Yes, and it is the right trigger for a reason
   that is not obvious** — see [The trigger](#the-trigger-reuse-pre-expiry-do-not-reach-for-the-cold-gate).
2. can we invoke the provider's *native* compaction instead of our own summarizer? **Two different
   "native"s, and the answers differ**: Claude Code's auto-compact, no; the Anthropic API's
   server-side compaction, yes, with one piece of state we would have to own — see
   [Native compaction](#native-compaction-two-different-questions-wearing-one-name).

---

## What the pre-expiry trigger is today

It exists once, as `(*ExtractSweep).sweeping` in `components/offload/extract_sweep.go:240`, and it is
four lines:

```go
func (e *ExtractSweep) sweeping(c *components.Ctx) bool {
	if c == nil || c.ColdCache || c.CacheTTLMs <= 0 || c.IdleMs <= 0 {
		return false
	}
	remaining := time.Duration(c.CacheTTLMs-c.IdleMs) * time.Millisecond
	return remaining > 0 && remaining <= e.preExpiry
}
```

Three facts on `components.Ctx`, all set once in `apply.BodyOpts` (`apply/apply.go:595-620`) so that
one fact has one reader:

| field | where it comes from | meaning |
|---|---|---|
| `CacheTTLMs` | `cacheTTL(provider, body)`, widened by `sessionTTL` under both the session id and the `session.Scoped` alias | the cache's **believed** lifetime, **derived from the request** — a bare `ephemeral` mark is 5 min, an explicit `ttl: "1h"` is an hour. `0` means the cache-aware path did not run, and unknown never fires. |
| `IdleMs` | `nowMs - prevAt`, where `prevAt` is `modes.Tracker.TurnAt`, taking the **later** of this session's clock and its alias's | how long this session was idle before this request |
| `ColdCache` | `cacheIsCold(prevAt, nowMs, ttl)` — the same inputs plus a one-minute clock-skew margin | apply's own verdict that the entry is gone |

`preExpiry` defaults to `time.Minute` (`defaultPreExpiry`), configurable as `pre_expiry_seconds`. On a
turn that does not qualify the component **does not return** — it still replays its frozen decisions,
which is what makes a saving survive the turn that earned it and keeps the forwarded prefix
byte-stable. `rep.Gate("not_in_pre_expiry_window")` records the decline.

### Why the window exists at all

The sweep's two halves want opposite cache states. Its **ask** — "which of these outputs does the
agent still need?" — is sent to the request's own model, appended to the previous turn's sent body, so
that the provider reads it out of the prompt cache those bytes populated (measured: 19,595 tokens read
from cache, 0 created). That needs the entry to still **exist**. Its **removal** rewrites deep
history, which invalidates a live prefix and forces a cache-write of the whole suffix at 1.25x fresh.
That wants the entry **gone**. Both are cheap only in the slice where the entry still exists and has
almost no life left. Hence `0 < remaining <= preExpiry`.

That is the whole derivation, and it matters for question 1: **the window's shape is dictated by the
sweep's decision procedure, not by the economics of removal.** `summarize` carries the content it is
compacting to a cheap model, so it needs no cached prefix, and the naive conclusion is that
timely-compact should use the plain cold gate instead. That conclusion is wrong, for a reason
specific to this codebase.

---

## The policy, stated plainly

Two tiers. The second one is the feature; the first is a backstop.

| tier | condition | why |
|---|---|---|
| 1 | `fill ≥ 0.99` | compact unconditionally — the ceiling is one turn away and something is going to compact |
| 2 | `0.9 ≤ fill < 0.99` **and** the cache is within seconds of expiring | compact now, so the rewrite that is coming lands on ~80k instead of ~940k |

**What the compaction actually buys, on opus at 0.9 of a 1M window** (input $5/MTok → read $0.50/MTok,
write $6.25/MTok):

| | on the compacting turn | on each later cold miss |
|---|---|---|
| do nothing | read 940k = **$0.47** | write 940k = **$5.88** |
| compact to ~80k | write 80k = **$0.50** | write 80k = **$0.50** |

So the compacting turn is a wash (+$0.03) and every subsequent miss saves ~$5.38. Measured fleet
average across mixed models and sizes: **$4.48** per cold event above 90%.

**The TTL refresh is not an additional bonus, and should not be quoted as one.** The provider
refreshes an entry's lifetime on every read as well as every write ("the cache is refreshed for no
additional cost each time the cached content is used" — quoted in `proxy/keepalive.go:39`), so the
uncompacted turn would have bought the same five minutes for free. What changes is not *how long* the
entry lives but *what it costs to lose*. The entire gain is the second column above.

## Why that trigger is pre-expiry rather than the cold gate

Three arguments, strongest first.

### 1. The cold gate is unsafe on any account running keep-alive; pre-expiry fails closed

`proxy/keepalive.go` pings an idle session (`max_tokens: 1`) to refresh the provider's entry. **The
ping does not call `modes.Tracker.TurnAt`** — nothing in `keepalive.go` touches the tracker. So on a
keep-alive'd session our idle clock keeps aging while the provider's entry stays warm, and:

- `ColdCache` reads **true** while the entry is **live** → a cold-gated compaction rewrites a warm
  prefix and pays the 1.25x suffix re-write. This is precisely the failure `proxy/promexport.go:807`
  documents at **−$708 on sonnet-5 against +$0.62 of upside**;
- pre-expiry reads `remaining = CacheTTLMs - IdleMs` as **negative** → the window **declines**.

Same divergence, opposite behaviour. Pre-expiry is the conservative predicate by construction, which
is why it is the one to reuse rather than re-derive. (`apply.go:498-540` already carries two
widenings — the alias clock and the alias TTL record — both deliberately monotone *toward warm*. The
keep-alive gap is a third instance of the same class and is not currently closed; see
[open questions](#what-is-not-measured).)

### 2. On a nearly-full session, keep-alive itself is the thing compaction makes cheap

A ping is a cache **read** of the whole prefix. At 940k tokens on opus that is
`0.1 × $5/MTok × 940k ≈ $0.47` per ping, and `MaxPings` lets several fire per idle gap. Compacting
first makes every subsequent ping ~12x cheaper *and* makes the eventual miss cheaper. The cold gate
cannot deliver this: on a keep-alive'd account the session is not supposed to go cold at all, so a
cold-gated compaction would either never fire (working keep-alive) or fire wrongly (argument 1).

### 3. Firing before the miss, not on it, is worth exactly one full-price rewrite

This is the weakest of the three and should be stated honestly, because the intuition ("compact
before the cache goes cold") overstates it. Our compaction shrinks **what we forward**, so a
compaction performed *on* the cold turn already pays the cache-write on the small body — most of the
money is not lost by waiting. What waiting costs is the one rewrite you were standing in front of.

### The measured case for the fill threshold

From the `cache-cold-timing-analysis` session (production snapshot 2026-09-04, `agent='claude-cli'`,
`status=200`, read-only against `cg.db`; scripts on the older eval box under `/tmp`, subject to its
10-day cleaner):

**Every session that reached 90% of a 1M window and kept going went cold in that band repeatedly.**
Universe: all 17 sessions that ever entered 90-100%.

| category | sessions | turns ≥90% | cold rewrites | paid | if compacted | **net** |
|---|---:|---:|---:|---:|---:|---:|
| A1 never compacted, never cold in band → pure loss | 2 | 10 | 0 | $0.00 | $0.00 | **−$0.84** |
| A2 never compacted, but went cold in band | 5 | 243 | 21 | $91.15 | $7.98 | **+$81.07** |
| B compacted, stayed hot throughout | **0** | — | — | — | — | — |
| C1 compacted, went cold **once** | **0** | — | — | — | — | — |
| C2 compacted, went cold **2+ times** | 10 | 1,388 | 133 | $571.41 | $40.42 | **+$530.99** |
| **total** | **17** | **1,641** | **154** | **$662.55** | **$48.40** | **+$611.22** |

B and C1 are empty. The minimum number of cold rewrites for a session that compacted is **2**;
median **7**; max **46**. The "we compacted and it stayed hot, so we only lost accuracy" case has
**zero members** in this corpus, and the entire downside is **−$0.84 across 2 sessions**.

Also: **110 of 126** rewrites landed in the 90-99% band on the way up, not at the ceiling — a trigger
at 0.9 catches them before they are paid.

**On the threshold itself: 0.9 is defensible but not special.** Swept at thread level, the trade is
almost perfectly linear — no knee:

| T | net recovered | turns run on compacted context | $ per degraded turn |
|---:|---:|---:|---:|
| 95% | $250.37 | 587 | $0.427 |
| **90%** | **$569.34** | **1,594** | **$0.357** |
| 85% | $939.92 | 3,022 | $0.311 |
| 80% | $1,315.37 | 4,377 | $0.301 |
| 75% | $1,683.21 | 5,837 | $0.288 |
| 60% | $3,065.48 | 11,798 | $0.260 |

Every 5-point step down buys $320-500 more at $0.22-0.32 per extra degraded turn, flat across the
whole range. So the threshold is not a discovered optimum, it is a price you choose: *what is one
turn of compacted-instead-of-full context worth?* Below ~$0.22 go to 60%; above ~$0.43 do not do this
at all. 0.9 is the conservative end of a defensible range, and 94-99% of the money at every threshold
comes from threads that demonstrably kept running after crossing it.

**One caveat that must travel with these numbers.** This is a **1M-window phenomenon**. The same
cold miss in the 200K regime, where Claude Code's own auto-compact governs, costs $0.85 an event
against $4.48 — 5.3x cheaper and 3.4x rarer, so **18x less money** ($25.54 total against $452.40).
The pitch is not "compaction is mistimed everywhere"; it is "when a session is allowed to fill a 1M
window, going cold up there costs 5x per event and nothing currently fires in front of it."

### Where the code goes: tier 2 is a TIMER, not a request gate

This is the part the first draft of this document got wrong, and it is the difference between a
feature that fires and one that mostly does not.

"The cache is at 280s of 300s" is a statement about a moment when **nothing is in flight**. A gate in
the pipeline can only observe it if the agent happens to send a turn inside that 20-second slice; a
session that pauses for twenty minutes presents its next turn *after* expiry and never qualifies. Put
tier 2 in the pipeline and its firing rate becomes the design's largest unknown — an unknown created
entirely by the placement.

`proxy/keepalive.go` **is already that timer**, and it already has everything tier 2 needs:

- it holds "the exact bytes that went upstream" per session (`keeper.record`, `keepalive.go:559`)
  plus the caller's credential;
- it wakes on an idle deadline derived from the same believed TTL;
- it already makes a real upstream request on its own initiative, with `max_tokens: 1`
  (`keepalive.go:1036`) — the one place in this codebase where work happens off the agent's critical
  path.

Today that ping re-sends the same bytes, refreshing the **big** entry. Tier 2 is: when the held body's
fill is ≥ 0.9, have the ping send the **summarized** body instead. Consequences, all of them wanted:

- it fires at 280s by construction, so there is no firing-rate question;
- the ~19.6s summarizer call is off the agent's critical path entirely;
- the small entry is what is warm when the agent comes back, and the next real turn — compacting to
  the same frozen checkpoint — reads it;
- `modes.Tracker` must record the ping's length as a turn, or the following turn treats those
  messages as mutable tail and rewrites them. This is the same argument `apply.go:560-585` already
  makes for why a *bypassed* turn has to record its length, and the fix is the same call.

Tier 1 (`fill ≥ 0.99`) stays a plain pipeline gate — at the ceiling there is a request in flight by
definition.

### What the code needs

Tier 1 is `fill ≥ 0.99`. Tier 2 is `fill ≥ 0.9` **AND** the pre-expiry window. Half of that already
exists:

- **fill** — `components.Trigger.MinRequestFrac` resolved against `Ctx.CtxWindow`
  (`components/trigger.go:47`). `summarize` already consults it (`summarize.go:157`). So
  `trigger: {min_request_frac: 0.9}` is expressible **today**.
- **cache state** — not expressible. `Trigger` has no cache-state field, and the predicate is a
  private method on `ExtractSweep`.

Smallest change that does not create a second reader of one fact:

1. Lift the predicate into `components` — e.g. `components.CachePhase(c, preExpiry) → warm |
   pre_expiry | cold | unknown` — and have `(*ExtractSweep).sweeping` call it, so the two cannot
   drift. The repo already carries a scar from exactly this (`ttlTier`: the cold decision and the
   dashboard disagreed because each derived the TTL itself).
2. Add `Trigger.CacheState` (`any` default | `pre_expiry` | `cold` | `pre_expiry_or_cold`) and
   `Trigger.PreExpirySeconds`, both surfaced through `TriggerFields` so the fields-parity test forces
   the settings form to learn about them.
3. Configure `summarize` with both tiers — `trigger: {min_request_frac: 0.99}` for the unconditional
   one, and `{min_request_frac: 0.9, cache_state: pre_expiry}` for the timer-driven one. The shared
   helper is what lets keep-alive and the pipeline agree on "the cache is nearly gone" instead of each
   deriving it.

`summarize` is already built for a trigger that fires rarely: its skip is documented as **recurring**
rather than one-off, a skipped turn "refreshes nothing but splices nothing either, so no marker of
theirs dangles" (`docs/reference/config.md:85`), and its checkpoint replay (`summarize.go:383-460`) is
what keeps the forwarded bytes identical on the turns in between. That is the property this design
depends on: fire once high in the window, then replay, so the saving persists and the cached prefix
stays byte-stable.

Two known costs of doing it inside `summarize`:

- it needs `CHEAP_MODEL` or it silently no-ops (`docs/reference/config.md:166`);
- the summarizer call is **on the agent's critical path**, measured ~19.6s mean on an idle box and
  budgeted to 300s. Adding that to a turn is a real UX cost. The one place in this codebase where
  work happens with **no request in flight** is the keep-alive timer, so computing the checkpoint
  during a ping and splicing it on the next real turn is the obvious latency fix — and a second,
  independent reason this feature and keep-alive belong in the same conversation. Out of scope for a
  first cut; worth recording as the intended shape.

---

## Native compaction: two different questions wearing one name

### (a) Claude Code's own auto-compact — a proxy cannot invoke it

It is **client-side**: the agent decides from its own token accounting, then issues a summarize
request that context-guru deliberately **bypasses** (`proxy/agentcompaction.go` — compacting the
compactor is the one case where the pipeline makes a request strictly worse than no proxy at all).
Only the client owns its transcript, so no request-path intervention can make it discard one.

**Where the client's threshold actually sits varies by client, and the fleet data does not describe
every deployment.** Two populations, both real:

- Sessions whose client budgets against a hardcoded **200,000**: a tight mode of 147 events between
  150K and 190K, median **167,425** (p10 157K / p90 177K), reproducing independently across eight
  tenants at medians of 158K-170K. Not `window − max_tokens` (200,000 − 64,000 = 136,000, 31K below
  the observed trigger), and stable across days.
- Sessions that run to **999,8xx tokens and then drop**, i.e. a client that knows the window is 1M and
  compacts near the ceiling. These are the ten sessions the money in this proposal comes from.

An earlier draft of this document generalized the first population's 167K figure into a claim about
the whole fleet, and drew from it the conclusion that "just fix the advertised window" would recover
~$1,073 with context-guru doing nothing. **That conclusion holds only for clients in the first
population and must not be repeated as a general caveat.** For a client that already knows it has 1M
and compacts at the ceiling, the 0.9-1.0 band is precisely the stretch nothing protects — which is the
premise of this proposal, not an objection to it. Check which population a given deployment is in
before quoting either number: `/context` on `claude-opus-5` through the gateway reports the window the
client believes it has.

**The lever that would let a proxy move the client's threshold, and why it is not this one.**
Inflating `usage.input_tokens` on the response would make the client's budgeter think it is closer to
its ceiling, and it would work. It should be rejected on honesty grounds rather than feasibility ones:
it corrupts the number the user reads in `/context` and in `cost.total_cost_usd` — which this repo's
own statusline skill consumes — it is unauditable from the client, and it makes the proxy lie about
billing.

**What we do instead is the honest version of the same effect, and it is a free consequence of tier 1
rather than a mechanism to build.** Our compaction genuinely forwards fewer tokens, so the
`input_tokens` the provider reports back are genuinely smaller. If the client derives its context
gauge from response usage — very likely, since Claude Code's statusline payload carries
`context_window.total_input_tokens` (`context-guru-plugin/plugin_test.go:2005`) — then compacting
**delays** the client's own auto-compact rather than racing it. Stated as inference, and cheap to
confirm: compact once at tier 1 and watch whether `/context` drops.

Two consequences of that, worth deciding deliberately rather than discovering:

- our compaction **substitutes** for the client's rather than duplicating it, which is the outcome we
  want (ours is reversible; the client's is baked into its transcript permanently);
- but the client's transcript keeps growing, so we carry and re-compact an ever-longer body. Fine on
  cost — the frozen checkpoint replays — and it means the client's `/context` reading stops tracking
  the real conversation length. Nothing measures where that stops being acceptable.

### (b) The Anthropic API's server-side compaction — yes, with one piece of state we must own

This is real and is a genuine alternative to our summarizer. Beta `compact-2026-01-12`,
`context_management.edits: [{type: "compact_20260112"}]`, **beta on all five platforms** (1P, Claude
Platform on AWS, Bedrock, Vertex, Foundry), on Opus 5 / 4.8 / 4.7 / 4.6, Sonnet 5 / 4.6 and the
Fable family — so it covers both `claude-opus-5` and `aws/claude-opus-5` as served here.

```json
{"context_management": {"edits": [{
  "type": "compact_20260112",
  "trigger": {"type": "input_tokens", "value": 150000},
  "pause_after_compaction": true
}]}}
```

- `trigger.type` is `input_tokens` only; **default 150,000, minimum 50,000**.
- **It cannot be forced.** There is no "compact now" call.
- **But the threshold is a per-request field, which is the whole trick.** A proxy that wants to
  compact *this* turn sets `trigger.value` just below the request's own input-token count, and leaves
  the edit off entirely on every other turn. Our trigger decides *when*; the provider's threshold
  becomes the mechanism. That is a de-facto on-demand invocation without asking the API for one.
- `pause_after_compaction: true` returns `stop_reason: "compaction"` after the summary is generated
  and before the model continues — so we can inspect, count and stash it before anything reaches the
  client.

**The one hard requirement.** The response carries a `compaction` content block, and it **must be
echoed back on every later request**, or the API drops every block *prior to* it. Claude Code has
never heard of a `compaction` block and will not echo it. So the proxy must own that state:
withhold the block from the client, stash it, and splice it into subsequent requests in place of the
history it replaces.

That is structurally what this codebase already does — a marker plus a stashed original plus a frozen
decision replayed on later turns — and both halves of the plumbing exist:

- **withholding blocks from a streamed response**: `sp.pass(resp.Body, expand.ToolName,
  adjudicate.ToolName)` in `proxy/proxy.go:1521` already withholds SSE events from the client and
  hands them back later, for the expand/adjudicate tool loop;
- **holding replayable per-session state**: `store` + `expand` markers + `summarize`'s checkpoint
  replay.

Three properties that make this attractive relative to our summarizer, and three that do not.

**For:**

1. The summary is produced by the request's **own** model over its own cached prefix — no
   `CHEAP_MODEL` dependency, and no separate call to pay for or time out.
2. **Reversibility is free here, unusually.** Native compaction is irreversible from the API's point
   of view, but the client is stateful and we are transparent: Claude Code re-sends the full
   uncompacted transcript on every turn, so we always hold the originals regardless. The
   fail-open/reversibility boundary in `CLAUDE.md` is satisfiable without a stash of our own.
3. Cache interaction is documented and matches our trigger: put a `cache_control` breakpoint **on the
   compaction block**, and a separate one at the end of `system`, so a compaction does not invalidate
   the cached system prefix. Compaction invalidates the prefix by construction — which is exactly why
   it should happen inside the pre-expiry window and nowhere else.

**Against:**

1. The summary's content and shape are **not ours to tune**, and we have no equivalent of
   `marker_mode`, `keep_last`, `summary_level` or `resummarize_tokens`.
2. The summary is generated by the **expensive** model, not a cheap one. Whether that is cheaper than
   our summarizer's call net of the tokens it removes is unmeasured.
3. It adds an `anthropic-beta` header and a top-level body field that must survive litellm → bifrost
   → Bedrock. Any of those may strip or 400 an unknown beta, so this has to fail open per
   `CLAUDE.md`: on any error, drop the edit and forward the request unchanged.

**Recommendation:** two phases, because the trigger is the risky half and it is shared.

- **Phase 1 — our summarizer.** `summarize` with `trigger: {min_request_frac: 0.9, cache_state:
  pre_expiry}`, on top of the shared `CachePhase` helper. Nothing new upstream, reversible,
  measurable, and it validates the trigger — which is what both phases stand on.
- **Phase 2 — native compaction as an alternative compactor behind the same trigger.** Only after
  phase 1's firing rate is measured. Its value is summary quality and dropping the `CHEAP_MODEL`
  dependency, not the trigger.

---

## What is not measured

1. **How much of the traffic tier 2 can reach, given that keep-alive is opt-in per account.** Putting
   tier 2 on the keep-alive timer removes the firing-rate question — it fires at 280s by construction
   rather than hoping a turn lands in a 20-second slice — and replaces it with a coverage question:
   keep-alive is opt-in, capped per session and capped in count, because a ping is real spend on the
   caller's own credential. A deployment that has not enabled it gets tier 1 only. Whether tier 2
   should be able to run its own timer independently of the keep-alive authorization is a policy
   question, not a technical one, and the answer is probably no for the same reason keep-alive is
   opt-in: it spends someone else's money.
   The measurement worth doing first is still cheap and needs no code shipped — the inter-request gap
   distributions already in `dash/kvcache.go` (`MedianIdleMs` / `MeanIdleMs` / `P90IdleMs`) against
   each session's recorded TTL say how many sessions above 0.9 fill ever present a gap long enough for
   a pre-expiry ping to matter.
2. **Whether the keep-alive/tracker divergence should be closed** by having a ping record a turn.
   Doing so would make `ColdCache` honest on keep-alive'd accounts and would remove argument 1's
   asymmetry — but it also changes the sweep's behaviour, and the sweep is shipped. Its own issue,
   its own branch off `origin/main`.
3. **Accuracy cost.** The table prices dollars against *turns run on a compacted context*
   (1,388 + 243 + 10 at T=0.9) and does not measure what those turns lost. Nothing in this repo
   measures compaction's effect on task success at a given fill.
4. **The counterfactual is a price comparison, not a simulation.** It assumes each of the 154 cold
   events still happens but on an ~80k prefix instead of ~940k. Well grounded for C2 (those sessions
   demonstrably ran hundreds of turns after compacting); weaker for A2, whose restart level is
   *modelled* at 79,997 tokens rather than observed. Varying the assumed post-compaction prefix from
   30k to 110k — 3.6x — moves the answer under 10%, because the rewrites being repriced are
   600k-999k.
5. **Native compaction's cost and quality against `summarize`'s.** Unmeasured on both axes.
6. **Whether a per-request `trigger.value` set just under the current input count reliably fires
   exactly once.** Inferred from the documented semantics, not observed. Phase 2 starts here.

## Provenance

The production figures come from the `cache-cold-timing-analysis` session against the deployed
service's `cg.db` (snapshot 2026-09-04, `agent='claude-cli'`, `status=200`), and two of its own
caveats travel with them: the per-session **compaction counts** are unreliable (a fall from ≥90% of
window to <25% fires 229 times on one session — subagent requests interleaved under the same
`session_id`), and the 16-hex ids printed in the earliest version of that analysis are **tenant**
ids, not session ids. The peak-and-restart shape is the part that survives scrutiny, and it is the
only part the money depends on. Thread-level reconstruction fails to be non-decreasing on 9-11% of
consecutive pairs, which inflates both `turns ≥T` and `fires` — both working *against* the savings,
so the net figures are conservative on that axis.

Provider-side facts (the `compact_20260112` shape, the 50,000 minimum, `pause_after_compaction`, the
echo-back requirement, the platform availability row) are from the Anthropic compaction
documentation, fetched 2026-09-09.
