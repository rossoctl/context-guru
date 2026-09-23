# Components

Every registered component, exactly as it behaves in code. All operate on tool-output
messages (`role:"tool"`; for Anthropic, `tool_result` blocks normalized to that shape by
`apply`). **Reformat** = lossless. **Offload** = drops bytes, stashes the original, leaves a
`<<cg:HASH>>` marker recoverable via `context_guru_expand` / `GET /expand`.

## Summary

| Component | Kind | What it drops | Recoverable | Fires on | Key config (default) |
|---|---|---|---|---|---|
| [`format`](components/reformat.md#format) | Reformat | nothing (compacts JSON) | n/a (lossless) | pretty-printed JSON tool output | `min_tokens` (50) |
| [`toon`](components/reformat.md#toon) | Reformat | nothing (re-encodes JSON arrays as TOON) | n/a (lossless) | uniform flat JSON object-arrays; **opt-in, RETIRED from every preset 2026-08** — 0 acts in 5,752 production requests, 0 convertible candidates in 11.67M measured tokens, at 1.53 ms + a `TextTokens` call per tool message | `min_tokens` (50) |
| [`textclean`](components/reformat.md#textclean) | Reformat | nothing (strips ANSI + `\r` redraws) | n/a (lossless) | plain-text tool output with terminal control | `min_tokens` (50) |
| [`searchfold`](components/reformat.md#searchfold) | Reformat | nothing (folds the repeated path prefix out of search output) | n/a (lossless, exact inverse) | any tool output with a repeated path prefix — routed by CONTENT, attempted everywhere, kept only when its own inverse round-trips; **in every preset** since 2026-08 | `min_tokens` (50) |
| [`toolschema`](components/reformat.md#toolschema) | Reformat | nothing (strips JSON-Schema annotation keywords from `tools`) | n/a (lossless) | any request carrying tool schemas; **opt-in, in no preset** — it re-anchors the cached prefix once, see the break-even | — (no config) |
| [`toolfilter`](components/reformat.md#toolfilter) | Reformat | the tool/MCP declarations the account listed in `remove` (82.7% of a session's declared tokens are never invoked) | yes — delete the name from `remove` (re-anchors the prefix once) | any request carrying declarations; **opt-in, in no preset** — it never removes a name the account did not list, and keeps any name still described in the system prompt's prose | `remove` (empty) |
| [`cacheinject`](components/caching.md#cacheinject) | Reformat | nothing (adds `cache_control`) | n/a (lossless) | Anthropic-family requests; **opt-in, in no preset** — placement is unmeasured | `ttl` (5m) |
| [`cachesplit`](components/caching.md#cachesplit) | Reformat | nothing (splits a `system` block) | n/a (lossless) | Anthropic-family requests; **in every caching preset** — enables the measured volatile-tail split | — (no config) |
| [`skeleton`](components/skeleton.md) | Offload | function/method bodies | via expand | fenced ` ```lang ` code blocks and raw file dumps (`Read`/`cat`/`sed -n`); **behind the `cg_skeleton` build tag, needs CGO, in no preset, and must not be enabled** — it removes 0 tokens per live turn because every `Read` is the newest of its path, and the guards that make it safe are what make it worthless, see the measurement | `min_tokens` (80) |
| [`dedup`](components/offload-reducers.md#dedup) | Offload | later byte-identical tool outputs | via expand | repeated identical outputs | `min_tokens` (100) |
| [`readlifecycle`](components/offload-reducers.md#readlifecycle) | Offload | file `Read` bodies the transcript proves are stale (file edited later) or superseded (re-read later) | via expand | `Read` results in an editing session; **opt-in, in no preset** — 0 tokens warm, a third of a *cold* request, see the break-even | `min_tokens` (100), `stale`, `superseded`, `bash_edits`, `stale_at_depth`, `cold_cache` |
| [`collapse`](components/offload-reducers.md#collapse) | Offload | middle of an oversized output (by lines, or by characters when there are too few lines) | via expand | any large tool output (fallback) | `max_tokens` (2000), `head_lines` (20), `tail_lines` (20), `cold_cache` (**true**) |
| [`failed_run`](components/offload-reducers.md#failed_run) | Offload | earlier superseded test/build runs | via expand | ≥2 run-like outputs | `min_tokens` (100), `cold_cache` (**true**) |
| [`cmdfilter`](components/cmdfilter.md) | Offload | lines per declarative DSL filter | via expand | output matching a filter | `filters` ([]), `disable_builtins` (false), `min_size` (400) |
| [`linecap`](components/offload-reducers.md#linecap) | Offload | the tail of any line over 500 chars, and every NON-adjacent repeat of a line (first copy annotated `(xN)`) | via expand | any tool output ≥ `min_size`; command-agnostic, so it fires where per-command filters do not | `max_line_chars` (500), `collapse_duplicate_lines` (true), `min_size` (400) |
| [`extract`](components/offload-reducers.md#extract) | Offload | obvious noise (repeated lines/blocks, blank runs, progress bars) | via expand | any large output | `min_tokens` (300), `trigger` |
| [`extract_llm`](components/extract_llm.md) | Offload (LLM) | query-irrelevant content via an LLM-written sandboxed filter | via expand | large output in a large request | `strategy` (code), `model.source`, `trigger`, `rewrite`, `skip_file_reads` |
| [`smartcrush`](components/offload-reducers.md#smartcrush) | Offload | middle items of a JSON array | via expand | JSON-array tool output | `min_items` (5), `min_tokens` (200), `keep_first` (3), `keep_last` (2) |
| [`mask`](components/offload-reducers.md#mask) | Offload | older tool outputs (age-based) | via expand | more than `keep_recent` outputs | `keep_recent` (3), `min_tokens` (100), `keep_head_chars` (96), `cold_cache` (**true**) |
| [`summarize`](components/summarize.md) | Offload (LLM) | the middle of the transcript → one summary | via expand | long trajectories | `summary_level` (regular), `keep_last` (3), `min_tokens` (500), `resummarize_tokens` (6000), `model.source`, `trigger` |
| [`cache_aware_summarizer`](components/advanced-offload.md#cache_aware_summarizer) | Offload (LLM) | the middle of the transcript → one summary, but the summarizer CALL reuses the conversation's own prefix instead of rebuilding a prompt | via expand | long trajectories on a prefix-caching backend; **needs a `MessagesModel`** and declines without one | `keep_last_turns` (10), `instruction_role` (auto), `model_id`, `min_tokens` (500), `resummarize_tokens` (6000), `profiles_path`, `model.source`, `trigger` |
| [`agentdiet`](components/advanced-offload.md#agentdiet) | Offload (LLM) | useless/redundant/**expired** content in the step that just aged past the delay | via expand | a step above `min_step_tokens`, `delay_steps` turns back | `delay_steps` (2), `context_steps` (1), `min_step_tokens` (500), `min_saved_tokens` (400), `max_keep_ratio` (0.8), `model.source` |

Presets (`config/config.go`), verbatim: **`house`** (the proxy default), **`codesmart`** (the SWE-bench arm)
`[format, textclean, searchfold, dedup, failed_run, cmdfilter, extract_llm, extract, linecap, cachesplit]` ·
**`codesafe`**
`[format, textclean, searchfold, dedup, failed_run, cmdfilter, extract, collapse, linecap, cachesplit]`
(deterministic-only) · `off` `[]` · `safe` `[format, textclean, searchfold, cachesplit]` · `balanced`
`[format, textclean, searchfold, dedup, failed_run, cmdfilter, linecap, cachesplit]` · `aggressive`
`[format, textclean, searchfold, dedup, failed_run, cmdfilter, smartcrush, extract, extract_llm, linecap, cachesplit]` ·
`coding` `[format, textclean, searchfold, dedup, cmdfilter, extract, linecap, cachesplit]` ·
`mcp` `[format, textclean, smartcrush, cachesplit]` ·
**`agent`** `[format, textclean, searchfold, dedup, failed_run, mask, extract, extract_llm, cachesplit]` —
for long agentic sessions; `mask` is the biggest lever there (~27–30% content-token savings, no reward
loss — see [RESULTS.md](RESULTS.md)) ·
**`general`**
`[format, textclean, searchfold, dedup, failed_run, cmdfilter, mask, extract, extract_llm, collapse, linecap, cachesplit]`
— the recommended all-round pipeline: the reward-neutral levers of `agent` plus the situational
shrinkers (`cmdfilter`/`linecap`/`collapse`) that cost nothing when they don't fire. `balanced` is
**not** recommended for agentic traffic — it omits `mask`, so it barely helps (6% vs 31% in the
Terminal-Bench replay) ·
`summarize` `[summarize]` (run alone — it restructures the whole transcript) ·
`agentdiet` `[format, agentdiet, cachesplit]` (a published-method **baseline** for A/B, run without
our own offloaders so its effect is attributable — see [agentdiet](components/advanced-offload.md)).

**The lossless trio leads every working preset** (`mcp` takes only `format` + `textclean` — it
serves JSON list endpoints with no search output to fold). `format`, `textclean` and `searchfold` all
verify-then-adopt, so there is no risk argument for omitting one, and running them first makes every
downstream token count honest. Two of the three used to be missing: `textclean` shipped in `general`
alone while 49.6% of corpus messages carry ANSI, and `searchfold` shipped in NO preset at all.
`toon` is retired from all of them — it acted 0 times on 5,752 production requests.

**Cold turns sweep at depth.** `mask`, `failed_run` and `collapse` default to `cold_cache: true`:
on a turn whose prompt cache has provably expired there is no cached prefix to protect, and
production was freezing 90.8% of the context (38.4M of 42.3M tokens across 742 `ttl_expiry`
requests) to protect a cache that was already gone — on exactly the turns where a removed token is
worth ~12.5x a warm-turn one. Set `cold_cache: false` per component to restore the tail restriction.
It is **not** defaulted on for `readlifecycle` or `skeleton`, whose replacements are not pure
functions of (content, config).

Every preset that touches caching carries `cachesplit`, never `cacheinject` — see
[Presets](reference/presets.md).

**Dynamic, model-aware triggers.** Trigger thresholds can be expressed as **fractions of the model's
context window** (resolved dynamically via LiteLLM's public model map, no hand-maintained list):
`min_request_frac`, `min_output_frac`, and a hard `huge_output_frac` ("huge tool call" — act regardless of
the request-level gate). `collapse.max_frac` scales its size budget likewise.
When the window is unknown, fractions are ignored and absolutes apply (backward compatible). This lets
one config generalize across models/benchmarks.

!!! note "`min_request_frac` and `min_request_tokens` are measured differently, and are ANDed"
    A context window is stated in the tokens the **provider** bills — messages plus the system
    prompt, the tool declarations and the JSON envelope. `min_request_frac` is therefore compared
    against the provider's own reported input count for the session's previous turn.
    `min_request_tokens` is compared against **message text only**, which is what this proxy's own
    tokenizer can see, and on measured traffic runs a median **3.4x smaller** than the billed
    figure. Because the two are on different scales they are separate conditions, both of which
    must be met — taking the larger of them would be comparing unlike numbers. Set one or the
    other unless you mean both.

    The per-item fractions (`min_output_frac`, `huge_output_frac`) are unaffected: they size a
    single tool output, where the same tokenizer is on both sides of the comparison.

**Reversibility in practice.** The `context_guru_expand` tool is advertised on outgoing requests
(`INJECT_EXPAND=auto|always|never`, default `auto` = whenever the request already declares tools,
the store persists, **and the pipeline contains at least one Offload**), so Offload markers are
genuinely recoverable — not just described in marker text. That third condition is what keeps the
tool off pipelines that mint no markers, where every call to it would have to fail.
All three conditions are properties of the **session**, not of the turn, so the `tools` array a
session sends is byte-identical on every request in it. That matters more than it looks: `tools` sits ahead of
`system` and `messages` in the provider's prompt-cache hash, so the first request carrying a **new**
tools array re-creates the **entire** prefix at the write rate. `auto` used to also require a marker on
the request, which made the array grow on the first offloading turn and shrink again on the next turn
that carried none.

Measured on a real 7-turn session: the old behaviour flipped once, when the first marker appeared, and
that turn billed **write 43,359 / read 0** against the 40,320-token prefix the earlier turns had built.
After the change, one tools hash across all seven turns and that turn billed write 163 / read 43,135.
Paired sessions run in both arm orders: **35.4% and 35.3%** off input cost, with total input tokens
*up* 313 — the same bytes, repriced from 1.25× to 0.1×. Worth knowing the shape of the win: alternating
back to a previous tools array still **reads**, because the provider keeps both lineages alive within
the TTL, so the cost is one full-prefix write per *new* variant rather than one per flip. A session that
never offloads saves nothing.

Every
offloader also applies a **marker-inclusive** never-worse check per message, so a rewrite never grows a
message by the marker's tokens.

**LLM-based components** (`extract_llm`, `summarize`, `agentdiet`) call a model, chosen by
`model.source`: `incoming` (default — reuse the proxied request's own model + key) or `config` (a dedicated
cheap model set via `CHEAP_MODEL*` env / the gateway's `CheapModel`). When no model is available they
degrade — `extract_llm` to a no-op (the deterministic `extract` beside it in every preset does the
cheap pass), `summarize` to a no-op, `agentdiet` to replaying only what it already froze.
`extract` itself never calls a model. See
[design.md](design.md#llm-components).

Common gates every Offload respects: skip non-text (`Rewritable`) messages, skip content already
carrying a marker (no double-offload), and skip if the rewrite (marker + hint included) isn't
actually smaller.

---

## Reformat (lossless)

Six components repack a tool output or the request envelope without dropping anything. Full
detail on each, with before/after examples and config, lives on its own page:
[Reformat components](components/reformat.md) (format, toon, textclean, searchfold, toolschema,
toolfilter) and [Prompt caching](components/caching.md) (cacheinject, cachesplit).

## Offload (lossy, reversible)

Every Offload component stashes what it removes behind a `<<cg:HASH>>` marker, recoverable via
`context_guru_expand` / `GET /expand`. Full detail on each, with before/after examples, config
and measurements, lives on its own page:

- [Offload: deterministic reducers](components/offload-reducers.md) — dedup, failed_run, extract,
  linecap, smartcrush, mask, collapse, readlifecycle
- [`cmdfilter`](components/cmdfilter.md) — declarative DSL filters
- [`extract_llm`](components/extract_llm.md) — the LLM-based counterpart to `extract`
- [`skeleton`](components/skeleton.md) — function/method bodies → signatures (needs the
  `cg_skeleton` build tag; not recommended, see the page)
- [`summarize`](components/summarize.md) — the middle of the trajectory → one LLM-written summary
- [`coref`](components/coref.md) — co-reference-aware pronoun/reference collapsing
- [Advanced & experimental Offload components](components/advanced-offload.md) —
  `cache_aware_summarizer`, `agentdiet`, `extract_llm_sweep`

## The DSL filter engine

`cmdfilter`'s declarative, user-extensible YAML filter language (adapted from rtk —
Apache-2.0, see `THIRD-PARTY-NOTICES`) is documented on its own page, including the filter
schema, load-time validation, and how to write a custom filter:
[The DSL filter engine](components/dsl.md).
