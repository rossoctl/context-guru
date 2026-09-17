# `cache_aware_summarizer`

Offload (LLM). Replaces the middle of a long transcript with one model-written summary — the same
output shape as [`summarize`](summarize.md) — but builds the **summarization call** differently, so
that call can reuse a prefix the backend already holds instead of paying fresh prefill for tokens
it has already seen.

Run it **alone**: it changes the message count, and `apply`'s count-change rebuild re-emits each
retained message's original raw bytes by matching them exactly, so a message another component
edited in place fails to match and is re-marshalled instead.

## Why the call shape matters

`summarize` builds a fresh prompt — a preamble plus the rendered transcript as one user message.
That prompt shares no prefix with the conversation it describes, so the call is a cache miss on
every token.

This component sends **`[the conversation, verbatim and in order] + [one appended instruction]`**.
The rendered prefix is the one the agent's own request produced, so a prefix-caching backend can
charge prefill only on the appended suffix.

## ⛔ How far the reuse actually goes

The prefix that gets hit is one the **backend** has seen, and the backend only ever receives what
this proxy forwards. The agent keeps sending its full uncompacted history, but from the first
triggering turn onward the backend has received **compacted** requests — so there is no
full-history prefix upstream to match, and the shared prefix is roughly the pinned head.

Two consequences, both worth measuring rather than assuming:

- the saving is largest on the **first** compaction and smaller afterwards;
- sending the full growing history as a side call can **evict** the compacted prefix the forwarded
  request needs.

Judge this component on the backend's own telemetry — `vllm:prefix_cache_hits_total`, or
`usage.cache_read_input_tokens`, which the OpenAI client records — never on a savings percentage.

## Where the instruction goes, and why it is per model

The appended instruction is an **operator** instruction, so `role: system` is the correct channel
where it exists: it is non-spoofable, and a trailing user turn on a long trajectory reads to the
model as more trajectory and gets summarized instead of followed.

But a system message at the **end** of the messages array is not universally accepted, and the
constraint lives in two different places — on Anthropic the **server** decides, on vLLM the model's
**chat template** decides (vLLM validates only that `role` is a string).

The two failure modes are not equally visible:

| | |
|---|---|
| **loud** | the provider returns `400 role 'system' is not supported on this model` |
| **silent** | a template **drops** or **hoists** the message. The model continues the task, and its next turn is recorded as the summary. |

`instruction_role: auto` resolves per model from
[`summarizer_model_profiles.yaml`](https://github.com/rossoctl/context-guru/blob/main/components/offload/summarizer_model_profiles.yaml).
That file has two parts: `system_models`, an allow-list of match strings that take the instruction as
`role: system`, and `profiles`, a provenance record that grants nothing and only says what was
established about each model. A pinned `instruction_role: system` for a model the allow-list does not
name **declines** at request time and increments `cache_aware_summarizer_unverified_system`, rather
than risking the silent case.

**`system_models` is empty, so every model resolves to `user` today.** The reason is that a profile
described a *model*, while what has to accept a trailing system message is the whole *path* to it.
The component requires a `components.MessagesModel` and only the OpenAI-shaped client implements one,
so every reachable deployment sends OpenAI-shaped requests — and a gateway translating those to a
native provider API may lift a mid-array system message into that API's top-level `system` field.
Measured against a LiteLLM gateway fronting Bedrock: `aws/claude-opus-5` with a trailing `role: system`
returned `400 … the conversation must end with a user message` on every call (the instruction had been
removed from `messages[]`), while the same request with `role: user` returned 200. End to end, the
`claude-opus-5` arm compacted nothing and reported only `cache_aware_summarizer_errors`; the
`claude-sonnet-5` arm, which resolved to `user`, cut 2228 tokens to 876.

To promote a model, verify the **path** — probe through the same endpoint and credential production
uses and confirm the instruction arrives last — then add its match string to `system_models`, or ship
a `profiles_path` override so the promotion needs no rebuild.

## The call is detached, so compaction takes two turns

A summary here covers most of the transcript, so the call is large by construction and its budget is
300 s. Running that inline would stall the triggering turn by minutes, billed against the agent's own
timeout — `summarize_async.go` exists because that was already found too expensive on the request
path.

So the control flow is the same two-turn shape `summarize` uses: **the turn that triggers commissions
a summary and forwards untouched; the next eligible turn finds the checkpoint and splices.** The
detached goroutine reuses the shared flight registry and the global concurrency bound, so a saturated
proxy sheds compaction (`cache_aware_summarizer_async_refused`) rather than queueing a call nobody is
waiting for.

Read the `async_started` / `async_committed` **pair**: started without committed is a summary that was
paid for and lost, which no other counter would reveal.

## Two gates, two different quantities

`min_tokens` asks *is the span worth a call*. `max_request_tokens` asks *can we afford the call* — and
they differ because the request carries the **whole conversation**, not the span.

`max_request_tokens` is a **refusal, never a truncation**. Truncating the conversation to fit is not
available: the appended-suffix shape is the entire mechanism, and a truncated conversation is a
different prefix that matches nothing. An over-large session declines and says so
(`cache_aware_summarizer_too_large`).

## Reversibility and reuse

`marker_mode: full` (default) stashes the replaced span through `commitMark`, so a store that
**refuses** the payload causes the compaction to be skipped rather than leaving a `<<cg:HASH>>`
marker pointing at nothing. `StashRoom` is checked before the model call, so a summary is not paid
for and then thrown away.

`resummarize_tokens` (default 6000) keeps a checkpoint: the spliced summary is re-emitted
**byte-identically** on later turns until the untouched tail passes that threshold. Without it the
forwarded prefix would change at the head on every turn and the component would invalidate the
cache it exists to protect. The checkpoint namespace is shared with `summarize` — safe because both
must run alone, and `CoveredHash` rejects a checkpoint whose covered prefix has changed.

The model's reply is treated as **untrusted**: `sanitizeSummary` strips forged expand markers (both
spellings), the summary sentinel and a premature `</summary>` before the text is framed as
trustworthy earlier context. The whole trajectory reaches the summarizer, so planted text in any
tool output gets a long run at it.

## Counters at `/stats`

| field | means |
|---|---|
| `cache_aware_summarizer_calls` | summarization passes paid for; this method's cost is per compacted turn |
| `cache_aware_summarizer_declined` | ⭐ no `MessagesModel` was available. **A declining arm compacts nothing and is byte-identical to `off` on every other metric** — read this before believing any delta |
| `cache_aware_summarizer_empty` | a call was paid for and returned nothing usable — the signature of a dropped or hoisted instruction |
| `cache_aware_summarizer_unverified_system` | declined a pinned `system` role for a model no profile verifies |
| `cache_aware_summarizer_refused_stash` | the store would not accept the span, so the compaction was skipped |
| `cache_aware_summarizer_profile_fallbacks` | `profiles_path` was unreadable, so the embedded registry was used |
| `cache_aware_summarizer_too_large` | declined because the outbound request would exceed `max_request_tokens` |
| `cache_aware_summarizer_async_started` / `_committed` | the **pair** is the signal — started without committed is a summary paid for and lost |
| `cache_aware_summarizer_timeouts` / `_errors` | fail-open paths; a timeout means the budget is too small for this load, an error means the route is wrong |

## Not yet measured

The prefix-cache-hit improvement this design predicts is **unverified** — there is no live run
behind it yet. The counters and the backend telemetry above are how to establish it.
