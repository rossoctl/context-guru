# summarize

!!! info "Offload (LLM) — lossy, reversible"
    Compresses the middle of the trajectory into one LLM-written summary; the replaced span is stashed under a marker.

## How it works

`summarize` compresses the **middle of the trajectory** into one LLM-written summary (ported from
CE-Manager's ReSum-style summarizer). It restructures the message list to
`[msg0, <summary system message>, last-K]`; the replaced span is stashed under a marker carried in
the summary message, so `expand` restores the full earlier trajectory. This is the one component
that changes the message count — `apply.Body` rebuilds the body keeping the retained messages
byte-identical.

The summarizer is grounded in the **current task** (first user turn + recent turns are passed as
"summarize toward this"), not a blind digest of the middle.

`summarize` calls a model chosen by `model.source`: `incoming` (default — reuse the proxied
request's own model + key) or `config` (a dedicated cheap model set via `CHEAP_MODEL*` env / the
gateway's `CheapModel`). When no model is available it degrades to a no-op.

**Gating + reuse:** a `trigger` (`min_request_tokens`, `min_messages`; legacy `start_from_message`
folds into `min_messages`) gates the first summary so it fires only on a large/deep transcript.
After that, the summary is **checkpointed per session** and **reused verbatim** (no model call, and
byte-identical so the prefix stays KV-cache stable) until the un-summarized tail grows past
`resummarize_tokens`, when the checkpoint rolls forward with a fresh summary. This is what stops it
re-summarizing every turn.

Run it **alone** (its own preset) — it restructures the whole transcript.

## Before → After

```
before:  [system, u1, tool, a1, tool, u2, … 30 turns …, uN-1, uN]
after:   [system, "=== History Summary === … <summary> … <<cg:…>>", uN-1, uN]
```

## Lossiness

Lossy but reversible — the replaced span is stashed under the summary message's marker and
recovered via `context_guru_expand` / `GET /expand`.

## Shape invariants

Rewriting a transcript can make it *unsendable*, and this component has done so four times —
each found only when a provider returned a 400 on live traffic:

| Symptom | Cause |
|---|---|
| `400 messages.1: role 'system' must precede an 'assistant' message or end the array` | the summary was emitted with role `system` and spliced in front of the kept tail |
| `400 … unexpected tool_use_id found in tool_result blocks` | the span boundary cut a `tool_use` while keeping its `tool_result` |
| `400 … tool_use ids were found without tool_result blocks immediately after` | the mirror: the boundary kept an assistant tool-call turn and cut its results |
| `panic: index out of range [-1]` | a transcript shorter than `keep_last` |

So the output is held to invariants that are properties of the message list alone, checked
offline by `schema.ValidateShapeFor` (see `components/offload/summarize_shape_test.go` and
`apply/shape_validate_test.go`):

- a system-role message away from index 0 is followed by an assistant turn, or ends the array
  (Anthropic's rule; **not** "system only at index 0" — the Claude Agent SDK legitimately
  re-injects a system message every turn);
- every `tool_use` is answered in the contiguous run of tool results that follows it, and every
  `tool_result` answers an earlier `tool_use` — so the span boundary never splits an exchange;
- no message reaches the wire with blank content — a hard Anthropic 400. This component
  already refuses a blank summary itself; the invariant is what catches a *later* component
  in the same pipeline reducing a message `summarize` kept down to nothing.

Two things are deliberately **not** invariants. **Consecutive same-role messages are legal**:
this component's own correct output is `[msgs[0], summary(user), tail…]`, i.e. consecutive user
messages, and Anthropic accepts it — an alternation rule would reject correct output. And role
legality on the wire (`role:"tool"` never reaching Anthropic) is a property of the *bytes*, not
of the normalized list, so it is asserted on the raw body instead (`apply/toolrole_wire_test.go`).

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `summary_level` | `regular` | `concise` \| `regular` \| `highly_detailed`. |
| `keep_last` | 3 | Trailing messages kept verbatim. |
| `min_tokens` | 500 | Span floor — minimum middle size before summarizing. |
| `include_tool_calls` | `false` | `false` → tool outputs masked in the summarized trajectory. |
| `resummarize_tokens` | 6000 | Tail growth that triggers rolling the checkpoint forward. |
| `start_from_message` | 6 | Legacy message-count gate, folded into `trigger.min_messages` when that is unset. Prefer `trigger.min_messages`; this key is still read so old documents keep working. |
| `model.source` | `incoming` | LLM source: `incoming` (proxied model+key) or `config` (cheap model). |
| `model.model` | *the source's own model* | The model to summarize WITH, on that source's endpoint and credential. |
| `model.provider` | `anthropic` | Wire dialect for a config-pinned endpoint: `anthropic` \| `openai`. |
| `model.base_url` | *the provider's public API* | Pin a dedicated endpoint as a full URL. |
| `model.api_key` | *the process env key* | **Credential** for the pinned endpoint; empty falls back to the provider env key, which a hosted deployment refuses. Write-only on the settings page. |
| `model.auth` | `x-api-key` | Anthropic only: `x-api-key` \| `bearer`. |
| `trigger.min_request_frac` | **0.9** | Summarize only once the session has been billed at least this fraction of the model's context window. Measured from the provider's own input count for the previous turn — see below. |
| `trigger.cache_state` | **`pre_expiry_or_cold`** | Summarize only when the prompt cache is about to expire **or has already expired** — the two moments when the cache write it costs was going to be paid anyway. `any` removes the constraint. See below. |
| `trigger.pre_expiry_seconds` | 60 | How wide "about to expire" is. Unmeasured either way. |
| `trigger` (rest) | — | `min_request_tokens`, `min_messages`, `min_output_tokens`, `min_output_frac`, `huge_output_frac`. |
| `summary_wait_seconds` | 120 | The summary is produced **off the hot path**; a turn that arrives while one is still running waits this long for it. When the wait expires the turn proceeds regardless. See below. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

## When it summarizes, and why the default is not "whenever it's big"

By default this component pays for a summary only when **both** are true: the transcript is at least
**0.9** of the model's context window, and the prompt cache is within **60 seconds** of expiring.

The second condition is the one that saves money, and the rule behind it is **only ever spend a
cache write that was going to be spent anyway**. Compacting rewrites history, and rewriting history
the provider is currently holding in its cache invalidates that entry — the next turn re-writes the
whole suffix at 1.25x the fresh input rate.

There are two moments when that costs nothing, and the default permits both:

| Cache state | Without compaction | With compaction | |
|---|---|---|---|
| **warm** | a cheap read | a write, **and a live entry destroyed** | the only harmful case |
| **near expiry** | a cheap read now, a full rewrite later if the next turn is late | a small write now, cheap reads after | better, *given* an assumption about the next turn |
| **already expired** | a **full** rewrite — the entry is gone, this turn pays a write regardless | a **small** write | strictly better, unconditionally |

The expired case is the reliable one: on such a turn there is nothing left to invalidate, and the
only question is whether the write is the whole transcript or the summary. It is also the one you
can count on seeing — a near-expiry turn needs a request to land inside a 60-second slice of a
5-minute lifetime, while any gap longer than the TTL produces an expired turn on the next request.
A session that only ever went fully cold would, under `pre_expiry` alone, never compact at all and
grow until the provider rejected it.

"Expired" is decided by the **clock** (a known TTL and idle time), never by the cold-cache flag
alone: that flag reads cold on a keep-alive'd session whose entry is very much alive, and acting on
it would compact live prefixes on exactly the sessions someone is paying pings to protect.

Measured on production traffic: every session that reached 90% of a 1M window and kept running went
cold in that band **repeatedly** — minimum 2 full-prefix rewrites, median 7, maximum 46. At 0.9 of
1M on an Opus-class model one such rewrite is ~$5.88 against ~$0.50 on a compacted prefix.

`0.9` is not an optimum and is not defended as one: swept from 0.6 to 0.95 the trade is almost
perfectly linear, so every 5-point step down recovers a further $320-500 at $0.22-0.32 per extra
turn spent on a compacted context. 0.9 is the conservative end of a defensible range.

### The cache condition has to be met once, not every turn

It is easy to read the two conditions as a per-turn coin flip and conclude the component will
rarely act. It is not one. The cache condition gates **producing** a summary, and a session only
needs to produce one:

- The **first** summary needs one turn to arrive while the cache is near expiry — anywhere in the
  session's remaining turns, not on a particular turn.
- **Every turn after that** re-emits that summary from the checkpoint with no model call and no
  gate check at all, so the compacted, cache-stable prefix persists whether or not the conditions
  are ever met again.
- When the verbatim tail later grows past `resummarize_tokens`, rolling the checkpoint forward
  needs the conditions again — and **failing to roll it forward is cheap**: the standing summary
  is replayed instead of reverting to the full transcript, so the cost is a staler summary with a
  longer tail, never a cache invalidation.

What that leaves worth worrying about is narrow: a session that reaches the fill threshold and
never once has a turn land near cache expiry gets no benefit. What it does **not** do is lose
ground relative to not running the component at all.

Note also which size the fill fraction measures. It is the **incoming** request, before this
component acts. Because the proxy is transparent, the client never learns its transcript was
compacted and keeps re-sending everything it has, so that number grows monotonically and the fill
conjunct, once satisfied, stays satisfied. What shrinks is what goes **upstream**.

### The summary is produced off the hot path

A turn that decides to summarize **does not wait for the summary**. It starts the work and forwards
immediately with whatever it already had; a later turn splices the result.

That is not an optimisation, it is a correctness fix. The trigger fires when the prompt cache has at
most `pre_expiry_seconds` (60) left to live, and the model call's budget is 300 seconds — so a slow
call *guaranteed* the entry died while the request was held, and the full transcript then went
upstream at the cache-creation rate. Measured once on live traffic: 300s held, 2.8s of it upstream,
197,879 tokens re-written, and the model call wasted. The best moment to compact became the most
expensive one, from latency alone.

**Expect one turn per episode to stall for a few seconds.** The trigger fires only after a long idle
gap, so the user is active immediately afterwards, and in an agent loop the next request usually
arrives while the summary is still running. That turn waits — up to `summary_wait_seconds` — because
the alternative is sending the full transcript. When the wait expires the turn proceeds with what it
has.

Only one summary runs per session at a time. A request with **no session id** (the library API,
`/compact`) is summarized inline instead, because the checkpoint is keyed by session: with no key
there is no later turn that could ever find the result.

Two counters say whether this is healthy, and they are the only place a degraded summarizer now
shows up: `summarize_timeouts` / `summarize_errors` for calls that never landed, and the
`awaited_checkpoint` / `summary_wait_timeout` pair for whether the wait cap is set anywhere near
right.

### ⚠️ What caps the context is the CLIENT, not this component

A session whose turns are seconds apart never enters the pre-expiry window, so **this component will
decline on every turn while the transcript keeps growing.** That is intended: the ceiling belongs to
the agent. Claude Code runs its own compaction as it approaches its context budget, and this
component's job is to get ahead of the expensive rewrites below that ceiling, not to be the ceiling.

**If your client does not compact its own context** — anything driving a raw API, a custom harness,
a benchmark runner — nothing will stop the transcript growing until the provider rejects it. Those
deployments must opt out:

```yaml
components:
  summarize:
    trigger:
      cache_state: any        # summarize whenever the size gates are met
      min_request_frac: 0     # …and let min_request_tokens be the size gate
```

Both shipped example configs under `examples/llm-d-service/` do exactly this, because they drive a
raw endpoint.

An unrecognised `cache_state` is a config **error**, not a silent fall back to `any`: a typo in the
one key that decides when this component fires would otherwise turn the gate off while reading as
though it were on.

### The fill fraction is measured on the provider's numbers, and refuses to guess

Two things have to be true before "is this transcript 90% full" can be answered at all, and this
component declines — counting `window_not_exact` — whenever either is missing.

**The window has to be the real one.** It comes from an operator's own price list if configured,
else LiteLLM's public map. If neither answers, a small substring table compiled into the binary
does — and that table answers 200,000 for every Opus, against a real 1,000,000. A 0.9 fraction
resolved against it would fire at 180k, five times too early. So a window that came from the table,
or no window at all, declines rather than acts.

**The fill has to be measured the same way the window is.** A context window is stated in the
tokens the provider bills: the messages, *plus* the system prompt, *plus* the tool declarations,
*plus* the JSON envelope. This proxy's own tokenizer sees only the message text, which on measured
traffic is a median **3.4x smaller** (p25 2.4x, p90 6.8x). Comparing our count against the
provider's window would demand roughly three times a 1M window's worth of transcript — a threshold
that can never be reached, because the request is rejected upstream, or the client compacts, first.

So the fill comes from the provider's own reported input count for the session's **previous** turn,
recorded when that response arrived. That makes it one turn stale, which is deliberate: a
transcript only grows, so the previous turn is a sound lower bound, and a gate that opens one turn
late is the harmless direction. A session's **first** turn has no such figure and declines — an
unknown fill is not an empty one.

Setting `min_request_frac: 0` removes all of this, and then `min_request_tokens` (counted in
message text) is the size gate.

## Measuring what it earned

The Components tab carries a **What each summary earned** panel, which follows every summary for
**10% more of the model's context window** and reports what happened over that span:

| Column | What it is |
|---|---|
| Cold credit | turns in the span whose cache entry had lapsed — an uncompacted prefix would have been re-written in full at 1.25x fresh. This is the cost the trigger exists to avoid. |
| Read credit | turns whose cache hit. A shorter transcript is cheaper on every warm turn too, at the 0.1x read rate. Usually the larger column, because warm turns are far more numerous. |
| Our cost | the summary's own model call, plus the cache write the rewrite caused. The write half is an **upper bound** — some of it was growth that would have been paid anyway. |
| Net | credit minus cost. Negative is shown as negative. |

Three things about that panel are worth knowing before reading a number off it.

**It reports coverage, not just savings.** Beside the table is the count of conversations that
reached the fill threshold and produced **no** summary at all, and what their expired-cache
rewrites cost. Without that line the panel would only ever describe sessions this component fired
on, which is survivorship and always looks positive. That count is also the number that says
whether `pre_expiry_seconds` should be widened.

**`recorded` and `inferred` are separate rows and are never added.** `recorded` means the component
said so on that turn. `inferred` means an older row acted and carried no replay marker, so it must
have paid for a summary — sound, but deduced. It exists because the marker is new: `inferred` is
the only way to see what the **old size-only trigger** achieved, and therefore the only available
comparison.

**Spans that are `open` or `voided` are counted and not totalled.** Open means the span had not
finished inside the time range being viewed; voided means the client compacted its own transcript
part-way through, which is the very thing this trigger tries to get ahead of, so the remainder is
not comparable.

The measurement introduces no new pricing. Every dollar comes from the same per-request figures the
rest of the dashboard uses, priced when the request was recorded at the rates then in force — the
panel scopes them into spans and splits them by the cache state of the turn that earned them.
A conversation on a model whose context window is not published is excluded rather than measured
against a guess, for the same reason the trigger declines there.

## When it shines

Long agentic sessions where the bulk is stale middle context, behind a client that caps its own
context window.

## When it's inert

The cache state does not permit it (`cache_state_declined_warm`, or `…_cold` / `…_unknown` under a
non-default `cache_state`), transcript below `trigger` (`below_request_trigger`), context window
unknown or guessed (`window_not_exact`), span below `min_tokens`, or no model available
(`no_model`).

The full design — the run that motivated it, why an expired cache is the reliable case rather than
the merely-safe one, why there is no keep-alive ping, and how the saving is accounted — is in
`docs/proposals/pre-expiry-summary-gate.md` in the repository.

**Inert is not the same as untouched.** Once this component has summarized a session once, every
later turn re-emits that same summary from a checkpoint — byte-identically, with no model call, even
on turns the gates decline and even where no summarizer is configured. It has to: sending the full
transcript again on a quiet turn would diverge from the bytes the provider cached at the first
summarized message and force the very cache-write the trigger exists to avoid. Those turns are
counted as `reused_checkpoint` / `gated_replayed_checkpoint` replays rather than as acts, so `/stats`
distinguishes a component amortizing old work from one paying for new work.

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
