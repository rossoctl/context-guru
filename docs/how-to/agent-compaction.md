# When your agent compacts

Claude Code and Bob both summarize their own conversation when it grows too long for the
context window. context-guru is designed to make that happen later, and to stay correct
when it does happen.

## What context-guru does about it

**It pushes compaction further out.** Both agents decide when to compact from the token
usage the provider reports for the request that was actually sent. context-guru sends a
smaller request, so the reported usage is smaller and the threshold arrives later. Usage
figures themselves are never touched — the response is replayed to the agent byte for
byte, so `/cost`, the statusline and your provider bill all still agree.

**It treats the compaction request like any other request.** The pipeline runs on it, so the
request keeps the same tool list and the same rewritten history as the turns before it. This
keeps the provider cache warm. Measured on production traffic, a request that bypassed the
pipeline missed the cache 31% of the time. A request that did not bypass missed 1.6% of the time.
The trade-off: the summary model sees the rewritten history (shortened tool outputs and
`<<cg:HASH>>` markers), not the full original. Detection is a match on the opening of the
agent's own summarization prompt in the last message of the request, and it covers Claude Code
and Bob Shell (`proxy/agentcompaction.go`). Detection only feeds thread identification, the
larger size limit below, the dashboard row, and the `agent_compaction` counter in `/stats`. To
skip the pipeline for one request, send `x-context-guru-bypass: true`. A compaction body over
32 MiB is bypassed without the header, because the pipeline costs about 0.35 s per MiB.

**The summary request carries the expand tool, and two cases can leak a tool call.** The request
keeps the same tool list as the turns before it, so the cache stays warm. The prompt tells the
model to call no tool. If the model calls `context_guru_expand` anyway:

- Claude Code uses the Anthropic dialect. The proxy answers the call and the client gets the
  summary text, for JSON and for streaming.
- **OpenAI dialect with `stream:true`:** the proxy does not inspect that stream. The client
  receives the raw tool call and no summary.
- **Any dialect:** if the model calls expand on every round, the client receives the raw tool call
  after 3 continuation rounds, with no summary. That needs the model to ignore the "Do NOT call any
  tools" instruction 4 times in a row.

`proxy/compactionexpand_test.go` pins all of these.

**It also lets that request through when it's the one thing that's already too large.** A
session whose transcript grew past the [request-size ceiling](../reference/reference.md#request-size-ceiling-32-mib-by-default-128-mib-for-compaction)
(most often bytes diverging from tokens on highly repetitive tool/shell output — a
compact-and-cheap-in-tokens transcript can still be large in raw bytes) can no longer send an
ordinary turn, but its own compaction request is exempt up to a higher ceiling, so `/compact`
still has a way to shrink the session back down instead of leaving it permanently stuck. See
[rossoctl/context-guru#278](https://github.com/rossoctl/context-guru/issues/278) for the full
story.

**A compaction does not split your session.** The session id comes from a stable value the
agent already sends (`x-context-guru-session`, then Claude Code's `metadata.user_id`, then
Bob's `metadata.taskId`), so one conversation stays one session in the dashboard and keeps
its dedup memory, frozen offload decisions and cache boundary across the compaction.

## Can you turn agent compaction off?

| Agent | Answer |
|---|---|
| **Bob** | Yes. Set `model.compressionThreshold` in `~/.bob/settings.json` (restart required) — `0.9` compacts at 90% instead of the default 50%, a large value disables it in practice. `/compress` still forces it on demand. |
| **Claude Code** | No. Every threshold knob is clamped to at most the model's window, so it can only compact *earlier*. `DISABLE_AUTO_COMPACT` turns it off entirely, but then a long session walks into the provider's hard context limit with no summary to fall back on. |

## Bob: prefer lossless components

Bob cannot expand a `<<cg:HASH>>` marker — it has no way to call the expand tool (see
[Integrations](../integrations.md#use-it-with-an-agent-bob-bobshell)). Any lossy component
therefore leaves content permanently unrecoverable in a Bob session. Use lossless
components (`format`, `toon`, `cachesplit`) with Bob, and add lossy ones only where losing
that content is acceptable.

<details markdown="1">
<summary>Troubleshooting</summary>

**My savings look tiny early in the session.** context-guru only reduces `messages`. The
system prompt and tool schemas are the bulk of a fresh Claude Code request (roughly 55k of
64k billed input tokens on a measured session), and they are not ours to touch. As the
transcript grows it comes to dominate the request, so the reduction applies to almost all
of it by the time the compaction threshold is in sight.

**Compaction still fires at the same point.** The delay depends on the upstream reporting
usage for the *reduced* request. Any OpenAI- or Anthropic-compatible backend does; a
gateway that pre-counts or re-expands the body before billing would not. Compare the
`input_tokens` in the response against what your agent sent to check.

**The `agent_compaction` count is too high.** The compaction-detection phrase can appear in
ordinary content — for example if the agent reads a file that quotes it. The request is still
handled like any other. The only effects are one extra count and, for an oversized body, the
higher size limit.

**A conversation appears twice in the dashboard.** Send an explicit
`x-context-guru-session` header. Without any agent-supplied id the fallback is a hash of
the system prompt plus the first user message, and both of those change when the agent
compacts.

**Bob's numbers jump for one turn after a compaction.** Bob seeds its own token estimate
locally right after building a new chat, and that estimate is in force until the next
response arrives. It corrects itself on the following turn.

</details>

See also: [Reversibility & recovery](recover-context.md) ·
[Measuring savings](measure-savings.md)
