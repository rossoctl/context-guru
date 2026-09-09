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

### Why reuse is load-bearing rather than an optimisation

The trigger gates the component completely — below it `summarize` returns immediately and does nothing.
**And the trigger reads the request as the REST OF THE PIPELINE LEFT IT**, because one `req` pointer is
threaded through the components in order, each mutating it in place, and `summarize` runs last. So
upstream compaction decides whether this component fires at all: the system is self-limiting, and
measured it fires on 0–28% of requests (13.5% and 27.9% on iteration 025's two arms, 25.8% and 0% on the
iteration 026 probes) rather than on every turn.

That is also the deferral mechanism, stated without hand-waving: anything upstream that shrinks the
post-pipeline request below the threshold stops `summarize` running, and `acted / runs` against a
baseline arm is how you see it.

On a turn where it does fire, one of two paths is taken:

| path | when | cost |
|---|---|---|
| **reuse** | tail since the checkpoint < `resummarize_tokens` | **free** — no model call, summary byte-identical, cached prefix survives |
| **roll forward** | tail ≥ `resummarize_tokens` | a model call **plus a prefix rewrite from the summary onward**, i.e. a cache write |

`resummarize_tokens` is therefore a **refresh interval**, not a size limit: it is how much new content the
component will carry verbatim before paying to re-summarise. Larger means fewer model calls and fewer
cache writes, paid for by a bigger request each turn; smaller means a tighter request and a cache write
more often. Zero disables reuse, so every eligible turn re-summarises.

Read that against a cache write costing roughly 11.5x a cache read per token in this codebase's own
arithmetic and reuse is worth real money on the turns it applies — though at a 14–28% firing rate it is an
optimisation rather than, as an earlier draft of this section claimed, the only thing making per-turn
operation viable. Operation is not per-turn.

### The interaction to watch: an upstream component can defeat it

Reuse requires the covered span to be **byte-unchanged**, and the hash is taken over the messages *as
earlier components in the pipeline left them*. So any component that mutates a message inside the
checkpointed span invalidates the checkpoint and forces the paid path, even when the tail is small.

`extract_llm_sweep` is the case to watch, since it removes deep-history outputs and runs before
`summarize`. Note what it cannot do: because the trigger gates everything, a removal can never *cause* a
summarize run — it can only convert a free reuse into a paid refresh on a turn that was going to run
anyway. Whether the two spans overlap in practice is unmeasured: the sweep targets deep history while the
checkpoint boundary sits nearer the tail, so they may rarely collide.

**And that cannot currently be checked from a run.** The reuse path records no gate and no event, and
`rep.Replays` is never incremented, so `acted_replay` reads 0 for this component whether reuse fired on
every turn or none — see the open issue on summarize's uncounted reuse path. A zero there means
*unmeasured*, not *never*.

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
| `trigger` | — | Gates the first summary: `min_request_tokens`, `min_messages`, `min_output_tokens`, and the window fractions `min_request_frac`, `min_output_frac`, `huge_output_frac`. |
| `marker_mode` | `full` | `full` (stash + resolvable marker) / `summary` / `off`. |

## When it shines

Long agentic sessions where the bulk is stale middle context.

## When it's inert

Transcript below `trigger`, span below `min_tokens`, or no model available (no-op).

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
