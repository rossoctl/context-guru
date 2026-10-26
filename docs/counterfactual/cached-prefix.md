# What changes the cached prefix

**Status:** proposed.
**Date:** 2026-10-25.
**Part of:** [`design.md`](design.md), §0. The rule and section numbers below (R45, R50, §5.2)
refer to that document.

A provider reuses a cache entry only when the rendered prefix of a new request is the same as the
prefix of the entry, up to a cache breakpoint. A change before the breakpoint breaks the match from
the first changed token onward. The change can be in the content, or in a setting that the provider
renders into the prefix. The settings are different for each provider, so this document is a list that
grows. Each provider is added as it is read. The model keeps these changes as a `setting` context
operation (R50) when a context operator makes them, and as a client edit when the client makes them (§5.2).
`design.md` §4.2 maps the settings below to the position (`At`) where the provider stops matching.

**OpenAI** ([prompt caching guide](https://developers.openai.com/api/docs/guides/prompt-caching#which-settings-affect-the-cached-prefix),
read 2026-10-25). A change in any of these may change the rendered prefix. The guide says a change does
not always discard an entry, because what matters is that a later request has the same prefix and
reaches an eligible breakpoint.

| Change | Why it can break the cache |
|---|---|
| `model` | A different model can use different weights and different caching behaviour. |
| `tools` | Names, descriptions, schemas, order or tool-specific instructions change. |
| `parallel_tool_calls` | It can change the guidance about calling several tools in one turn. |
| `text.format` (Structured Outputs) | It adds output-format instructions and the requested schema. |
| `reasoning.effort` | It can change the instructions for reasoning. On supported models a configuration update changes the effort and keeps the earlier prefix. |
| `text.verbosity` | It can change the instructions for the detail of the answer. |
| `context_management` (compaction) | It replaces earlier conversation with compacted content, so reuse stops at the first changed token. |

Other OpenAI facts that the model depends on:

- **Lifetime is a minimum, not an expiry.** From GPT-5.6, `prompt_cache_options.ttl` is `30m` (default) or
  `12h`, and both are minimum lifetimes. Earlier models use `prompt_cache_retention`, `in_memory` or `24h`,
  with typical lifetimes of 5 to 10 minutes and about 30 minutes, and no documented maximum. This is why
  a gap past the TTL gives `unknown` and not `cold` (R45, §5.2).
- **Writes have a price from GPT-5.6.** A read costs 0.1 times the uncached input rate, a write costs
  1.25 times for 30 minutes and 2.5 times for 12 hours. These are total rates and not surcharges.
  Earlier models have a cached-input rate and no write charge.
- **Reporting.** `usage.input_tokens_details.cached_tokens` counts billed reads, and `cache_write_tokens`
  counts writes, split by lifetime.
- **Granularity.** From GPT-5.6 the cached count is exact at the eligible boundary. On GPT-5.5 and
  earlier it is rounded down to a multiple of 128, and the minimum prefix depends on the request.
- **Routing.** `prompt_cache_key` affects routing and, from GPT-5.6, separate accounting for each
  user. It does not guarantee a hit.

**Anthropic** ([prompt caching guide](https://platform.claude.com/docs/en/build-with-claude/prompt-caching),
read 2026-10-25). The cache has three levels in this order: `tools`, then `system`, then `messages`. A
change at one level breaks that level and every later level. In the table, ✘ means broken and ✓ means
still valid.

| Change | tools | system | messages | Note |
|---|---|---|---|---|
| Tool definitions (names, descriptions, parameters) | ✘ | ✘ | ✘ | The whole cache |
| Web search toggle | ✓ | ✘ | ✘ | It changes the system prompt |
| Citations toggle | ✓ | ✘ | ✘ | It changes the system prompt |
| `speed` (`fast` or standard) | ✓ | ✘ | ✘ | |
| `tool_choice` | ✓ | ✓ | ✘ | Only the message blocks |
| Images, added or removed anywhere | ✓ | ✓ | ✘ | |
| Thinking parameters (mode, `budget_tokens`) | model-specific | model-specific | ✘ | Tools and system break too, on models that render the configuration ahead of them |
| `output_config.effort` | model-specific | model-specific | ✘ | The model default is the same as omitting it, so nothing breaks. On models with a per-message effort, a change in a `role: "system"` message inside `messages` keeps the prefix |
| Non-tool-result user content, in a request with thinking | ✓ | ✓ | model-specific | Newer models keep the thinking blocks, so the cache stays valid. Older ones strip them, which breaks `messages` |
| Dropped thinking blocks, when not preserved | ✓ | ✓ | ✘ | The prefix changes from that block onward |

Other Anthropic facts that the model depends on:

- **A hit needs identical content up to and including the `cache_control` block.** An unstable key order
  in a `tool_use` block (Swift and Go randomise it) breaks the cache.
- **Two ways to change a conversation without a break:** append a `{"role": "system"}` message to
  `messages` (not on every model), or add tools with a `tool_addition` block (a beta).
- **The lifetime is exact.** It is 5 minutes, or 1 hour with `ttl: "1h"`. It is measured from the start
  of the request that wrote or read the entry. A 1-hour write costs 2 times the base input rate.
  Longer lifetimes must come before shorter ones.
- **The lookback window is 20 blocks.** For each breakpoint the system checks at most 20 positions,
  counting the breakpoint itself as the first. If it finds no matching entry in that window, checking
  stops, or resumes from the next explicit breakpoint, if any. On the Claude API, a run of consecutive
  `tool_use` blocks counts as one position, and so does a run of consecutive `tool_result` blocks, so a
  turn with many parallel tool calls does not push the previous request's entry out of the window on
  its own. The prefix can be unchanged and still miss, when the previous entry is more than 20
  positions behind the breakpoint.
- **A minimum length applies.** It is 512, 1,024, 2,048 or 4,096 tokens, depending on the model. A shorter
  prompt is processed without caching and returns no error.
- **Isolation.** A cache is private to a workspace on the Claude API, and to an organization on Bedrock
  and on Google Cloud.

The page does not list a change of `model` in its table. The provider keys a cache to the exact prompt
prefix. The model is part of the request, so a change of model is treated as a break in this design,
which §5.2 row 3 already does. This is an inference, not a statement from the page.

**Other providers.** Not yet read.
