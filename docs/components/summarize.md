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

## Configuration

| Key | Default | Meaning |
|---|---|---|
| `summary_level` | `regular` | `concise` \| `regular` \| `highly_detailed`. |
| `keep_last` | 3 | Trailing messages kept verbatim. |
| `min_tokens` | 500 | Span floor — minimum middle size before summarizing. |
| `include_tool_calls` | `false` | `false` → tool outputs masked in the summarized trajectory. |
| `resummarize_tokens` | 6000 | Tail growth that triggers rolling the checkpoint forward. |
| `model.source` | `incoming` | LLM source: `incoming` (proxied model+key) or `config` (cheap model). |

!!! danger "This component could not send a valid Anthropic request until 2026-08-21"
    `summarize` had **three independent message-shape defects**, each masking the next, all found by
    running [LOCA-bench](../experiments/loca/iter005/results.md) against a real API — never by tests
    or by replay, because `/compact` returns the rewritten body **without forwarding upstream**, so
    no provider ever validated it.

    | defect | provider response | fixed by |
    |---|---|---|
    | summary emitted as a **`system`-role** message at index 1 | `400 messages.1: role 'system' must precede an 'assistant' message or end the array` | emit it as a **user** message, as Claude Code's own compaction does |
    | `tool_result` blocks kept whose `tool_use` was deleted | `400 …unexpected tool_use_id found in tool_result blocks` | `dropOrphanedToolResults` |
    | `tool_use` blocks kept whose `tool_result` was deleted | `400 …tool_use ids were found without tool_result blocks immediately after` | **`summarizeSpan`** — see below |

    The last two are the same mistake from either side, and both came from boundaries chosen by
    arithmetic (`msgs[1 : len−keepLast]`) that knew nothing about tool pairing. The root fix is one
    rule: **a tool exchange is atomic.** `summarizeSpan` advances `end` past any tool messages the
    kept tail would begin with, so an exchange is summarized whole rather than split; and it drops
    the preserved head when `msgs[0]` is an assistant tool-call message, because that message's
    results necessarily lie inside the span and the head exists to carry the conversation's
    *identity* (its system prompt or opening user turn), which a tool call is not.

    Review framed it best: an unanswered `tool_use` means the agent is still **waiting** on that
    tool, so summarize *after* the exchange completes, not through it. That needs no synthetic
    `[tool result unavailable]` placeholder — which would be a second fiction on top of the summary.

    Guarded by `schema.ValidateShape` plus an all-presets test that fails on the pre-fix code in
    0.07s. **Any component that deletes messages needs this invariant**; `coref` does not, because it
    rewrites a tool message's text in place and never removes a message.

!!! warning "Run it alone"
    `summarize` restructures the transcript and changes the message count, so another component's
    in-place edits can race `apply`'s rebuild. Two experiment iterations
    ([004](../experiments/loca/iter004/results.md), 005) were invalidated by ignoring this. If a
    pipeline needs both compaction and summarization, chain **two proxies** — a compaction pipeline
    in front of a summarize-only one — which is also the deployable shape.
| `trigger` | — | Gates the first summary: `min_request_tokens`, `min_messages`. |

## When it shines

Long agentic sessions where the bulk is stale middle context.

## When it's inert

Transcript below `trigger`, span below `min_tokens`, or no model available (no-op).

See also: [Components overview](../components.md) · [Choose a preset](../how-to/choose-a-preset.md)
